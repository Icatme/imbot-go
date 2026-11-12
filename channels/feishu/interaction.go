package feishu

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

// Card callbacks omit chat type and thread ID. Only a known successful send can
// recover the exact route without guessing a new session or calling another API.
// This is local retention, not a claim about Feishu's card lifetime.
func cardRouteKey(tenant, chat, message string) string {
	b, _ := json.Marshal([]string{tenant, chat, message})
	return string(b)
}

func (a *Adapter) rememberCardRoute(r gateway.Route, message string) {
	expires := time.Now().Add(30 * time.Minute)
	a.cardRoutes.Put(cardRouteKey(r.TenantID, r.ChatID, message), r, expires)
	a.replyRoutes.Put(wire.ContextKey(r, message), true, expires)
}

func (a *Adapter) decodeCardAction(raw []byte, v envelope) (gateway.Event, error) {
	var body struct {
		Event struct {
			Operator struct {
				OpenID string `json:"open_id"`
			} `json:"operator"`
			Action  json.RawMessage `json:"action"`
			Context struct {
				MessageID string `json:"open_message_id"`
				ChatID    string `json:"open_chat_id"`
			} `json:"context"`
		} `json:"event"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return gateway.Event{}, err
	}
	c := body.Event
	var action struct {
		Tag string `json:"tag"`
	}
	if json.Unmarshal(c.Action, &action) != nil || v.Header.EventID == "" || v.Header.Tenant == "" || c.Operator.OpenID == "" || action.Tag == "" || c.Context.ChatID == "" || c.Context.MessageID == "" {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	r, ok := a.cardRoutes.Get(cardRouteKey(v.Header.Tenant, c.Context.ChatID, c.Context.MessageID))
	if !ok {
		return gateway.Event{}, gateway.ErrReplyExpired
	}
	e := gateway.Event{
		ID: v.Header.EventID, MessageID: c.Context.MessageID,
		MessageType: "card.action.trigger", Route: r, SenderID: c.Operator.OpenID,
		Time: time.Now(),
		// Preserve the complete action, including form/input/select data. The
		// sibling event.token credential is deliberately not retained here.
		Interaction: &gateway.Interaction{ID: v.Header.EventID, Name: action.Tag, MessageID: c.Context.MessageID, Value: append(json.RawMessage(nil), c.Action...)},
	}
	if n, err := strconv.ParseInt(v.Header.Created, 10, 64); err == nil && n > 0 {
		e.Time = time.UnixMicro(n)
	}
	return e, nil
}
