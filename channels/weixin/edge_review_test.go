package weixin

import (
	"errors"
	"testing"

	"github.com/Icatme/imbot-go"
	"github.com/Icatme/imbot-go/gateway"
)

func TestPartsSkipEmptyWireMediaWithoutShiftingResourceIndex(t *testing.T) {
	a, _ := fixture(t)
	m := incoming()
	media := &imbot.CDNMedia{EncryptQueryParam: "resource"}
	m.Raw.ItemList = []imbot.MessageItem{
		{Type: imbot.ItemImage},
		{Type: imbot.ItemText, TextItem: &imbot.TextItem{Text: "before image"}},
		{Type: imbot.ItemImage, ImageItem: &imbot.ImageItem{Media: media}},
	}
	// The native parser ignores the empty image_item; its image array has one entry.
	m.Images = []imbot.ImageContent{{Media: media}}
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
				m.Images = []imbot.ImageContent{{}}
			case "image_with_empty_cdn":
				m.Images = []imbot.ImageContent{{Media: &imbot.CDNMedia{}, URL: "https://example.com/image"}}
			case "file":
				m.Files = []imbot.FileContent{{FileName: "missing.bin"}}
			case "video":
				m.Videos = []imbot.VideoContent{{}}
			case "voice":
				m.Voices = []imbot.VoiceContent{{Media: &imbot.CDNMedia{}, Text: "transcript"}}
			}
			if _, err := a.normalize(m); !errors.Is(err, gateway.ErrInvalidEvent) {
				t.Fatalf("accepted media without usable locator: %v", err)
			}
		})
	}
}
