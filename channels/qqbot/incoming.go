package qqbot

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

type incomingAttachment struct {
	Type     string `json:"content_type"`
	Name     string `json:"filename"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
	VoiceWAV string `json:"voice_wav_url"`
	ASRText  string `json:"asr_refer_text"`
}

type incomingElement struct {
	Content     string               `json:"content"`
	Attachments []incomingAttachment `json:"attachments"`
}

type incoming struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	Timestamp   string `json:"timestamp"`
	Group       string `json:"group_openid"`
	MessageType int    `json:"message_type"`
	Author      struct {
		User   string `json:"user_openid"`
		Member string `json:"member_openid"`
		Bot    bool   `json:"bot"`
	} `json:"author"`
	Attachments []incomingAttachment `json:"attachments"`
	Elements    []incomingElement    `json:"msg_elements"`
}

func (a *Adapter) decode(v envelope) (gateway.Event, error) {
	if v.Type == "INTERACTION_CREATE" {
		return a.decodeInteraction(v)
	}
	if v.Type != "C2C_MESSAGE_CREATE" && v.Type != "GROUP_AT_MESSAGE_CREATE" {
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "qqbot", MessageType: v.Type}
	}
	var m incoming
	if json.Unmarshal(v.Data, &m) != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if m.MessageType != 0 && m.MessageType != 103 {
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "qqbot", MessageType: strconv.Itoa(m.MessageType)}
	}
	sender, kind := m.Author.User, "direct"
	chat := sender
	if v.Type == "GROUP_AT_MESSAGE_CREATE" {
		sender, chat, kind = m.Author.Member, m.Group, "group"
	}
	e := gateway.Event{ID: v.ID, MessageID: m.ID, MessageType: v.Type, SenderID: sender, SenderIsBot: m.Author.Bot, Text: strings.TrimSpace(m.Content), Mentioned: kind == "group", Route: gateway.Route{Account: a.Account(), TenantID: a.cfg.AppID, ChatID: chat, Kind: kind}, Time: time.Now(), Native: json.RawMessage(append([]byte(nil), v.Data...))}
	if e.ID == "" {
		e.ID = m.ID
	}
	if e.ID == "" || e.MessageID == "" || sender == "" || e.Route.Validate() != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if ts, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil {
		e.Time = ts
	}
	if e.Text != "" {
		e.Parts = append(e.Parts, gateway.ContentPart{Text: e.Text})
	}
	for i, p := range m.Attachments {
		attachment, err := a.rememberAttachment(e.Route, e.MessageID, "current:"+strconv.Itoa(i), p)
		if err != nil {
			return gateway.Event{}, err
		}
		e.Attachments = append(e.Attachments, attachment)
		e.Parts = append(e.Parts, gateway.ContentPart{Attachment: &attachment})
		if transcript := strings.TrimSpace(p.ASRText); transcript != "" {
			if e.Text != "" {
				e.Text += "\n"
			}
			e.Text += transcript
			e.Parts = append(e.Parts, gateway.ContentPart{Text: transcript})
		}
	}
	if m.MessageType == 103 {
		if len(m.Elements) == 0 {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
		e.Quote = &gateway.Quote{}
		var text []string
		for i, element := range m.Elements {
			if element.Content != "" {
				text = append(text, element.Content)
			}
			for j, p := range element.Attachments {
				attachment, err := a.rememberAttachment(e.Route, e.MessageID, "quote:"+strconv.Itoa(i)+":"+strconv.Itoa(j), p)
				if err != nil {
					return gateway.Event{}, err
				}
				e.Quote.Attachments = append(e.Quote.Attachments, attachment)
				if transcript := strings.TrimSpace(p.ASRText); transcript != "" {
					text = append(text, transcript)
				}
			}
		}
		e.Quote.Text = strings.Join(text, "\n")
		if e.Quote.Text == "" && len(e.Quote.Attachments) == 0 {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
	}
	if e.Text == "" && len(e.Attachments) == 0 && e.Quote == nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	return e, nil
}

func (a *Adapter) rememberAttachment(r gateway.Route, messageID, position string, p incomingAttachment) (gateway.Attachment, error) {
	if p.Size < 0 || p.URL == "" {
		return gateway.Attachment{}, gateway.ErrInvalidEvent
	}
	if strings.HasPrefix(p.URL, "//") {
		p.URL = "https:" + p.URL
	}
	if strings.HasPrefix(p.VoiceWAV, "//") {
		p.VoiceWAV = "https:" + p.VoiceWAV
	}
	kind := attachmentKind(p.Type)
	ref := inboundRefPrefix + messageID + ":" + position
	expires := time.Now().Add(30 * time.Minute)
	a.media.Put(wire.ContextKey(r, ref), p, expires)
	return gateway.Attachment{Kind: kind, Ref: ref, Name: p.Name, MIME: p.Type, Size: p.Size, MessageID: messageID, Scope: &r, ExpiresAt: expires}, nil
}

func attachmentKind(mediaType string) string {
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return "image"
	case mediaType == "voice", strings.HasPrefix(mediaType, "audio/"):
		return "voice"
	case strings.HasPrefix(mediaType, "video/"):
		return "video"
	default:
		return "file"
	}
}
