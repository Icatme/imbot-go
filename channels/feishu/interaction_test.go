package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

func cardPayload() []byte {
	return []byte(`{"schema":"2.0","header":{"event_id":"click-1","event_type":"card.action.trigger","create_time":"1730000000000000","tenant_key":"tenant","token":"verify","app_id":"app"},"event":{"operator":{"open_id":"user"},"token":"secret-card-token","action":{"tag":"button","value":{"operation":"approve","item":42}},"context":{"open_message_id":"msg","open_chat_id":"chat"}}}`)
}

func cardRoute(a *Adapter) gateway.Route {
	return gateway.Route{Account: a.Account(), TenantID: "tenant", ChatID: "chat", Kind: "group", ThreadID: "thread"}
}

func TestCardActionRetainsRouteAndPassesGatewayAuthorization(t *testing.T) {
	a := testAdapter(t)
	route := cardRoute(a)
	a.rememberCardRoute(route, "msg")
	admissions, authorizations := 0, 0
	g, err := gateway.New(gateway.Options{
		Authorize: func(_ context.Context, e gateway.Event) error {
			authorizations++
			if e.SenderID == "denied" {
				return gateway.ErrUnauthorized
			}
			if e.Route != route || e.Interaction == nil || e.Interaction.Name != "button" {
				t.Errorf("incorrect authorized action: %+v", e)
			}
			return nil
		},
		Handler: func(_ context.Context, e gateway.Event) error {
			admissions++
			if e.MessageID != "msg" || e.ID == e.MessageID || e.Interaction.ID != e.ID || e.Interaction.MessageID != "msg" || string(e.Interaction.Value) != `{"tag":"button","value":{"operation":"approve","item":42}}` {
				t.Errorf("incorrect action identity: %+v", e)
			}
			raw, _ := json.Marshal(e)
			if bytes.Contains(raw, []byte("secret-card-token")) {
				t.Error("card credential escaped normalization")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Register(a); err != nil {
		t.Fatal(err)
	}
	h := a.Handler(g.Bind(a.Account()))
	for _, raw := range [][]byte{cardPayload(), cardPayload(), bytes.Replace(cardPayload(), []byte("click-1"), []byte("click-2"), 1), bytes.Replace(cardPayload(), []byte(`"user"`), []byte(`"denied"`), 1)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signed(raw))
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{}" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if admissions != 2 || authorizations != 4 {
		t.Fatal(admissions, authorizations)
	}
}

func TestCardActionRejectsMissingOrExpiredRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      []byte
		remember bool
		want     error
	}{
		{"unknown_route", cardPayload(), false, gateway.ErrReplyExpired},
		{"other_chat", bytes.Replace(cardPayload(), []byte(`"chat"`), []byte(`"other"`), 1), true, gateway.ErrReplyExpired},
		{"other_tenant", bytes.Replace(cardPayload(), []byte(`"tenant"`), []byte(`"other"`), 1), true, gateway.ErrReplyExpired},
		{"missing_user", bytes.Replace(cardPayload(), []byte(`"user"`), []byte(`""`), 1), true, gateway.ErrInvalidEvent},
		{"missing_chat", bytes.Replace(cardPayload(), []byte(`"chat"`), []byte(`""`), 1), true, gateway.ErrInvalidEvent},
		{"missing_event_id", bytes.Replace(cardPayload(), []byte(`"click-1"`), []byte(`""`), 1), true, gateway.ErrInvalidEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testAdapter(t)
			if tc.remember {
				a.rememberCardRoute(cardRoute(a), "msg")
			}
			if _, err := a.Decode(tc.raw); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			a.Handler(func(context.Context, gateway.Event) error { t.Fatal("invalid callback reached sink"); return nil }).ServeHTTP(w, signed(tc.raw))
			if w.Code != 503 {
				t.Fatal(w.Code)
			}
		})
	}
	a := testAdapter(t)
	a.cardRoutes.Put(cardRouteKey("tenant", "chat", "msg"), cardRoute(a), time.Now().Add(-time.Second))
	if _, err := a.Decode(cardPayload()); !errors.Is(err, gateway.ErrReplyExpired) {
		t.Fatal(err)
	}
}

func TestCardSendKeepsNativeContentAndRegistersRoute(t *testing.T) {
	a := testAdapter(t)
	route := cardRoute(a)
	route.ThreadID = ""
	card := json.RawMessage(`{"schema":"2.0","body":{"elements":[]}}`)
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"tenant_access_token":"token","expire":7200}`
		if strings.HasSuffix(r.URL.Path, "/messages") {
			var request map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			var content, typ string
			json.Unmarshal(request["content"], &content)
			json.Unmarshal(request["msg_type"], &typ)
			if content != string(card) || typ != "interactive" {
				t.Fatal(content, typ)
			}
			body = `{"code":0,"data":{"message_id":"msg"}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if _, err := a.Send(context.Background(), route, gateway.Message{Card: card}); err != nil {
		t.Fatal(err)
	}
	e, err := a.Decode(cardPayload())
	if err != nil || e.Route != route {
		t.Fatal(e, err)
	}
}
