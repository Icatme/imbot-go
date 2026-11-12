package wecom

import (
 "context"
 "encoding/json"
 "errors"
 "strings"
 "sync"
 "sync/atomic"
 "testing"
 "time"

 "github.com/Icatme/wechatbot-go/gateway"
)
type fakeSocket struct{onWrite func([]byte);closed atomic.Bool}
func(f *fakeSocket)Write(_ context.Context,_ byte,b []byte)error{if f.onWrite!=nil{f.onWrite(b)};return nil}
func(f *fakeSocket)Read(ctx context.Context)(byte,[]byte,error){<-ctx.Done();return 0,nil,ctx.Err()}
func(f *fakeSocket)Close()error{f.closed.Store(true);return nil}
func fixture(t *testing.T)(*Adapter,*session,*fakeSocket){t.Helper();a,err:=New(Config{AccountID:"a",BotID:"bot",Secret:"secret"});if err!=nil{t.Fatal(err)};f:=&fakeSocket{};s:=newSession(context.Background(),f);a.current=s;t.Cleanup(s.close);return a,s,f}
func pushed()frame{return frame{Cmd:"aibot_msg_callback",Headers:map[string]string{"req_id":"reply-request"},Body:json.RawMessage(`{"msgid":"m","aibotid":"bot","chattype":"single","from":{"userid":"u"},"msgtype":"text","text":{"content":"hi"}}`)}}
func autoAck(s *session,f *fakeSocket,check func(frame)){f.onWrite=func(b []byte){var v frame;_=json.Unmarshal(b,&v);if check!=nil{check(v)};code:=0;s.deliver(frame{Headers:v.Headers,Code:&code})}}
func TestDecodeIsolation(t *testing.T){a,s,_:=fixture(t);e,err:=a.decode(pushed(),s);if err!=nil||e.Route.ChatID!="u"||e.Route.Platform!="wecom"{t.Fatal(e,err)};v:=pushed();v.Body=[]byte(strings.Replace(string(v.Body),`"bot"`,`"foreign"`,1));if _,err=a.decode(v,s);!errors.Is(err,gateway.ErrUnauthorized){t.Fatal(err)}}
func TestSecretsNotSerialized(t *testing.T){a,s,_:=fixture(t);v:=pushed();v.Body=[]byte(`{"msgid":"m","aibotid":"bot","chattype":"single","from":{"userid":"u"},"msgtype":"image","image":{"url":"https://example.com/secret","aeskey":"secret-key"}}`);e,err:=a.decode(v,s);if err!=nil{t.Fatal(err)};raw,_:=json.Marshal(e);if strings.Contains(string(raw),"secret"){t.Fatal(string(raw))}}
func TestACKCorrelation(t *testing.T){_,s,f:=fixture(t);autoAck(s,f,func(v frame){if v.Headers["req_id"]!="id"{t.Error(v)}});out,err:=s.request(context.Background(),"ping","id",nil);if err!=nil||!out.Accepted{t.Fatal(out,err)}}
func TestACKTimeoutClosesEpoch(t *testing.T){_,s,f:=fixture(t);s.timeout=5*time.Millisecond;_,err:=s.request(context.Background(),"x","id",map[string]string{"x":"y"});var api *gateway.APIError;if !errors.As(err,&api)||!api.Uncertain||!f.closed.Load(){t.Fatal(err,f.closed.Load())}}
func TestMissingACKCode(t *testing.T){_,s,f:=fixture(t);f.onWrite=func(b []byte){var v frame;_=json.Unmarshal(b,&v);s.deliver(frame{Headers:v.Headers})};if _,err:=s.request(context.Background(),"x","id",nil);err==nil||!f.closed.Load(){t.Fatal(err)}}
func TestSameRequestSerialized(t *testing.T){_,s,f:=fixture(t);var active atomic.Int32;f.onWrite=func(b []byte){var v frame;_=json.Unmarshal(b,&v);if active.Add(1)!=1{t.Error("same req_id overlapped")};go func(){time.Sleep(time.Millisecond);active.Add(-1);code:=0;s.deliver(frame{Headers:v.Headers,Code:&code})}()};var wg sync.WaitGroup;for i:=0;i<10;i++{wg.Add(1);go func(){defer wg.Done();if _,err:=s.request(context.Background(),"reply","same",nil);err!=nil{t.Error(err)}}()};wg.Wait()}
func TestStreamSnapshot(t *testing.T){a,s,f:=fixture(t);e,err:=a.decode(pushed(),s);if err!=nil{t.Fatal(err)};autoAck(s,f,func(v frame){var b struct{Type string `json:"msgtype"`;Stream struct{ID,Content string;Finish bool} `json:"stream"`};_=json.Unmarshal(v.Body,&b);if v.Cmd!="aibot_respond_msg"||v.Headers["req_id"]!="reply-request"||b.Stream.ID!="stream"||b.Stream.Content!="whole snapshot"||!b.Stream.Finish{t.Error(v,b)}});out,err:=a.UpdateStream(context.Background(),e.Route,gateway.StreamUpdate{ReplyTo:"m",StreamID:"stream",Content:"whole snapshot",Finished:true});if err!=nil||!out.Accepted{t.Fatal(out,err)}}
func TestOldConnectionReplyRejected(t *testing.T){a,s,_:=fixture(t);e,_:=a.decode(pushed(),s);a.mu.Lock();a.current=nil;a.mu.Unlock();_,err:=a.Send(context.Background(),e.Route,e.Reply("hi"));if !errors.Is(err,gateway.ErrReplyExpired){t.Fatal(err)}}
func TestProactiveCard(t *testing.T){a,s,f:=fixture(t);autoAck(s,f,func(v frame){var b map[string]any;_=json.Unmarshal(v.Body,&b);if v.Cmd!="aibot_send_msg"||b["chatid"]!="chat"||b["msgtype"]!="template_card"{t.Error(v,b)}});out,err:=a.Send(context.Background(),gateway.Route{Account:a.Account(),ChatID:"chat",Kind:"group"},gateway.Message{Card:json.RawMessage(`{"card_type":"text_notice"}`)});if err!=nil||!out.Accepted{t.Fatal(out,err)}}
