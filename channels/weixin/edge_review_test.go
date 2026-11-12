package weixin

import (
	"errors"
	"testing"

	wechatbot "github.com/Icatme/wechatbot-go"
	"github.com/Icatme/wechatbot-go/gateway"
)

func TestPartsSkipEmptyWireMediaWithoutShiftingResourceIndex(t *testing.T) {
	a, _ := fixture(t)
	m := incoming()
	media := &wechatbot.CDNMedia{EncryptQueryParam: "resource"}
	m.Raw.ItemList = []wechatbot.MessageItem{
		{Type: wechatbot.ItemImage},
		{Type: wechatbot.ItemText, TextItem: &wechatbot.TextItem{Text: "before image"}},
		{Type: wechatbot.ItemImage, ImageItem: &wechatbot.ImageItem{Media: media}},
	}
	// The native parser ignores the empty image_item; its image array has one entry.
	m.Images = []wechatbot.ImageContent{{Media: media}}
	e, err := a.normalize(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Parts) != 2 || e.Parts[0].Text != "before image" || e.Parts[1].Attachment == nil || e.Parts[1].Attachment.Ref != e.Attachments[0].Ref {
		t.Fatalf("empty wire item shifted media order: %#v", e.Parts)
	}
}

func TestMalformedMediaDoesNotAdvertiseDownloadableReference(t *testing.T) {
	for _, kind := range []string{"image", "image_with_empty_cdn", "file", "video", "voice"} {
		t.Run(kind, func(t *testing.T) {
			a, _ := fixture(t)
			m := incoming()
			switch kind {
			case "image":
				m.Images = []wechatbot.ImageContent{{}}
			case "image_with_empty_cdn":
				m.Images = []wechatbot.ImageContent{{Media: &wechatbot.CDNMedia{}, URL: "https://example.com/image"}}
			case "file":
				m.Files = []wechatbot.FileContent{{FileName: "missing.bin"}}
			case "video":
				m.Videos = []wechatbot.VideoContent{{}}
			case "voice":
				m.Voices = []wechatbot.VoiceContent{{Media: &wechatbot.CDNMedia{}, Text: "transcript"}}
			}
			if _, err := a.normalize(m); !errors.Is(err, gateway.ErrInvalidEvent) {
				t.Fatalf("accepted media without usable locator: %v", err)
			}
		})
	}
}
