package wecom

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Icatme/imbot-go/receive"
)

func TestVoiceContentKindsSurviveTranscriptNormalization(t *testing.T) {
	a, s, _ := fixture(t)
	f := pushed()
	f.Body = json.RawMessage(`{"msgid":"m","chattype":"single","from":{"userid":"u"},"msgtype":"voice","voice":{"content":"transcript"},"quote":{"msgtype":"voice","voice":{"content":"quoted transcript"}}}`)
	e, err := a.decode(f, s)
	if err != nil || !slices.Equal(e.ContentKinds, []receive.Kind{receive.Voice}) || e.Text != "transcript" || len(e.Attachments) != 0 {
		t.Fatalf("voice semantics lost: %+v error=%v", e, err)
	}
	if e.Quote == nil || !slices.Equal(e.Quote.ContentKinds, []receive.Kind{receive.Voice}) || e.Quote.Text != "quoted transcript" || len(e.Quote.Attachments) != 0 {
		t.Fatalf("quoted voice semantics lost: %+v", e.Quote)
	}
}
