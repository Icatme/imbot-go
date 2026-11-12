package dingtalk

import (
	"slices"
	"testing"

	"github.com/Icatme/imbot-go/receive"
)

func TestRecognizedAudioRetainsVoiceKindWithOrWithoutMedia(t *testing.T) {
	for _, media := range []bool{false, true} {
		a := testAdapter(t)
		content := `"recognition":"transcript"`
		if media {
			content += `,"downloadCode":"private"`
		}
		e, err := a.Decode([]byte(`{"msgId":"m","msgtype":"audio","senderStaffId":"u","senderCorpId":"tenant","conversationType":"1","content":{`+content+`}}`), "delivery")
		if err != nil || !slices.Equal(e.ContentKinds, []receive.Kind{receive.Voice}) || e.Text != "transcript" || (len(e.Attachments) != 0) != media {
			t.Fatalf("media=%t voice semantics lost: %+v error=%v", media, e, err)
		}
	}
}
