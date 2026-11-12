package weixin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	wechatbot "github.com/Icatme/wechatbot-go"
	"github.com/Icatme/wechatbot-go/gateway"
)

func TestDownloadSelectsSpecificMediaAndProtectsLocator(t *testing.T) {
	a, f := fixture(t)
	m := incoming()
	m.Files = []wechatbot.FileContent{{FileName: "first", Media: &wechatbot.CDNMedia{EncryptQueryParam: "first-secret"}}, {FileName: "second", Media: &wechatbot.CDNMedia{EncryptQueryParam: "second-secret"}}}
	e, err := a.normalize(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Attachments) != 2 || e.Attachments[0].Ref == e.Attachments[1].Ref {
		t.Fatal(e.Attachments)
	}
	m.Files[1].Media.EncryptQueryParam = "mutated"
	p := e.Attachments[1]
	data, err := a.Download(context.Background(), e.Route, p)
	if err != nil || string(data.Data) != "download" || f.rawMedia == nil || f.rawMedia.EncryptQueryParam != "second-secret" {
		t.Fatal(data, err, f.downloaded)
	}
	foreign := e.Route
	foreign.ChatID = "other"
	p.Scope = &foreign
	if _, err = a.Download(context.Background(), foreign, p); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "secret") {
		t.Fatal(string(raw))
	}
}

