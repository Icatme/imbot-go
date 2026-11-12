package qqbot

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

// Interaction IDs cannot be used as QQ passive-reply message IDs. Preserve that
// distinction after the short acknowledgement cache has expired or been evicted.
const interactionMessagePrefix = "qqbot:interaction:"

func (a *Adapter) isInteractionReply(r gateway.Route, id string) bool {
	if strings.HasPrefix(id, interactionMessagePrefix) {
		return true
	}
	_, ok := a.interactions.Get(wire.ContextKey(r, id))
	return ok
}

func (a *Adapter) decodeInteraction(v envelope) (gateway.Event, error) {
	var in struct {
		ID       string `json:"id"`
		Type     int    `json:"type"`
		ChatType int    `json:"chat_type"`
		User     string `json:"user_openid"`
		Group    string `json:"group_openid"`
		Member   string `json:"group_member_openid"`
		Data     struct {
			Type     int `json:"type"`
			Resolved struct {
				ButtonID   string `json:"button_id"`
				ButtonData string `json:"button_data"`
				UserID     string `json:"user_id"`
				MessageID  string `json:"message_id"`
			} `json:"resolved"`
		} `json:"data"`
	}
	if json.Unmarshal(v.Data, &in) != nil || in.ID == "" {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	// Official BotGo dto/interaction.go: command=2, inline keyboard click=11.
	// Fields with the same names in future interaction types are not buttons.
	if in.Type != 2 || in.Data.Type != 11 {
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "qqbot", MessageType: "interaction:" + strconv.Itoa(in.Type) + "/" + strconv.Itoa(in.Data.Type)}
	}
	r := gateway.Route{Account: a.Account(), TenantID: a.cfg.AppID}
	sender := ""
	switch in.ChatType {
	case 1:
		r.Kind = "group"
		r.ChatID = in.Group
		sender = in.Member
	case 2:
		r.Kind = "direct"
		r.ChatID = in.User
		sender = in.User
	default:
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "qqbot", MessageType: "interaction_chat:" + strconv.Itoa(in.ChatType)}
	}
	if sender == "" || r.Validate() != nil || (in.Data.Resolved.UserID != "" && in.Data.Resolved.UserID != sender) {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if in.Data.Resolved.ButtonID == "" {
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "qqbot", MessageType: "interaction:" + strconv.Itoa(in.Type)}
	}
	value, _ := json.Marshal(in.Data.Resolved.ButtonData)
	e := gateway.Event{ID: in.ID, MessageID: interactionMessagePrefix + in.ID, MessageType: "INTERACTION_CREATE", SenderID: sender, Route: r, Time: time.Now(), Native: json.RawMessage(bytes.Clone(v.Data)), Interaction: &gateway.Interaction{ID: in.ID, Name: in.Data.Resolved.ButtonID, MessageID: in.Data.Resolved.MessageID, Value: value}}
	a.interactions.Put(wire.ContextKey(r, in.ID), true, time.Now().Add(5*time.Second))
	return e, nil
}

// AcknowledgeInteraction sends the QQ-specific button acknowledgement. Call it
// after the normal Gateway Authorize/Sink decision, within QQ's five-second
// interaction window. Webhook receipt ACK and this operation are separate.
// Codes: 0 success, 1 failure, 2 too frequent, 3 duplicate, 4 denied, 5 admin-only.
func (a *Adapter) AcknowledgeInteraction(ctx context.Context, r gateway.Route, id string, code int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.mediaRoute(r); err != nil {
		return err
	}
	if id == "" || code < 0 || code > 5 {
		return gateway.ErrInvalidEvent
	}
	if _, ok := a.interactions.Get(wire.ContextKey(r, id)); !ok {
		return gateway.ErrReplyExpired
	}
	return a.operationJSON(ctx, http.MethodPut, "/interactions/"+wire.PathID(id), map[string]int{"code": code}, nil)
}

// Message.Card carries a native QQ keyboard object, not an invented cross-platform
// card template. Every permission is explicit. Only inline content is accepted;
// application-owned keyboard template IDs require a separate platform contract.
func validateKeyboard(raw json.RawMessage) error {
	var keyboard struct {
		Content struct {
			Rows []struct {
				Buttons []struct {
					ID      string `json:"id"`
					GroupID string `json:"group_id,omitempty"`
					Render  struct {
						Label        string `json:"label"`
						VisitedLabel string `json:"visited_label"`
						Style        *int   `json:"style"`
					} `json:"render_data"`
					Action struct {
						Type       *int   `json:"type"`
						Data       string `json:"data"`
						ClickLimit *int   `json:"click_limit,omitempty"`
						Permission struct {
							Type *int `json:"type"`
						} `json:"permission"`
					} `json:"action"`
				} `json:"buttons"`
			} `json:"rows"`
		} `json:"content"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&keyboard) != nil || len(keyboard.Content.Rows) == 0 {
		return gateway.ErrInvalidEvent
	}
	if dec.Decode(new(any)) != io.EOF {
		return gateway.ErrInvalidEvent
	}
	ids := map[string]bool{}
	for _, row := range keyboard.Content.Rows {
		if len(row.Buttons) == 0 {
			return gateway.ErrInvalidEvent
		}
		for _, b := range row.Buttons {
			if b.ID == "" || ids[b.ID] || b.Render.Label == "" || b.Render.VisitedLabel == "" || b.Render.Style == nil || (*b.Render.Style != 0 && *b.Render.Style != 1) || b.Action.Type == nil || *b.Action.Type < 0 || *b.Action.Type > 2 || b.Action.Data == "" || b.Action.Permission.Type == nil || (b.Action.ClickLimit != nil && *b.Action.ClickLimit < 1) {
				return gateway.ErrInvalidEvent
			}
			// No default all-users permission. Restrict this typed subset to the
			// explicit admin/all-user policies; named-role/user lists are not dropped.
			if *b.Action.Permission.Type != 1 && *b.Action.Permission.Type != 2 {
				return gateway.ErrUnsupported
			}
			ids[b.ID] = true
		}
	}
	return nil
}
