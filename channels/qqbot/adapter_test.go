package qqbot

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/gateway"
	"github.com/Icatme/imbot-go/receive"
)

func fixture(t *testing.T) *Adapter {
	t.Helper()
	a, err := New(Config{AccountID: "a", AppID: "app", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func signRequest(a *Adapter, b []byte) *http.Request {
	r := httptest.NewRequest("POST", "/qq", bytes.NewReader(b))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r.Header.Set("X-Signature-Timestamp", ts)
	r.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(a.private, append([]byte(ts), b...))))
	return r
}
func payload() []byte {
	return []byte(`{"op":0,"id":"event","t":"C2C_MESSAGE_CREATE","d":{"id":"msg","content":"hello","timestamp":"2026-11-12T08:00:00+08:00","author":{"user_openid":"user"}}}`)
}

// Fixed raw bytes from the official BotGo signature fixture, not copied SDK code.
func TestOfficialSignatureVector(t *testing.T) {
	a, err := New(Config{AccountID: "a", AppID: "app", ClientSecret: "123456abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	sig := "e949b5b94ef4103df903fb031d1d16e358db3db83e79e117edd404c8508be3ce8a76d7bad1bed353194c126a1a5915b4ad8b5288c1191cc53a12acffccd82004"
	body := []byte(`{"id":"ROBOT1.0_veoihSEXDc8Q.g-6eLpNIa11bH8MisOjn-m-LKxCPntMk6exUXgcWCGpVO7L2QKTNZzjZzFFDSbiOFcqAPWyVA!!","content":"哦一下","timestamp":"2024-10-15T16:33:15+08:00","author":{"id":"675860273","user_openid":"675860273"}}`)
	if !a.verify("1728981195", sig, body) {
		t.Fatal("official fixed-byte signature failed")
	}
	if a.verify("1728981195", sig, append(body, ' ')) {
		t.Fatal("changed raw bytes accepted")
	}
}
func TestOfficialPublicKeyVector(t *testing.T) {
	a, err := New(Config{AccountID: "a", AppID: "app", ClientSecret: "naOC0ocQE3shWLAfffVLB1rhYPG7"})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{215, 195, 98, 254, 120, 174, 248, 31, 242, 50, 135, 180, 147, 98, 139, 93, 176, 42, 60, 79, 227, 11, 33, 94, 77, 25, 96, 155, 93, 118, 103, 58}
	if !bytes.Equal(a.public, want) {
		t.Fatal("public key differs from official protocol sample")
	}
}
func TestEmptySecret(t *testing.T) {
	if _, err := key(""); err == nil {
		t.Fatal("empty secret")
	}
}
func TestChallengeAuthenticated(t *testing.T) {
	a := fixture(t)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	b := []byte(`{"op":13,"d":{"plain_token":"challenge","event_ts":"` + ts + `"}}`)
	w := httptest.NewRecorder()
	a.Handler(func(context.Context, gateway.Event) error { t.Fatal("challenge dispatched"); return nil }).ServeHTTP(w, signRequest(a, b))
	var v map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	sig, _ := hex.DecodeString(v["signature"])
	if w.Code != 200 || v["plain_token"] != "challenge" || !ed25519.Verify(a.public, []byte(ts+"challenge"), sig) {
		t.Fatal(w.Code, v)
	}
}
func TestTamperAndUnsigned(t *testing.T) {
	a := fixture(t)
	for _, r := range []*http.Request{httptest.NewRequest("POST", "/qq", bytes.NewReader(payload())), signRequest(a, []byte(`{}`))} {
		r.Body = ioBody(payload())
		w := httptest.NewRecorder()
		a.Handler(func(context.Context, gateway.Event) error { t.Fatal("invalid request dispatched"); return nil }).ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}

type byteBody struct{ *bytes.Reader }

func (b byteBody) Close() error { return nil }
func ioBody(b []byte) byteBody  { return byteBody{bytes.NewReader(b)} }
func TestOldTimestamp(t *testing.T) {
	a := fixture(t)
	r := signRequest(a, payload())
	r.Header.Set("X-Signature-Timestamp", "1")
	r.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(a.private, append([]byte("1"), payload()...))))
	w := httptest.NewRecorder()
	a.Handler(func(context.Context, gateway.Event) error { t.Fatal("replay"); return nil }).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestAdmissionACK(t *testing.T) {
	a := fixture(t)
	for _, tc := range []struct {
		err          error
		ack, reports int
	}{{nil, 0, 0}, {errors.New("persist failed"), 1, 1}, {&receive.Rejection{Kind: receive.Voice}, 0, 0}} {
		reports := 0
		a.cfg.OnError = func(error) { reports++ }
		w := httptest.NewRecorder()
		a.Handler(func(_ context.Context, e gateway.Event) error {
			if e.Route.TenantID != "app" || e.Route.ChatID != "user" {
				t.Error(e)
			}
			return tc.err
		}).ServeHTTP(w, signRequest(a, payload()))
		var v map[string]int
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if w.Code != 200 || v["op"] != 12 || v["d"] != tc.ack || reports != tc.reports {
			t.Fatal(w.Code, v, reports)
		}
	}
}
func TestHeartbeat(t *testing.T) {
	a := fixture(t)
	w := httptest.NewRecorder()
	a.Handler(func(context.Context, gateway.Event) error { t.Fatal("heartbeat"); return nil }).ServeHTTP(w, signRequest(a, []byte(`{"op":1,"d":123}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"op":11`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestGroupRouting(t *testing.T) {
	a := fixture(t)
	e, err := a.decode(envelope{ID: "ev", Type: "GROUP_AT_MESSAGE_CREATE", Data: json.RawMessage(`{"id":"m","group_openid":"g","author":{"member_openid":"u"},"content":"hi"}`)})
	if err != nil || e.Route.Kind != "group" || e.Route.ChatID != "g" || e.SenderID != "u" || !e.Mentioned {
		t.Fatal(e, err)
	}
}
func TestSendContract(t *testing.T) {
	a := fixture(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			fmt.Fprint(w, `{"access_token":"token","expires_in":"7200"}`)
			return
		}
		if r.URL.Path != "/v2/users/user/messages" || r.Header.Get("Authorization") != "QQBot token" {
			t.Error(r.URL.Path, r.Header)
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["msg_id"] != "msg" || b["msg_seq"] != float64(2) {
			t.Error(b)
		}
		fmt.Fprint(w, `{"id":"out"}`)
	}))
	defer s.Close()
	a.cfg.APIBase = s.URL
	a.cfg.TokenURL = s.URL + "/app/getAppAccessToken"
	out, err := a.Send(context.Background(), gateway.Route{Account: a.Account(), TenantID: "app", ChatID: "user", Kind: "direct"}, gateway.Message{Text: "hi", ReplyTo: "msg", Sequence: 2})
	if err != nil || out.MessageID != "out" {
		t.Fatal(out, err)
	}
}
func TestOptInCapabilities(t *testing.T) {
	a := fixture(t)
	r := gateway.Route{Account: a.Account(), TenantID: "app", ChatID: "user", Kind: "direct"}
	for _, m := range []gateway.Message{{Text: "hi"}, {Text: "hi", ReplyTo: "m", Format: "markdown"}} {
		if _, err := a.Send(context.Background(), r, m); !errors.Is(err, gateway.ErrUnsupported) {
			t.Fatal(err)
		}
	}
}
