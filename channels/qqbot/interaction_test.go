package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

func interactionPayload(t *testing.T) []byte {
	t.Helper()
	return protocolFixture(t, "interaction_button_c2c.json")
}

func TestInteractionUsesAuthenticatedGatewayAdmission(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "denied", true: "allowed"}[allowed], func(t *testing.T) {
			a := fixture(t)
			authorized, handled := 0, 0
			g, err := gateway.New(gateway.Options{
				Authorize: func(_ context.Context, e gateway.Event) error {
					authorized++
					if e.Route != route(a) || e.SenderID != "user" || e.Interaction == nil || e.Interaction.ID != "interaction" || e.Interaction.Name != "confirm" || e.Interaction.MessageID != "original" || string(e.Interaction.Value) != `"chosen"` {
						t.Fatalf("wrong interaction: %+v", e)
					}
					if !allowed {
						return gateway.ErrUnauthorized
					}
					return nil
				},
				Handler: func(context.Context, gateway.Event) error { handled++; return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Register(a); err != nil {
				t.Fatal(err)
			}
			h := a.Handler(g.Bind(a.Account()))
			unsigned := httptest.NewRecorder()
			h.ServeHTTP(unsigned, httptest.NewRequest("POST", "/qq", strings.NewReader(string(interactionPayload(t)))))
			if unsigned.Code != http.StatusForbidden || authorized != 0 || handled != 0 {
				t.Fatal("unsigned interaction reached admission")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, signRequest(a, interactionPayload(t)))
			var ack map[string]int
			if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || ack["op"] != 12 || ack["d"] != 0 || authorized != 1 || handled != map[bool]int{false: 0, true: 1}[allowed] {
				t.Fatalf("wrong admission: %d %v authorize=%d handled=%d", w.Code, ack, authorized, handled)
			}
		})
	}
}

func TestInteractionGroupAndIdentityValidation(t *testing.T) {
	a := fixture(t)
	group := `{"id":"interaction","type":2,"chat_type":1,"group_openid":"group","group_member_openid":"member","data":{"type":11,"resolved":{"button_id":"confirm","button_data":"chosen","user_id":"member"}}}`
	e, err := a.decode(envelope{Type: "INTERACTION_CREATE", Data: json.RawMessage(group)})
	if err != nil || e.Route.Kind != "group" || e.Route.ChatID != "group" || e.SenderID != "member" {
		t.Fatal(e, err)
	}
	for _, invalid := range []string{strings.Replace(group, `"user_id":"member"`, `"user_id":"other"`, 1), strings.Replace(group, `"group_member_openid":"member",`, "", 1)} {
		if _, err := a.decode(envelope{Type: "INTERACTION_CREATE", Data: json.RawMessage(invalid)}); !errors.Is(err, gateway.ErrInvalidEvent) {
			t.Fatalf("invalid actor accepted: %v", err)
		}
	}
	for _, unsupported := range []string{strings.Replace(group, `"chat_type":1`, `"chat_type":0`, 1), strings.Replace(group, `"button_id":"confirm",`, "", 1)} {
		if _, err := a.decode(envelope{Type: "INTERACTION_CREATE", Data: json.RawMessage(unsupported)}); !errors.Is(err, gateway.ErrUnsupported) {
			t.Fatalf("unsupported interaction accepted: %v", err)
		}
	}
}

func TestInteractionReplyNeverUsesPassiveMessageID(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "expired"}[expired], func(t *testing.T) {
			a := fixture(t)
			var v envelope
			if err := json.Unmarshal(interactionPayload(t), &v); err != nil {
				t.Fatal(err)
			}
			e, err := a.decode(v)
			if err != nil {
				t.Fatal(err)
			}
			if e.MessageID != interactionMessagePrefix+e.Interaction.ID {
				t.Fatalf("interaction message ID lacks explicit namespace: %q", e.MessageID)
			}
			if expired {
				a.interactions.Put(wire.ContextKey(e.Route, e.Interaction.ID), true, time.Now().Add(-time.Second))
			}
			calls := 0
			a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return response(`{}`), nil
			})}
			if _, err := a.Send(context.Background(), e.Route, e.Reply("clicked")); !errors.Is(err, gateway.ErrUnsupported) {
				t.Fatalf("interaction accepted as passive reply: %v", err)
			}
			if !expired {
				if _, err := a.Send(context.Background(), e.Route, gateway.Message{Text: "clicked", ReplyTo: e.Interaction.ID}); !errors.Is(err, gateway.ErrUnsupported) {
					t.Fatalf("known raw interaction ID accepted as passive reply: %v", err)
				}
			}
			if _, err := a.Upload(context.Background(), e.Route, gateway.MediaUpload{Kind: "file", Data: []byte("data"), ReplyTo: e.Reply("clicked").ReplyTo}); !errors.Is(err, gateway.ErrUnsupported) {
				t.Fatalf("interaction accepted as upload passive-reply permission: %v", err)
			}
			if calls != 0 {
				t.Fatalf("interaction reply made %d HTTP requests", calls)
			}
		})
	}
}

