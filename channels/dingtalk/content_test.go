package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Icatme/imbot-go/gateway"
)

func richPayload(typ string) []byte {
	return []byte(`{"msgId":"m","msgtype":"` + typ + `","senderStaffId":"u","senderCorpId":"tenant","conversationId":"c","conversationType":"1","content":{"richText":[{"text":"看图"},{"type":"picture","pictureDownloadCode":"secret-picture"},{"text":"再看文件"},{"type":"file","downloadCode":"secret-file","fileName":"说明.pdf"}]}}`)
}
func TestRichTextPreservesOrderAndHidesDownloadCodes(t *testing.T) {
	a := testAdapter(t)
	e, err := a.Decode(richPayload("richText"), "delivery")
	if err != nil || e.Text != "看图再看文件" || len(e.Attachments) != 2 || len(e.Parts) != 4 {
		t.Fatal(e, err)
	}
	if e.Parts[0].Text != "看图" || e.Parts[1].Attachment.Ref != "m:0" || e.Parts[2].Text != "再看文件" || e.Parts[3].Attachment.Name != "说明.pdf" {
		t.Fatalf("incorrect parts: %+v", e.Parts)
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "secret-") {
		t.Fatal("download credentials leaked")
	}
}
func TestRichTextAndUnknownStreamACK(t *testing.T) {
	for _, typ := range []string{"richText", "future-format"} {
		t.Run(typ, func(t *testing.T) {
			a := testAdapter(t)
			calls, reports := 0, 0
			a.cfg.OnError = func(err error) {
				var unsupported *gateway.UnsupportedMessageError
				if !errors.As(err, &unsupported) || unsupported.Platform != "dingtalk" || unsupported.MessageType != typ {
					t.Errorf("incorrect diagnostic: %v", err)
				}
				reports++
			}
			ack, _ := a.handle(context.Background(), streamFrame{Type: "CALLBACK", Headers: map[string]string{"topic": topic, "messageId": "delivery"}, Data: string(richPayload(typ))}, func(context.Context, gateway.Event) error { calls++; return nil })
			if ack.Code != 200 || typ == "richText" && (calls != 1 || reports != 0) || typ != "richText" && (calls != 0 || reports != 1) {
				t.Fatal(ack, calls, reports)
			}
		})
	}
}
func TestRichTextAdmissionFailureIsRetriable(t *testing.T) {
	a := testAdapter(t)
	ack, _ := a.handle(context.Background(), streamFrame{Type: "CALLBACK", Headers: map[string]string{"topic": topic}, Data: string(richPayload("richText"))}, func(context.Context, gateway.Event) error { return errors.New("inbox unavailable") })
	if ack.Code != 500 {
		t.Fatal(ack)
	}
}
