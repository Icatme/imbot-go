package weixin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	wechatbot "github.com/Icatme/wechatbot-go"
	"github.com/Icatme/wechatbot-go/gateway"
)

type fakeClient struct {
	handler wechatbot.MessageHandler
	inbound *wechatbot.IncomingMessage
	sent    wechatbot.OutboundMessage
	user    string
}

func (f *fakeClient) Handle(h wechatbot.MessageHandler) { f.handler = h }
func (f *fakeClient) Run(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (f *fakeClient) ReplyMessage(_ context.Context, m *wechatbot.IncomingMessage, out wechatbot.OutboundMessage) (wechatbot.SendResult, error) {
	f.inbound = m
	f.sent = out
	return wechatbot.SendResult{MessageID: "receipt"}, nil
}
func (f *fakeClient) SendMessage(_ context.Context, u string, out wechatbot.OutboundMessage) (wechatbot.SendResult, error) {
	f.user = u
	f.sent = out
	return wechatbot.SendResult{MessageID: "receipt"}, nil
}
func fixture(t *testing.T) (*Adapter, *fakeClient) {
	t.Helper()
	a, err := New(Config{AccountID: "a", Bot: wechatbot.New(), TakeHandlerOwnership: true})
	if err != nil { t.Fatal(err) }
	f := &fakeClient{}
	a.bot = f
	return a, f
}
func incoming() *wechatbot.IncomingMessage {
	return &wechatbot.IncomingMessage{UserID: "u", Text: "hi", Timestamp: time.Now(), ContextToken: "secret-context", SessionID: "session", RunID: "run", Raw: &wechatbot.WireMessage{MessageID: "18446744073709551615", FromUserID: "u", ItemList: []wechatbot.MessageItem{{Type: wechatbot.ItemText, TextItem: &wechatbot.TextItem{Text: "hi"}}}}}
}
func TestOwnershipRequired(t *testing.T) {
	if _, err := New(Config{AccountID: "a", Bot: wechatbot.New()}); err == nil { t.Fatal("implicit handler takeover") }
}
func TestNativeReplyContext(t *testing.T) {
	a, f := fixture(t)
	m := incoming()
	e, err := a.normalize(m)
	if err != nil { t.Fatal(err) }
	out, err := a.Send(context.Background(), e.Route, gateway.Message{Text: "reply", ReplyTo: e.MessageID, ClientID: "stable"})
	if err != nil || !out.Accepted || f.inbound.ContextToken != "secret-context" || f.sent.ClientID != "stable" || f.sent.RunID != "run" { t.Fatal(out, err, f.sent) }
	if e.MessageID != "18446744073709551615" { t.Fatal("lossy id") }
}
func TestNativeMutationCannotRetarget(t *testing.T) {
	a, f := fixture(t)
	e, _ := a.normalize(incoming())
	native := e.Native.(*wechatbot.IncomingMessage)
	native.UserID = "foreign"
	native.ContextToken = "wrong"
	if _, err := a.Send(context.Background(), e.Route, e.Reply("reply")); err != nil { t.Fatal(err) }
	if f.inbound.UserID != "u" || f.inbound.ContextToken != "secret-context" { t.Fatal("cached target mutated") }
}
func TestReplyRouteIsolation(t *testing.T) {
	a, _ := fixture(t)
	e, _ := a.normalize(incoming())
	e.Route.ChatID = "other"
	if _, err := a.Send(context.Background(), e.Route, e.Reply("reply")); !errors.Is(err, gateway.ErrReplyExpired) { t.Fatal(err) }
}
func TestAckRetryDropMapping(t *testing.T) {
	a, _ := fixture(t)
	for _, err := range []error{nil, gateway.ErrUnauthorized, errors.New("db down")} {
		got := a.handle(context.Background(), incoming(), func(context.Context, gateway.Event) error { return err })
		want := wechatbot.AckMessage()
		if errors.Is(err, gateway.ErrUnauthorized) { want = wechatbot.DropMessage(err) } else if err != nil { want = wechatbot.RetryMessage(err) }
		if got.Action != want.Action { t.Fatal(got, want) }
	}
}
func TestSecretNotSerialized(t *testing.T) {
	a, _ := fixture(t)
	e, _ := a.normalize(incoming())
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "secret-context") { t.Fatal(string(b)) }
}
func TestMediaURLNotCopiedToText(t *testing.T) {
	a, _ := fixture(t)
	m := incoming()
	m.Text = "https://cdn.invalid/?secret=key"
	m.Raw.ItemList = []wechatbot.MessageItem{{Type: wechatbot.ItemImage, ImageItem: &wechatbot.ImageItem{URL: m.Text}}}
	m.Images = []wechatbot.ImageContent{{URL: m.Text}}
	e, err := a.normalize(m)
	if err != nil || e.Text != "" || len(e.Attachments) != 1 { t.Fatal(e, err) }
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "secret=key") { t.Fatal(string(b)) }
}
func TestGroupSendFailsClosed(t *testing.T) {
	a, _ := fixture(t)
	m := incoming()
	m.GroupID = "group"
	e, _ := a.normalize(m)
	if _, err := a.Send(context.Background(), e.Route, e.Reply("reply")); !errors.Is(err, gateway.ErrUnsupported) { t.Fatal(err) }
}
