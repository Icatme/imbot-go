package wecomapp

import(
 "bytes"
 "context"
 "crypto/aes"
 "crypto/cipher"
 "encoding/base64"
 "encoding/binary"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "net/http"
 "net/http/httptest"
 "net/url"
 "strconv"
 "strings"
 "testing"
 "time"

 "github.com/Icatme/wechatbot-go/gateway"
)
func fixture(t *testing.T)*Adapter{t.Helper();a,err:=New(Config{AccountID:"a",CorpID:"corp",CorpSecret:"secret",Token:"token",AgentID:100,EncodingAESKey:strings.TrimRight(base64.StdEncoding.EncodeToString(make([]byte,32)),"=")});if err!=nil{t.Fatal(err)};return a}
func encrypt(a *Adapter,text,corp string)string{b:=make([]byte,16);b=binary.BigEndian.AppendUint32(b,uint32(len(text)));b=append(b,[]byte(text)...);b=append(b,[]byte(corp)...);n:=32-len(b)%32;b=append(b,bytes.Repeat([]byte{byte(n)},n)...);block,_:=aes.NewCipher(a.key);cipher.NewCBCEncrypter(block,a.key[:16]).CryptBlocks(b,b);return base64.StdEncoding.EncodeToString(b)}
func request(a *Adapter,method,encrypted,ts string)*http.Request{q:=url.Values{"timestamp":{ts},"nonce":{"n"},"msg_signature":{hex.EncodeToString(signature(a.cfg.Token,ts,"n",encrypted))}};if method=="GET"{q.Set("echostr",encrypted)};return httptest.NewRequest(method,"/callback?"+q.Encode(),strings.NewReader("<xml><Encrypt>"+encrypted+"</Encrypt></xml>"))}
func xmlMessage()string{return "<xml><ToUserName>corp</ToUserName><FromUserName>u</FromUserName><AgentID>100</AgentID><MsgId>18446744073709551615</MsgId><MsgType>text</MsgType><Content>你好</Content><CreateTime>1000</CreateTime></xml>"}
func TestURLChallenge(t *testing.T){a:=fixture(t);enc:=encrypt(a,"challenge","corp");w:=httptest.NewRecorder();a.Handler(func(context.Context,gateway.Event)error{t.Fatal("challenge dispatched");return nil}).ServeHTTP(w,request(a,"GET",enc,strconv.FormatInt(time.Now().Unix(),10)));if w.Code!=200||w.Body.String()!="challenge"{t.Fatal(w.Code,w.Body.String())}}
func TestCallbackAdmission(t *testing.T){a:=fixture(t);enc:=encrypt(a,xmlMessage(),"corp");w:=httptest.NewRecorder();n:=0;a.Handler(func(_ context.Context,e gateway.Event)error{n++;if e.ID!="18446744073709551615"||e.Route.TenantID!="corp"{t.Error(e)};return nil}).ServeHTTP(w,request(a,"POST",enc,strconv.FormatInt(time.Now().Unix(),10)));if w.Code!=200||w.Body.String()!="success"||n!=1{t.Fatal(w.Code,n)}}
func TestFailedSinkNotAcknowledged(t *testing.T){a:=fixture(t);w:=httptest.NewRecorder();a.Handler(func(context.Context,gateway.Event)error{return errors.New("db")}).ServeHTTP(w,request(a,"POST",encrypt(a,xmlMessage(),"corp"),strconv.FormatInt(time.Now().Unix(),10)));if w.Code!=503{t.Fatal(w.Code)}}
func TestWrongIdentityRejected(t *testing.T){a:=fixture(t);for _,v:=range []struct{text,corp string}{{xmlMessage(),"foreign"},{strings.Replace(xmlMessage(),"<AgentID>100","<AgentID>101",1),"corp"}}{w:=httptest.NewRecorder();a.Handler(func(context.Context,gateway.Event)error{t.Fatal("wrong identity dispatched");return nil}).ServeHTTP(w,request(a,"POST",encrypt(a,v.text,v.corp),strconv.FormatInt(time.Now().Unix(),10)));if w.Code!=403{t.Fatal(w.Code)}}}
func TestTimestampAndTamper(t *testing.T){a:=fixture(t);for _,ts:=range []string{"1",strconv.FormatInt(time.Now().Unix(),10)}{r:=request(a,"POST",encrypt(a,xmlMessage(),"corp"),ts);if ts!="1"{q:=r.URL.Query();q.Set("msg_signature","00");r.URL.RawQuery=q.Encode()};w:=httptest.NewRecorder();a.Handler(func(context.Context,gateway.Event)error{t.Fatal("tamper");return nil}).ServeHTTP(w,r);if w.Code!=403{t.Fatal(w.Code)}}}
func TestXMLDirectiveRejected(t *testing.T){var v any;if safeXML([]byte(`<!DOCTYPE xml [<!ENTITY x SYSTEM "file:///etc/passwd">]><xml>&x;</xml>`),&v)==nil{t.Fatal("DTD accepted")}}
func TestInvalidCiphertext(t *testing.T){a:=fixture(t);ts:=strconv.FormatInt(time.Now().Unix(),10);enc:=base64.StdEncoding.EncodeToString([]byte{1,2,3});if _,err:=a.decrypt(hex.EncodeToString(signature("token",ts,"n",enc)),ts,"n",enc);err==nil{t.Fatal("accepted")}}
func TestSendAndRecipientError(t *testing.T){a:=fixture(t);invalid:=false;s:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.URL.Path=="/cgi-bin/gettoken"{fmt.Fprint(w,`{"errcode":0,"access_token":"token","expires_in":7200}`);return};var b map[string]any;_=json.NewDecoder(r.Body).Decode(&b);if b["touser"]!="u"||b["agentid"]!=float64(100){t.Error(b)};if invalid{fmt.Fprint(w,`{"errcode":0,"invaliduser":"u"}`)}else{fmt.Fprint(w,`{"errcode":0,"msgid":"out"}`)}}));defer s.Close();a.cfg.APIBase=s.URL;r:=gateway.Route{Account:a.Account(),TenantID:"corp",ChatID:"u",Kind:"direct"};out,err:=a.Send(context.Background(),r,gateway.Message{Text:"hi"});if err!=nil||!out.Accepted{t.Fatal(out,err)};invalid=true;if _,err=a.Send(context.Background(),r,gateway.Message{Text:"hi"});err==nil{t.Fatal("invalid recipient marked accepted")}}
func TestBroadcastAndWrongTenantRejected(t *testing.T){a:=fixture(t);for _,r:=range []gateway.Route{{Account:a.Account(),TenantID:"corp",ChatID:"@all",Kind:"direct"},{Account:a.Account(),TenantID:"other",ChatID:"u",Kind:"direct"},{Account:a.Account(),TenantID:"corp",ChatID:"u|other",Kind:"direct"}}{if _,err:=a.Send(context.Background(),r,gateway.Message{Text:"hi"});!errors.Is(err,gateway.ErrInvalidEvent){t.Fatal(err)}}}
