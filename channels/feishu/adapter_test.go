package feishu

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

func testAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := New(Config{AccountID: "bot", AppID: "app", AppSecret: "secret", VerificationToken: "verify", EncryptKey: "key", BotOpenID: "bot-open"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func payload() []byte {
	return []byte(`{"header":{"event_id":"ev","event_type":"im.message.receive_v1","tenant_key":"tenant","token":"verify","app_id":"app"},"event":{"sender":{"sender_type":"user","sender_id":{"open_id":"user"}},"message":{"message_id":"msg","chat_id":"chat","chat_type":"p2p","message_type":"text","content":"{\"text\":\"你好\"}","create_time":"1730000000000"}}}`)
}
func TestDecode(t *testing.T) {
	a := testAdapter(t)
	e, err := a.Decode(payload())
	if err != nil || e.Text != "你好" || e.Route.ID != "bot" || e.Route.TenantID != "tenant" {
		t.Fatal(e, err)
	}
}
func TestGroupMentionGate(t *testing.T) {
	a := testAdapter(t)
	raw := bytes.Replace(payload(), []byte(`"p2p"`), []byte(`"group"`), 1)
	if _, err := a.Decode(raw); !errors.Is(err, gateway.ErrUnauthorized) {
		t.Fatal(err)
	}
}
func signed(raw []byte) *http.Request {
	r := httptest.NewRequest("POST", "/callback", bytes.NewReader(raw))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha256.Sum256(append([]byte(ts+"nonce"+"key"), raw...))
	r.Header.Set("X-Lark-Request-Timestamp", ts)
	r.Header.Set("X-Lark-Request-Nonce", "nonce")
	r.Header.Set("X-Lark-Signature", hex.EncodeToString(sum[:]))
	return r
}
func TestWebhookSignatureAndAdmission(t *testing.T) {
	a := testAdapter(t)
	n := 0
	h := a.Handler(func(context.Context, gateway.Event) error { n++; return nil })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/callback", bytes.NewReader(payload())))
	if w.Code != 403 || n != 0 {
		t.Fatal(w.Code, n)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signed(payload()))
	if w.Code != 200 || n != 1 {
		t.Fatal(w.Code, n)
	}
	bad := signed(payload())
	bad.Header.Set("X-Lark-Signature", "00")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, bad)
	if w.Code != 403 || n != 1 {
		t.Fatal(w.Code, n)
	}
}
func TestEncryptedChallenge(t *testing.T) {
	a := testAdapter(t)
	plain := []byte(`{"type":"url_verification","token":"verify","challenge":"ok"}`)
	n := 16 - len(plain)%16
	plain = append(plain, bytes.Repeat([]byte{byte(n)}, n)...)
	key := sha256.Sum256([]byte("key"))
	block, _ := aes.NewCipher(key[:])
	iv := make([]byte, 16)
	enc := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, plain)
	raw, _ := json.Marshal(map[string]string{"encrypt": base64.StdEncoding.EncodeToString(append(iv, enc...))})
	w := httptest.NewRecorder()
	a.Handler(func(context.Context, gateway.Event) error { t.Fatal("challenge reached agent"); return nil }).ServeHTTP(w, signed(raw))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("ok")) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestFrameRoundTrip(t *testing.T) {
	f := frame{Seq: 1 << 60, Log: 1 << 61, Service: 123, Method: 1, Headers: map[string]string{"type": "event"}, Payload: []byte("hi"), Extra: blob(nil, 9, []byte("new-log"))}
	got, err := decodeFrame(f.encode())
	if err != nil || got.Seq != f.Seq || got.Log != f.Log || string(got.Payload) != "hi" || !bytes.Equal(got.Extra, f.Extra) {
		t.Fatal(got, err)
	}
}
func TestFrameMalformed(t *testing.T) {
	for _, b := range [][]byte{{0}, {0xff}, {0x42, 0xff, 0xff}, {0x0a, 0}} {
		if _, err := decodeFrame(b); err == nil {
			t.Fatalf("accepted %x", b)
		}
	}
}
func TestAssembly(t *testing.T) {
	var a assembler
	f := frame{Headers: map[string]string{"sum": "2", "seq": "1", "message_id": "m"}, Payload: []byte("b")}
	if _, ready, err := a.combine(f); err != nil || ready {
		t.Fatal(ready, err)
	}
	f.Headers["seq"] = "0"
	f.Payload = []byte("a")
	b, ready, err := a.combine(f)
	if err != nil || !ready || string(b) != "ab" {
		t.Fatal(string(b), ready, err)
	}
	f.Headers["sum"] = "999"
	if _, _, err = a.combine(f); err == nil {
		t.Fatal("unbounded fragments")
	}
}
func TestSendContract(t *testing.T) {
	a := testAdapter(t)
	n := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"token","expire":7200}`)
			return
		}
		n++
		if r.Header.Get("Authorization") != "Bearer token" || r.URL.Path != "/open-apis/im/v1/messages/msg/reply" {
			t.Error(r.URL.Path, r.Header)
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["reply_in_thread"] != true || b["uuid"] == "" {
			t.Error(b)
		}
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"out"}}`)
	}))
	defer s.Close()
	a.cfg.BaseURL = s.URL
	r := gateway.Route{Account: a.Account(), ChatID: "chat", ThreadID: "thread"}
	a.replyRoutes.Put(wire.ContextKey(r, "msg"), true, time.Now().Add(time.Minute))
	out, err := a.Send(context.Background(), r, gateway.Message{Text: "hi", ReplyTo: "msg", ClientID: "stable"})
	if err != nil || !out.Accepted || n != 1 {
		t.Fatal(out, err, n)
	}
}
func TestReplyRouteIsolation(t *testing.T) {
	a := testAdapter(t)
	e, err := a.Decode(payload())
	if err != nil {
		t.Fatal(err)
	}
	e.Route.ChatID = "wrong-chat"
	if _, err := a.Send(context.Background(), e.Route, e.Reply("no")); !errors.Is(err, gateway.ErrReplyExpired) {
		t.Fatal(err)
	}
}

func TestDiscoveryErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		code      int
		retryable bool
	}{{1, true}, {1000040343, true}, {1000040344, false}, {403, false}} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			a := testAdapter(t)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":%d}`, tc.code)
			}))
			defer s.Close()
			a.cfg.BaseURL = s.URL
			err := a.connect(context.Background(), func(context.Context, gateway.Event) error { t.Fatal("unexpected admission"); return nil })
			var api *gateway.APIError
			if !errors.As(err, &api) || api.Retryable != tc.retryable || api.Code != strconv.Itoa(tc.code) {
				t.Fatalf("discovery code %d: %v", tc.code, err)
			}
		})
	}
}
