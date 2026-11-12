package wecomapp

import (
	"slices"
	"strings"
	"testing"

	"github.com/Icatme/imbot-go/receive"
)

func TestRecognizedVoiceIsNotIndependentText(t *testing.T) {
	a := fixture(t)
	xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>voice</MsgType><MediaId>voice</MediaId><Recognition>transcript</Recognition>", 1)
	e, err := a.Decode([]byte(xml))
	if err != nil || !slices.Equal(e.ContentKinds, []receive.Kind{receive.Voice}) || e.Text != "transcript" || len(e.Attachments) != 1 || len(e.Parts) != 2 {
		t.Fatalf("voice semantics lost: %+v error=%v", e, err)
	}
}
