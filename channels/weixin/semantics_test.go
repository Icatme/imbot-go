package weixin

import (
	"slices"
	"testing"

	"github.com/Icatme/imbot-go"
	"github.com/Icatme/imbot-go/receive"
)

func TestNativeContentKindsPreserveMixedAndTranscriptSemantics(t *testing.T) {
	voice := imbot.MessageItem{Type: imbot.ItemVoice, VoiceItem: &imbot.VoiceItem{Text: "transcript"}}
	text := imbot.MessageItem{Type: imbot.ItemText, TextItem: &imbot.TextItem{Text: "written"}}
	for _, tc := range []struct {
		name  string
		items []imbot.MessageItem
		typ   imbot.ContentType
		want  []receive.Kind
	}{
		{"voice_only", []imbot.MessageItem{voice}, imbot.ContentVoice, []receive.Kind{receive.Voice}},
		{"mixed_primary_text", []imbot.MessageItem{text, voice, voice}, imbot.ContentText, []receive.Kind{receive.Text, receive.Voice}},
		{"mixed_primary_voice", []imbot.MessageItem{voice, text}, imbot.ContentVoice, []receive.Kind{receive.Voice, receive.Text}},
		{"fallback_voice", nil, imbot.ContentVoice, []receive.Kind{receive.Voice}},
		{"mixed_tool_call", []imbot.MessageItem{text, {Type: imbot.ItemToolCallStart}}, imbot.ContentText, []receive.Kind{receive.Text, receive.ToolCall}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := fixture(t)
			m := incoming()
			m.Type, m.Text, m.Raw.ItemList = tc.typ, "transcript", tc.items
			e, err := a.normalize(m)
			if err != nil || !slices.Equal(e.ContentKinds, tc.want) {
				t.Fatalf("kinds=%v want=%v error=%v", e.ContentKinds, tc.want, err)
			}
		})
	}
}

func TestNativeQuoteKindsKeepTranscriptAsVoice(t *testing.T) {
	a, _ := fixture(t)
	m := incoming()
	m.QuotedMessage = &imbot.QuotedMessage{MessageID: "old", Type: imbot.ContentVoice, Text: "quoted transcript"}
	e, err := a.normalize(m)
	if err != nil || e.Quote == nil || !slices.Equal(e.Quote.ContentKinds, []receive.Kind{receive.Voice}) || e.Quote.Text != "quoted transcript" || len(e.Quote.Attachments) != 0 || !slices.Equal(e.ContentKinds, []receive.Kind{receive.Text}) {
		t.Fatalf("quote lost voice semantics: %+v error=%v", e, err)
	}
}

func TestNativeQuoteKindsPreserveCachedMixedSemantics(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quote imbot.QuotedMessage
		want  []receive.Kind
	}{
		{
			name:  "file_caption",
			quote: imbot.QuotedMessage{Type: imbot.ContentFile, Text: "caption", ContentKinds: []receive.Kind{receive.Text, receive.File}},
			want:  []receive.Kind{receive.Text, receive.File},
		},
		{
			name:  "image_with_voice_transcript",
			quote: imbot.QuotedMessage{Type: imbot.ContentImage, Text: "transcript", ContentKinds: []receive.Kind{receive.Voice, receive.Image}},
			want:  []receive.Kind{receive.Voice, receive.Image},
		},
		{
			name:  "fallback_file_caption",
			quote: imbot.QuotedMessage{Type: imbot.ContentFile, Text: "caption"},
			want:  []receive.Kind{receive.File, receive.Text},
		},
		{
			name:  "fallback_image_caption",
			quote: imbot.QuotedMessage{Type: imbot.ContentImage, Text: "caption"},
			want:  []receive.Kind{receive.Image, receive.Text},
		},
		{
			name:  "explicit_kinds_still_include_media",
			quote: imbot.QuotedMessage{Type: imbot.ContentImage, Text: "transcript", ContentKinds: []receive.Kind{receive.Voice}, Media: &imbot.QuotedMedia{Type: imbot.ContentImage}},
			want:  []receive.Kind{receive.Voice, receive.Image},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := fixture(t)
			m := incoming()
			m.QuotedMessage = &tc.quote
			e, err := a.normalize(m)
			if err != nil || e.Quote == nil || !slices.Equal(e.Quote.ContentKinds, tc.want) {
				t.Fatalf("quote=%+v want kinds=%v error=%v", e.Quote, tc.want, err)
			}
			if len(tc.quote.ContentKinds) > 0 {
				e.Quote.ContentKinds[0] = receive.ToolCall
				if tc.quote.ContentKinds[0] != tc.want[0] {
					t.Fatalf("normalized quote aliases native kinds: %v", tc.quote.ContentKinds)
				}
			}
		})
	}
}
