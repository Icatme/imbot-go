package wecom

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
	"github.com/Icatme/imbot-go/receive"
)

type content struct {
	Content string `json:"content"`
	URL     string `json:"url"`
	AESKey  string `json:"aeskey"`
	Base64  string `json:"base64"`
	Name    string `json:"filename"`
}
type messageContent struct {
	Type  string  `json:"msgtype"`
	ID    string  `json:"msgid"`
	Text  content `json:"text"`
	Voice content `json:"voice"`
	Image content `json:"image"`
	File  content `json:"file"`
	Video content `json:"video"`
	Mixed struct {
		Items []messageContent `json:"msg_item"`
	} `json:"mixed"`
	AppMsg struct {
		Title string   `json:"title"`
		File  *content `json:"file"`
		Image *content `json:"image"`
	} `json:"appmsg"`
}
type incoming struct {
	messageContent
	BotID    string `json:"aibotid"`
	ChatID   string `json:"chatid"`
	ChatType string `json:"chattype"`
	Created  int64  `json:"create_time"`
	From     struct {
		ID string `json:"userid"`
	} `json:"from"`
	Quote *messageContent `json:"quote"`
}

// decodeEvent keeps interaction admission on the same Gateway path as messages.
// An event's own ID is not a text-reply anchor and is never added to replies.
func (a *Adapter) decodeEvent(f frame) (gateway.Event, error) {
	var v struct {
		incoming
		Event json.RawMessage `json:"event"`
	}
	if json.Unmarshal(f.Body, &v) != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if v.BotID != "" && v.BotID != a.cfg.BotID {
		return gateway.Event{}, gateway.ErrUnauthorized
	}
	var event struct {
		Type string `json:"eventtype"`
	}
	if json.Unmarshal(v.Event, &event) != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if event.Type == "disconnected_event" {
		return gateway.Event{}, &gateway.APIError{Platform: "wecom", Code: "connection_taken_over"}
	}
	if event.Type != "template_card_event" {
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "wecom", MessageType: "event/" + event.Type}
	}
	if v.Type != "event" || v.ID == "" || v.From.ID == "" || f.Headers["req_id"] == "" {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	r := gateway.Route{Account: a.Account(), TenantID: a.cfg.TenantID, ChatID: v.ChatID, Kind: "group"}
	switch v.ChatType {
	case "single":
		r.Kind = "direct"
		r.ChatID = v.From.ID
	case "group":
	case "":
		if v.ChatID != "" {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
		r.Kind = "direct"
		r.ChatID = v.From.ID
	default:
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if r.Validate() != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	// Keep the authenticated native event object, including nested form choices.
	// Flattening fields according to a partial SDK type loses real card payloads.
	value := append(json.RawMessage(nil), v.Event...)
	e := gateway.Event{ID: v.ID, MessageID: v.ID, MessageType: "event/template_card_event", Route: r, SenderID: v.From.ID, Time: time.Now(), Native: json.RawMessage(append([]byte(nil), f.Body...)), Interaction: &gateway.Interaction{ID: v.ID, Name: event.Type, Value: value}}
	if v.Created > 0 {
		e.Time = time.Unix(v.Created, 0)
	}
	return e, nil
}

func (a *Adapter) decode(f frame, s *session) (gateway.Event, error) {
	var v incoming
	if json.Unmarshal(f.Body, &v) != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if v.BotID != "" && v.BotID != a.cfg.BotID {
		return gateway.Event{}, gateway.ErrUnauthorized
	}
	e := gateway.Event{ID: v.ID, MessageID: v.ID, MessageType: v.Type, SenderID: v.From.ID, Time: time.Now(), Route: gateway.Route{Account: a.Account(), TenantID: a.cfg.TenantID, ChatID: v.ChatID, Kind: "group"}, Native: json.RawMessage(append([]byte(nil), f.Body...))}
	switch v.ChatType {
	case "single":
		e.Route.Kind = "direct"
		e.Route.ChatID = v.From.ID
	case "group":
	default:
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "wecom", MessageType: "chat/" + v.ChatType}
	}
	if e.ID == "" || e.SenderID == "" || e.Route.Validate() != nil || f.Headers["req_id"] == "" {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if v.Created > 0 {
		e.Time = time.Unix(v.Created, 0)
	}
	text, attachments, parts, err := a.parseContent(e.Route, v.ID, "body", v.messageContent)
	if err != nil {
		return gateway.Event{}, err
	}
	e.Text, e.Attachments, e.Parts = text, attachments, parts
	if v.Type == "voice" {
		e.ContentKinds = []receive.Kind{receive.Voice}
	}
	if v.Quote != nil {
		text, attachments, _, err = a.parseContent(e.Route, v.ID, "quote", *v.Quote)
		if err != nil {
			return gateway.Event{}, err
		}
		e.Quote = &gateway.Quote{MessageID: v.Quote.ID, Text: text, Attachments: attachments}
		if v.Quote.Type == "voice" {
			e.Quote.ContentKinds = []receive.Kind{receive.Voice}
		}
		e.ReplyTo = v.Quote.ID
	}
	a.replies.Put(wire.ContextKey(e.Route, e.MessageID), replyContext{s, f.Headers["req_id"]}, time.Now().Add(5*time.Minute))
	return e, nil
}

func (a *Adapter) parseContent(route gateway.Route, messageID, path string, v messageContent) (string, []gateway.Attachment, []gateway.ContentPart, error) {
	var texts []string
	var attachments []gateway.Attachment
	var parts []gateway.ContentPart
	addText := func(text string) {
		if text != "" {
			texts = append(texts, text)
			parts = append(parts, gateway.ContentPart{Text: text})
		}
	}
	addMedia := func(kind string, c content, position string) error {
		if strings.TrimSpace(c.URL) == "" && strings.TrimSpace(c.Base64) == "" {
			return gateway.ErrInvalidEvent
		}
		// The platform's inbound media URLs are valid for only five minutes.
		// Do not advertise the longer local cache retention used for uploads.
		expires := time.Now().Add(5 * time.Minute)
		ref := inboundRefPrefix + messageID + ":" + position + ":" + kind
		attachment := gateway.Attachment{Kind: kind, Ref: ref, Name: c.Name, MessageID: messageID, Scope: &route, ExpiresAt: expires}
		attachments = append(attachments, attachment)
		parts = append(parts, gateway.ContentPart{Attachment: &attachment})
		// Inline payloads need a much smaller cache than URL/key locators. A
		// callback frame is bounded to 2 MiB; 32 entries bound retained payloads.
		inline := c.Base64 != ""
		if inline {
			a.inlineMedia.Put(wire.ContextKey(route, ref), c.Base64, expires)
			c.Base64 = ""
		}
		a.downloads.Put(wire.ContextKey(route, ref), mediaSource{content: c, kind: kind, inline: inline}, expires)
		return nil
	}
	switch v.Type {
	case "text":
		addText(v.Text.Content)
	case "voice":
		addText(v.Voice.Content)
		if v.Voice.URL != "" || v.Voice.Base64 != "" {
			if err := addMedia("voice", v.Voice, path); err != nil {
				return "", nil, nil, err
			}
		}
	case "image":
		if err := addMedia("image", v.Image, path); err != nil {
			return "", nil, nil, err
		}
	case "file":
		if err := addMedia("file", v.File, path); err != nil {
			return "", nil, nil, err
		}
	case "video":
		if err := addMedia("video", v.Video, path); err != nil {
			return "", nil, nil, err
		}
	case "appmsg":
		if v.AppMsg.File != nil {
			c := *v.AppMsg.File
			if c.Name == "" {
				c.Name = v.AppMsg.Title
			}
			if err := addMedia("file", c, path+":file"); err != nil {
				return "", nil, nil, err
			}
		}
		if v.AppMsg.Image != nil {
			if err := addMedia("image", *v.AppMsg.Image, path+":image"); err != nil {
				return "", nil, nil, err
			}
		}
		if v.AppMsg.File == nil && v.AppMsg.Image == nil {
			return "", nil, nil, &gateway.UnsupportedMessageError{Platform: "wecom", MessageType: "appmsg"}
		}
	case "mixed":
		for i, item := range v.Mixed.Items {
			switch item.Type {
			case "text":
				addText(item.Text.Content)
			case "image":
				if err := addMedia("image", item.Image, fmt.Sprintf("%s:%d", path, i)); err != nil {
					return "", nil, nil, err
				}
			default:
				return "", nil, nil, &gateway.UnsupportedMessageError{Platform: "wecom", MessageType: "mixed/" + item.Type}
			}
		}
	default:
		return "", nil, nil, &gateway.UnsupportedMessageError{Platform: "wecom", MessageType: v.Type}
	}
	return strings.Join(texts, "\n"), attachments, parts, nil
}
