package dingtalk

import(
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "net/http"
 "net/http/httptest"
 "net/url"
 "strings"
 "testing"
 "time"
 "github.com/Icatme/wechatbot-go/gateway"
)
func testAdapter(t *testing.T)*Adapter{t.Helper();a,err:=New(Config{AccountID:"a",ClientID:"app",ClientSecret:"secret",RobotCode:"robot"});if err!=nil{t.Fatal(err)};return a}
func data()[]byte{return []byte(`{"msgId":"m","msgtype":"text","senderStaffId":"u","senderCorpId":"tenant","conversationId":"c","conversationType":"1","text":{"content":"hi"}}`)}
func TestDecode(t *testing.T){a:=testAdapter(t);e,err:=a.Decode(data(),"delivery");if err!=nil||e.Route.ChatID!="u"||e.Route.TenantID!="tenant"||e.MessageID!="m"||e.ID!="delivery"{t.Fatal(e,err)}}
func TestGroupGate(t *testing.T){a:=testAdapter(t);b:=strings.Replace(string(data()),`"conversationType":"1"`,`"conversationType":"2"`,1);if _,err:=a.Decode([]byte(b),"e");!errors.Is(err,gateway.ErrUnauthorized){t.Fatal(err)}}
func TestSystemACK(t *testing.T){a:=testAdapter(t);f:=streamFrame{Type:"SYSTEM",Headers:map[string]string{"topic":"disconnect","messageId":"id"},Data:`{"opaque":"ping"}`};ack,stop:=a.handle(context.Background(),f,func(context.Context,gateway.Event)error{t.Fatal("system dispatched");return nil});if !stop||ack.Code!=200||ack.Headers["messageId"]!="id"||ack.Data!=f.Data{t.Fatal(ack,stop)}}
func TestAdmissionFailure(t *testing.T){a:=testAdapter(t);f:=streamFrame{Type:"CALLBACK",Headers:map[string]string{"topic":topic,"messageId":"id"},Data:string(data())};ack,_:=a.handle(context.Background(),f,func(context.Context,gateway.Event)error{return errors.New("db down")});if ack.Code!=500{t.Fatal(ack)}}
func TestWebhookDestination(t *testing.T){a:=testAdapter(t);for _,s:=range []string{"https://evil.test/robot/send","https://oapi.dingtalk.com@evil.test/robot/send","https://oapi.dingtalk.com/other","http://oapi.dingtalk.com/robot/send"}{if a.validWebhook(s){t.Fatal(s)}}}
func TestReplyExpired(t *testing.T){a:=testAdapter(t);_,err:=a.Send(context.Background(),gateway.Route{Account:a.Account(),ChatID:"u",Kind:"direct"},gateway.Message{Text:"hi",ReplyTo:"expired"});if !errors.Is(err,gateway.ErrReplyExpired){t.Fatal(err)}}
func TestReplyContract(t *testing.T){
 a:=testAdapter(t);s:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){var b map[string]any;_=json.NewDecoder(r.Body).Decode(&b);if b["msgtype"]!="markdown"||r.URL.Path!="/robot/send"{t.Error(b,r.URL.Path)};fmt.Fprint(w,`{"errcode":0}`)}));defer s.Close();u,_:=url.Parse(s.URL);a.cfg.SessionWebhookHost=u.Host
 var v map[string]any;_=json.Unmarshal(data(),&v);v["sessionWebhook"]=s.URL+"/robot/send?access_token=secret";v["sessionWebhookExpiredTime"]=time.Now().Add(time.Minute).UnixMilli();raw,_:=json.Marshal(v);e,err:=a.Decode(raw,"e");if err!=nil{t.Fatal(err)};b,_:=json.Marshal(e);if strings.Contains(string(b),"access_token"){t.Fatal("secret exposed")};out,err:=a.Send(context.Background(),e.Route,gateway.Message{Text:"hi",ReplyTo:"m",Format:"markdown"});if err!=nil||!out.Accepted{t.Fatal(out,err)}
}
func TestProactiveContract(t *testing.T){
 a:=testAdapter(t);s:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.URL.Path=="/v1.0/oauth2/accessToken"{fmt.Fprint(w,`{"accessToken":"token","expireIn":7200}`);return};if r.URL.Path!="/v1.0/robot/oToMessages/batchSend"||r.Header.Get("x-acs-dingtalk-access-token")!="token"{t.Error(r.URL.Path)};fmt.Fprint(w,`{"processQueryKey":"out"}`)}));defer s.Close();a.cfg.APIBase=s.URL;out,err:=a.Send(context.Background(),gateway.Route{Account:a.Account(),ChatID:"u",Kind:"direct"},gateway.Message{Text:"hi"});if err!=nil||out.MessageID!="out"{t.Fatal(out,err)}
}
