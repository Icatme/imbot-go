package dingtalk

import (
	"strings"

	"github.com/Icatme/wechatbot-go/gateway"
)

type richElement struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	DownloadCode string `json:"downloadCode"`
	PictureCode  string `json:"pictureDownloadCode"`
	FileName     string `json:"fileName"`
}

func decodeRichText(items []richElement) (string, []gateway.Attachment, []gateway.ContentPart, error) {
	if items == nil {
		return "", nil, nil, gateway.ErrInvalidEvent
	}
	var text strings.Builder
	var attachments []gateway.Attachment
	var parts []gateway.ContentPart
	for _, item := range items {
		kind := item.Type
		if kind == "" && item.Text != "" {
			kind = "text"
		}
		switch kind {
		case "text":
			text.WriteString(item.Text)
			parts = append(parts, gateway.ContentPart{Text: item.Text})
		case "picture", "image", "file", "audio", "voice", "video":
			ref := item.DownloadCode
			if ref == "" {
				ref = item.PictureCode
			}
			if ref == "" {
				return "", nil, nil, gateway.ErrInvalidEvent
			}
			if kind == "picture" {
				kind = "image"
			}
			p := gateway.Attachment{Kind: kind, Ref: ref, Name: item.FileName}
			attachments = append(attachments, p)
			parts = append(parts, gateway.ContentPart{Attachment: &p})
		default:
			return "", nil, nil, &gateway.UnsupportedMessageError{Platform: "dingtalk", MessageType: "richText/" + item.Type}
		}
	}
	return text.String(), attachments, parts, nil
}