func TestUploadSendUsesNativeMediaAndBindsReply(t *testing.T) {
	a, f := fixture(t)
	e, _ := a.normalize(incoming())
	p, err := a.Upload(context.Background(), e.Route, gateway.MediaUpload{Kind: "file", Name: "report.pdf", Data: []byte("pdf"), ReplyTo: e.MessageID})
	if err != nil || f.uploads != 1 {
		t.Fatal(p, err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "private") {
		t.Fatal(string(raw))
	}
	result, err := a.Send(context.Background(), e.Route, gateway.Message{Attachments: []gateway.Attachment{p}, ReplyTo: e.MessageID})
	if err != nil || !result.Accepted || f.sent.Item.Type != wechatbot.ItemFile || f.sent.Item.FileItem.Media.AESKey != "secret-key" || f.sent.Upload == nil || f.inbound.ContextToken != "secret-context" {
		t.Fatal(result, err, f.sent)
	}
	if _, err = a.Send(context.Background(), e.Route, gateway.Message{Attachments: []gateway.Attachment{p}}); !errors.Is(err, gateway.ErrInvalidEvent) {
		t.Fatal(err)
	}
	foreign := e.Route
	foreign.ChatID = "other"
	p.Scope = &foreign
	if _, err = a.Send(context.Background(), foreign, gateway.Message{Attachments: []gateway.Attachment{p}, ReplyTo: e.MessageID}); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
}

func TestUploadRejectsBeforeNativeIO(t *testing.T) {
	a, f := fixture(t)
	e, _ := a.normalize(incoming())
	for _, u := range []gateway.MediaUpload{{Kind: "file", Data: []byte("x"), ReplyTo: "unknown"}, {Kind: "voice", Data: []byte("x"), ReplyTo: e.MessageID}} {
		if _, err := a.Upload(context.Background(), e.Route, u); err == nil {
			t.Fatal("accepted invalid upload")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Upload(ctx, e.Route, gateway.MediaUpload{Kind: "file", Data: []byte("x"), ReplyTo: e.MessageID}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.uploads != 0 {
		t.Fatal("invalid input performed upload")
	}
}

func TestQuotedDownloadPreservesNativeRecoveryAndErrors(t *testing.T) {
	a, f := fixture(t)
	m := incoming()
	m.QuotedMessage = &wechatbot.QuotedMessage{MessageID: "old", Text: "quoted text", Media: &wechatbot.QuotedMedia{Type: wechatbot.ContentFile, FileName: "old.pdf", Cached: true}}
	e, err := a.normalize(m)
	if err != nil {
		t.Fatal(err)
	}
	if e.Quote == nil || len(e.Attachments) != 0 || len(e.Quote.Attachments) != 1 {
		t.Fatal(e)
	}
	f.mediaErr = wechatbot.ErrQuoteMediaUnavailable
	if _, err = a.Download(context.Background(), e.Route, e.Quote.Attachments[0]); !errors.Is(err, wechatbot.ErrQuoteMediaUnavailable) {
		t.Fatal(err)
	}
	if !f.quoted || !f.downloaded.QuotedMessage.Media.Cached {
		t.Fatal("native quote recovery bypassed")
	}
}

func TestUploadSendDoesNotExposeCachedMutableResources(t *testing.T) {
	for _, kind := range []string{"image", "file", "video"} {
		t.Run(kind, func(t *testing.T) {
			a, f := fixture(t)
			e, _ := a.normalize(incoming())
			p, err := a.Upload(context.Background(), e.Route, gateway.MediaUpload{Kind: kind, Name: "original", Data: []byte("data"), ReplyTo: e.MessageID})
			if err != nil {
				t.Fatal(err)
			}
			message := gateway.Message{Attachments: []gateway.Attachment{p}, ReplyTo: e.MessageID}
			if _, err = a.Send(context.Background(), e.Route, message); err != nil {
				t.Fatal(err)
			}
			resource := func(out wechatbot.OutboundMessage) *wechatbot.CDNMedia {
				switch kind {
				case "image":
					return out.Item.ImageItem.Media
				case "video":
					return out.Item.VideoItem.Media
				default:
					return out.Item.FileItem.Media
				}
			}
			// BeforeSend/AfterSend hooks receive this native object and may edit it.
			resource(f.sent).EncryptQueryParam = "hook-replaced-resource"
			f.sent.Upload.Media.EncryptQueryParam = "hook-replaced-upload"
			if _, err = a.Send(context.Background(), e.Route, message); err != nil {
				t.Fatal(err)
			}
			if resource(f.sent).EncryptQueryParam != "private-location" || f.sent.Upload.Media.EncryptQueryParam != "private-location" {
				t.Fatal("native hook mutation changed the cached upload")
			}
		})
	}
}

func TestPartsPreserveVoiceTranscriptOrder(t *testing.T) {
	a, _ := fixture(t)
	m := incoming()
	media := &wechatbot.CDNMedia{EncryptQueryParam: "voice", AESKey: "key"}
	m.Raw.ItemList = []wechatbot.MessageItem{
		{Type: wechatbot.ItemText, TextItem: &wechatbot.TextItem{Text: "before"}},
		{Type: wechatbot.ItemVoice, VoiceItem: &wechatbot.VoiceItem{Media: media, Text: "recognized speech"}},
		{Type: wechatbot.ItemText, TextItem: &wechatbot.TextItem{Text: "after"}},
	}
	m.Voices = []wechatbot.VoiceContent{{Media: media, Text: "recognized speech"}}
	e, err := a.normalize(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Parts) != 4 || e.Parts[0].Text != "before" || e.Parts[1].Text != "recognized speech" || e.Parts[2].Attachment == nil || e.Parts[3].Text != "after" {
		t.Fatalf("voice transcript lost or reordered: %+v", e.Parts)
	}
}

func TestVoiceTranscriptWithoutMediaHasNoDownloadReference(t *testing.T) {
	a, _ := fixture(t)
	m := incoming()
	m.Raw.ItemList = []wechatbot.MessageItem{{Type: wechatbot.ItemVoice, VoiceItem: &wechatbot.VoiceItem{Text: "recognized speech"}}}
	m.Voices = []wechatbot.VoiceContent{{Text: "recognized speech"}}
	e, err := a.normalize(m)
	if err != nil || e.Text != "recognized speech" || len(e.Attachments) != 0 || len(e.Parts) != 1 || e.Parts[0].Text != e.Text {
		t.Fatalf("transcript-only voice advertised unavailable audio: %+v, %v", e, err)
	}
	m.Raw.ItemList[0].VoiceItem.Text = ""
	m.Voices[0].Text = ""
	if _, err := a.normalize(m); !errors.Is(err, gateway.ErrInvalidEvent) {
		t.Fatalf("empty voice content: %v", err)
	}
}
