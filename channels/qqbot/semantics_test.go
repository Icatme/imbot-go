package qqbot

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Icatme/imbot-go/receive"
)

func TestASRContentKindsDistinguishWrittenTextAndQuotedVoice(t *testing.T) {
	for _, written := range []string{"", "written"} {
		a := fixture(t)
		raw := `{"id":"m","author":{"user_openid":"user"},"content":"` + written + `","message_type":103,"attachments":[{"content_type":"audio/silk","url":"https://gchat.qpic.cn/audio","asr_refer_text":"transcript"}],"msg_elements":[{"attachments":[{"content_type":"voice","url":"https://gchat.qpic.cn/quoted","asr_refer_text":"quoted transcript"}]}]}`
		e, err := a.decode(envelope{Type: "C2C_MESSAGE_CREATE", Data: json.RawMessage(raw)})
		want := []receive.Kind{receive.Voice}
		if written != "" {
			want = []receive.Kind{receive.Text, receive.Voice}
		}
		if err != nil || !slices.Equal(e.ContentKinds, want) || e.Quote == nil || !slices.Equal(e.Quote.ContentKinds, []receive.Kind{receive.Voice}) || e.Quote.Text != "quoted transcript" {
			t.Fatalf("written=%q event=%+v error=%v", written, e, err)
		}
	}
}
