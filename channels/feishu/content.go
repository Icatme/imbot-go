package feishu

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Icatme/wechatbot-go/gateway"
)

type postElement struct {
	Tag       string `json:"tag"`
	Text      string `json:"text"`
	Href      string `json:"href"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	ImageKey  string `json:"image_key"`
	FileKey   string `json:"file_key"`
	FileName  string `json:"file_name"`
	EmojiType string `json:"emoji_type"`
}

// decodePost accepts the direct event shape and the locale-keyed API shape.
// It preserves paragraph and attachment order, without fetching remote resources.
func decodePost(raw []byte) (string, []gateway.Attachment, []gateway.ContentPart, error) {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		return "", nil, nil, err
	}
	if _, ok := shape["content"]; !ok {
		var locale string
		for _, preferred := range []string{"zh_cn", "en_us", "ja_jp"} {
			if _, ok := shape[preferred]; ok {
				locale = preferred
				break
			}
		}
		if locale == "" {
			keys := make([]string, 0, len(shape))
			for key := range shape {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if len(keys) > 0 {
				locale = keys[0]
			}
		}
		if locale == "" {
			return "", nil, nil, gateway.ErrInvalidEvent
		}
		raw = shape[locale]
	}
	var post struct {
		Title   string          `json:"title"`
		Content [][]postElement `json:"content"`
	}
	if err := json.Unmarshal(raw, &post); err != nil {
		return "", nil, nil, err
	}
	if post.Content == nil {
		return "", nil, nil, gateway.ErrInvalidEvent
	}
	var lines []string
	if post.Title != "" {
		lines = append(lines, post.Title)
	}
	var attachments []gateway.Attachment
	var parts []gateway.ContentPart
	addText := func(text string) {
		if text != "" {
			parts = append(parts, gateway.ContentPart{Text: text})
		}
	}
	addAttachment := func(p gateway.Attachment) {
		attachments = append(attachments, p)
		parts = append(parts, gateway.ContentPart{Attachment: &p})
	}
	if post.Title != "" {
		addText(post.Title)
		addText("\n")
	}
	for rowIndex, row := range post.Content {
		if rowIndex > 0 {
			addText("\n")
		}
		var line strings.Builder
		for _, p := range row {
			switch p.Tag {
			case "text", "md", "code_block":
				line.WriteString(p.Text)
				addText(p.Text)
			case "a":
				start := line.Len()
				line.WriteString(p.Text)
				if p.Href != "" && p.Href != p.Text {
					if p.Text != "" {
						line.WriteString(" (")
					}
					line.WriteString(p.Href)
					if p.Text != "" {
						line.WriteByte(')')
					}
				}
				addText(line.String()[start:])
			case "at":
				start := line.Len()
				line.WriteByte('@')
				if p.UserName != "" {
					line.WriteString(p.UserName)
				} else {
					line.WriteString(p.UserID)
				}
				addText(line.String()[start:])
			case "img":
				if p.ImageKey == "" {
					return "", nil, nil, gateway.ErrInvalidEvent
				}
				addAttachment(gateway.Attachment{Kind: "image", Ref: p.ImageKey})
			case "media", "file", "audio":
				if p.FileKey == "" {
					return "", nil, nil, gateway.ErrInvalidEvent
				}
				kind := p.Tag
				if kind == "media" {
					kind = "video"
				}
				addAttachment(gateway.Attachment{Kind: kind, Ref: p.FileKey, Name: p.FileName})
			case "hr":
				line.WriteString("\n---\n")
				addText("\n---\n")
			case "emotion":
				if p.EmojiType == "" {
					return "", nil, nil, gateway.ErrInvalidEvent
				}
				label := "[" + p.EmojiType + "]"
				line.WriteString(label)
				addText(label)
			default:
				return "", nil, nil, &gateway.UnsupportedMessageError{Platform: "feishu", MessageType: "post/" + p.Tag}
			}
		}
		lines = append(lines, line.String())
	}
	return strings.Join(lines, "\n"), attachments, parts, nil
}

func outboundContent(m gateway.Message) (string, []byte, error) {
	if len(m.Card) > 0 {
		if !json.Valid(m.Card) || m.Text != "" || len(m.Attachments) > 0 || m.Format == "markdown" {
			return "", nil, gateway.ErrUnsupported
		}
		return "interactive", m.Card, nil
	}
	if len(m.Attachments) == 0 && m.Format != "markdown" {
		b, err := json.Marshal(map[string]string{"text": m.Text})
		return "text", b, err
	}
	if len(m.Attachments) == 1 && m.Text == "" && m.Format != "markdown" {
		p := m.Attachments[0]
		typ, key := "file", "file_key"
		switch p.Kind {
		case "image":
			typ, key = "image", "image_key"
		case "file":
		case "audio", "voice":
			typ = "audio"
		case "video", "media":
			typ = "media"
		default:
			return "", nil, gateway.ErrUnsupported
		}
		b, err := json.Marshal(map[string]string{key: p.Ref})
		return typ, b, err
	}
	var rows [][]map[string]string
	if m.Text != "" {
		tag := "text"
		if m.Format == "markdown" {
			tag = "md"
		}
		rows = append(rows, []map[string]string{{"tag": tag, "text": m.Text}})
	}
	for _, p := range m.Attachments {
		switch p.Kind {
		case "image":
			rows = append(rows, []map[string]string{{"tag": "img", "image_key": p.Ref}})
		case "video", "media":
			rows = append(rows, []map[string]string{{"tag": "media", "file_key": p.Ref}})
		default:
			return "", nil, gateway.ErrUnsupported // post has no general file/audio node
		}
	}
	b, err := json.Marshal(map[string]any{"zh_cn": map[string]any{"content": rows}})
	return "post", b, err
}
