package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Icatme/wechatbot-go/gateway"
)

func cardEventFrame() frame {
	// Wire shape from the official production parser, independently of our Go types:
	// https://github.com/WecomTeam/wecom-openclaw-plugin/blob/main/src/message-parser.ts
	return frame{Cmd: "aibot_event_callback", Headers: map[string]string{"req_id": "event-request"}, Body: json.RawMessage(`{"msgid":"click-event","aibotid":"bot","create_time":100,"chattype":"single","from":{"userid":"u"},"msgtype":"event","event":{"eventtype":"template_card_event","template_card_event":{"card_type":"multiple_interaction","event_key":"approve","task_id":"task-one","selected_items":{"selected_item":[{"question_key":"plan","option_ids":{"option_id":["first","second"]}}]}}}}`)}
}

func TestTemplateInteractionUsesGatewayAuthorizationAndFailureAdmission(t *testing.T) {
	a, _, _ := fixture(t)
	e, err := a.decodeEvent(cardEventFrame())
	if err != nil {
		t.Fatal(err)
	}
	if e.Interaction == nil || e.Interaction.ID != "click-event" || e.Interaction.Name != "template_card_event" || e.Interaction.MessageID != "" || e.Route.ChatID != "u" {
		t.Fatal(e)
	}
	var value struct {
		Type string `json:"eventtype"`
		Card struct {
			Type     string `json:"card_type"`
			Key      string `json:"event_key"`
			Task     string `json:"task_id"`
			Selected struct {
				Items []struct {
					Key     string `json:"question_key"`
					Options struct {
						IDs []string `json:"option_id"`
					} `json:"option_ids"`
				} `json:"selected_item"`
			} `json:"selected_items"`
		} `json:"template_card_event"`
	}
	if json.Unmarshal(e.Interaction.Value, &value) != nil || value.Type != "template_card_event" || value.Card.Key != "approve" || value.Card.Task != "task-one" || value.Card.Type != "multiple_interaction" || len(value.Card.Selected.Items) != 1 || value.Card.Selected.Items[0].Key != "plan" || !reflect.DeepEqual(value.Card.Selected.Items[0].Options.IDs, []string{"first", "second"}) {
		t.Fatal(string(e.Interaction.Value))
	}
	if _, err = a.Send(context.Background(), e.Route, e.Reply("reply")); !errors.Is(err, gateway.ErrReplyExpired) {
		t.Fatal("event registered as text reply", err)
	}
	allowed := false
	calls := 0
	failure := errors.New("storage unavailable")
	g, err := gateway.New(gateway.Options{Authorize: func(context.Context, gateway.Event) error {
		if !allowed {
			return gateway.ErrUnauthorized
		}
		return nil
	}, Handler: func(_ context.Context, got gateway.Event) error {
		calls++
		if got.Interaction == nil {
			t.Fatal("action lost")
		}
		if calls == 1 {
			return failure
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Register(a); err != nil {
		t.Fatal(err)
	}
	admit := g.Bind(a.Account())
	if err = admit(context.Background(), e); !errors.Is(err, gateway.ErrUnauthorized) || calls != 0 {
		t.Fatal(err, calls)
	}
	allowed = true
	if err = admit(context.Background(), e); !errors.Is(err, failure) || gateway.Consumed(err) {
		t.Fatal("failed action marked admitted", err)
	}
	if err = admit(context.Background(), e); err != nil || calls != 2 {
		t.Fatal("failed action could not be admitted again", err, calls)
	}
}

func TestTemplateEventPreservesNativeObjectWithoutSchemaFallback(t *testing.T) {
	a, _, _ := fixture(t)
	for _, payload := range []string{
		`{"eventtype":"template_card_event","event_key":"sdk-key","task_id":"sdk-task"}`,
		`{"eventtype":"template_card_event","template_card_event":{"event_key":"button","task_id":"task","selected_items":{"selected_item":[]},"future_field":{"value":1}}}`,
	} {
		f := cardEventFrame()
		var body map[string]json.RawMessage
		if err := json.Unmarshal(f.Body, &body); err != nil {
			t.Fatal(err)
		}
		body["event"] = json.RawMessage(payload)
		f.Body, _ = json.Marshal(body)
		e, err := a.decodeEvent(f)
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		if json.Unmarshal([]byte(payload), &want) != nil || json.Unmarshal(e.Interaction.Value, &got) != nil || !reflect.DeepEqual(want, got) {
			t.Fatal(string(e.Interaction.Value))
		}
	}
}

func TestTemplateEventRejectsForeignBotAndReportsUnknownEvent(t *testing.T) {
	a, _, _ := fixture(t)
	f := cardEventFrame()
	f.Body = []byte(strings.Replace(string(f.Body), `"aibotid":"bot"`, `"aibotid":"foreign"`, 1))
	if _, err := a.decodeEvent(f); !errors.Is(err, gateway.ErrUnauthorized) {
		t.Fatal(err)
	}
	f = cardEventFrame()
	f.Body = []byte(strings.Replace(string(f.Body), "template_card_event", "future_event", 1))
	_, err := a.decodeEvent(f)
	var unsupported *gateway.UnsupportedMessageError
	if !errors.As(err, &unsupported) || unsupported.MessageType != "event/future_event" {
		t.Fatal(err)
	}
}