func TestInteractionAcknowledgementIsExplicitAndRouteBound(t *testing.T) {
	a := fixture(t)
	var v envelope
	if err := json.Unmarshal(interactionPayload(t), &v); err != nil {
		t.Fatal(err)
	}
	e, err := a.decode(v)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		if r.URL.Path != "/interactions/interaction" || r.Method != http.MethodPut || r.Header.Get("Authorization") != "QQBot tok" {
			t.Fatalf("wrong interaction ACK: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]int
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["code"] != 0 {
			t.Fatalf("wrong ACK code: %v %v", body, err)
		}
		out := response("")
		out.StatusCode = http.StatusNoContent
		return out, nil
	})}
	other := e.Route
	other.ChatID = "other"
	if err := a.AcknowledgeInteraction(context.Background(), other, e.ID, 0); !errors.Is(err, gateway.ErrReplyExpired) {
		t.Fatal(err)
	}
	if err := a.AcknowledgeInteraction(context.Background(), e.Route, e.ID, 6); !errors.Is(err, gateway.ErrInvalidEvent) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("invalid ACK caused IO")
	}
	if err := a.AcknowledgeInteraction(context.Background(), e.Route, e.ID, 0); err != nil || calls != 2 {
		t.Fatalf("ACK failed: %v %d", err, calls)
	}
	a.interactions.Put(wire.ContextKey(e.Route, e.ID), true, time.Now().Add(-time.Second))
	if err := a.AcknowledgeInteraction(context.Background(), e.Route, e.ID, 0); !errors.Is(err, gateway.ErrReplyExpired) || calls != 2 {
		t.Fatalf("expired ACK used network: %v %d", err, calls)
	}
}

const keyboardJSON = `{"content":{"rows":[{"buttons":[{"id":"confirm","render_data":{"label":"Confirm","visited_label":"Confirmed","style":1},"action":{"type":2,"data":"confirm","permission":{"type":2}}}]}]}}`

func TestNativeKeyboardOptInAndExactPayload(t *testing.T) {
	a := fixture(t)
	m := gateway.Message{Text: "Choose", ReplyTo: "message", Card: json.RawMessage(keyboardJSON)}
	if _, err := a.Send(context.Background(), route(a), m); !errors.Is(err, gateway.ErrUnsupported) {
		t.Fatalf("keyboard accepted without opt-in: %v", err)
	}
	a.cfg.EnableKeyboard = true
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		var body struct {
			Keyboard json.RawMessage `json:"keyboard"`
			Content  string          `json:"content"`
			ReplyTo  string          `json:"msg_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body.Keyboard) != keyboardJSON || body.Content != m.Text || body.ReplyTo != m.ReplyTo {
			t.Fatalf("keyboard changed: %s %v", body.Keyboard, err)
		}
		return response(`{"id":"sent"}`), nil
	})}
	if !a.Capabilities().Card {
		t.Fatal("keyboard capability not advertised")
	}
	if _, err := a.Send(context.Background(), route(a), m); err != nil || calls != 2 {
		t.Fatalf("keyboard send failed: %v %d", err, calls)
	}
	for _, invalid := range []string{
		strings.Replace(keyboardJSON, `,"permission":{"type":2}`, "", 1),
		strings.Replace(keyboardJSON, `"permission":{"type":2}`, `"permission":{"type":0,"specify_user_ids":["user"]}`, 1),
		strings.Replace(keyboardJSON, `"style":1`, `"style":9`, 1),
		keyboardJSON + `{}`,
	} {
		m.Card = json.RawMessage(invalid)
		if _, err := a.Send(context.Background(), route(a), m); err == nil {
			t.Fatal("malformed/unsupported keyboard accepted")
		}
	}
	if calls != 2 {
		t.Fatal("invalid keyboard used network")
	}
}
