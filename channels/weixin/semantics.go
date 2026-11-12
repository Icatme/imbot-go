package weixin

import (
	"slices"

	"github.com/Icatme/imbot-go"
	"github.com/Icatme/imbot-go/receive"
)

func addContentKind(kinds []receive.Kind, kind receive.Kind) []receive.Kind {
	if kind != "" && !slices.Contains(kinds, kind) {
		return append(kinds, kind)
	}
	return kinds
}

func nativeKind(kind imbot.ContentType) receive.Kind {
	switch kind {
	case imbot.ContentText:
		return receive.Text
	case imbot.ContentImage:
		return receive.Image
	case imbot.ContentVoice:
		return receive.Voice
	case imbot.ContentVideo:
		return receive.Video
	case imbot.ContentFile:
		return receive.File
	case imbot.ContentToolCallStart, imbot.ContentToolCallResult:
		return receive.ToolCall
	default:
		return ""
	}
}

// The primary native Type cannot describe a mixed message. Inspect every raw
// item before its voice transcription is flattened into Text and Parts.
func nativeContentKinds(m *imbot.IncomingMessage) []receive.Kind {
	var kinds []receive.Kind
	for _, item := range m.Raw.ItemList {
		var kind receive.Kind
		switch item.Type {
		case imbot.ItemText:
			kind = receive.Text
		case imbot.ItemImage:
			kind = receive.Image
		case imbot.ItemVoice:
			kind = receive.Voice
		case imbot.ItemVideo:
			kind = receive.Video
		case imbot.ItemFile:
			kind = receive.File
		case imbot.ItemToolCallStart, imbot.ItemToolCallResult:
			kind = receive.ToolCall
		}
		kinds = addContentKind(kinds, kind)
	}
	for _, media := range []struct {
		count int
		kind  receive.Kind
	}{{len(m.Images), receive.Image}, {len(m.Voices), receive.Voice}, {len(m.Files), receive.File}, {len(m.Videos), receive.Video}} {
		if media.count > 0 {
			kinds = addContentKind(kinds, media.kind)
		}
	}
	if len(m.Raw.ItemList) == 0 {
		kinds = addContentKind(kinds, nativeKind(m.Type))
		if len(kinds) == 0 && m.Text != "" {
			kinds = append(kinds, receive.Text)
		}
	}
	return kinds
}

func nativeQuoteKinds(q *imbot.QuotedMessage) []receive.Kind {
	kinds := slices.Clone(q.ContentKinds)
	if len(kinds) == 0 {
		kinds = addContentKind(kinds, nativeKind(q.Type))
		if q.Text != "" && q.Type != imbot.ContentVoice {
			kinds = addContentKind(kinds, receive.Text)
		}
	}
	if q.Media != nil {
		kinds = addContentKind(kinds, nativeKind(q.Media.Type))
	}
	return kinds
}
