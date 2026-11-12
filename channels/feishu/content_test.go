package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Icatme/wechatbot-go/gateway"
)

func messagePayload(typ, content string) []byte {
	var v envelope
	if err := json.Unmarshal(payload(), &v); err != nil {
		panic(err)
	}
	v.Event.Message.Type = typ
	v.Event.Message.Content = content
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

const mixedPost = `{"title":"主题","content":[[{"tag":"text","text":"看图"},{"tag":"img","image_key":"img-1"},{"tag":"text","text":"和文件"},{"tag":"file","file_key":"file-1","file_name":"说明.pdf"}],[{"tag":"a","text":"链接","href":"https://example.com"}]]}`

func TestPostPreservesTextMediaAndOrder(t *testing.T) {
	a := testAdapter(t)
	e, err := a.Decode(messagePayload("post", mixedPost))
	if err != nil {
		t.Fatal(err)
	}
	if e.Text != "主题\n看图和文件\n链接 (https://example.com)" || len(e.Attachments) != 2 || e.Attachments[0].Ref != "img-1" || e.Attachments[1].Name != "说明.pdf" {
		t.Fatalf("unexpected event: %+v", e)
	}
	if len(e.Parts) != 8 || e.Parts[2].Text != "看图" || e.Parts[3].Attachment.Ref != "img-1" || e.Parts[4].Text != "和文件" || e.Parts[5].Attachment.Ref != "file-1" {
		t.Fatalf("incorrect part order: %+v", e.Parts)
	}
	if e.Attachments[0].Scope == nil || *e.Attachments[0].Scope != e.Route || e.Attachments[0].MessageID != "msg" {
		t.Fatalf("missing resource scope: %+v", e.Attachments[0])
	}
}

func TestPostLocalesAndReferenceDoNotInventThread(t *testing.T) {
	a := testAdapter(t)
	var v envelope
	json.Unmarshal(messagePayload("post", `{"en_us":{"title":"Hello","content":[[{"tag":"text","text":"body"}]]}}`), &v)
	v.Event.Message.Root = "parent-message"
	b, _ := json.Marshal(v)
	e, err := a.Decode(b)
	if err != nil || e.Text != "Hello\nbody" || e.ReplyTo != "parent-message" || e.Route.ThreadID != "" {
		t.Fatal(e, err)
	}
}

func TestPostAndUnknownWebhookAdmission(t *testing.T) {
	for _, typ := range []string{"post", "future-format"} {
		t.Run(typ, func(t *testing.T) {
			a := testAdapter(t)
			calls, reports := 0, 0
			a.cfg.OnError = func(err error) {
				var unsupported *gateway.UnsupportedMessageError
				if !errors.As(err, &unsupported) || unsupported.Platform != "feishu" || unsupported.MessageType != typ {
					t.Errorf("incorrect diagnostic: %v", err)
				}
				reports++
			}
			rec := httptest.NewRecorder()
			a.Handler(func(_ context.Context, e gateway.Event) error {
				calls++
				if e.Text == "" || len(e.Attachments) != 2 {
					t.Errorf("incomplete event: %+v", e)
				}
				return nil
			}).ServeHTTP(rec, signed(messagePayload(typ, mixedPost)))
			if rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body.String())
			}
			if typ == "post" && (calls != 1 || reports != 0) || typ != "post" && (calls != 0 || reports != 1) {
				t.Fatal(calls, reports)
			}
		})
	}
}

func TestMalformedPostIsNotSilentlyConsumed(t *testing.T) {
	a := testAdapter(t)
	rec := httptest.NewRecorder()
	a.Handler(func(context.Context, gateway.Event) error { t.Fatal("malformed post reached sink"); return nil }).ServeHTTP(rec, signed(messagePayload("post", `{"content":[[{"tag":"img"}]]}`)))
	if rec.Code != 503 {
		t.Fatal(rec.Code)
	}
}

func TestUnknownTypeDoesNotAssumeKnownContentSchema(t *testing.T) {
	a := testAdapter(t)
	_, err := a.Decode(messagePayload("new-binary-message", "not JSON"))
	var unsupported *gateway.UnsupportedMessageError
	if !errors.As(err, &unsupported) || unsupported.MessageType != "new-binary-message" {
		t.Fatal(err)
	}
}
