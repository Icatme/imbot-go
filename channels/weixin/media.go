package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	wechatbot "github.com/Icatme/wechatbot-go"
	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

type mediaEntry struct {
	inbound  *wechatbot.IncomingMessage
	quoted   bool
	outbound *wechatbot.MessageItem
	upload   *wechatbot.UploadResult
	replyTo  string
}

// copyOutbound keeps mutable native hook arguments out of the shared cache.
// Copying UploadResult also preserves its private native session binding.
func (e mediaEntry) copyOutbound(clientID string) wechatbot.OutboundMessage {
	out := wechatbot.OutboundMessage{ClientID: clientID, Item: *e.outbound}
	if p := out.Item.ImageItem; p != nil {
		v := *p
		v.Media, v.ThumbMedia = cloneCDN(p.Media), cloneCDN(p.ThumbMedia)
		out.Item.ImageItem = &v
	}
	if p := out.Item.FileItem; p != nil {
		v := *p
		v.Media = cloneCDN(p.Media)
		out.Item.FileItem = &v
	}
	if p := out.Item.VideoItem; p != nil {
		v := *p
		v.Media, v.ThumbMedia = cloneCDN(p.Media), cloneCDN(p.ThumbMedia)
		out.Item.VideoItem = &v
	}
	if e.upload != nil {
		upload := *e.upload
		upload.AESKey = bytes.Clone(upload.AESKey)
		out.Upload = &upload
	}
	return out
}

func cloneCDN(p *wechatbot.CDNMedia) *wechatbot.CDNMedia {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func hasMediaLocator(p *wechatbot.CDNMedia) bool {
	return p != nil && (p.EncryptQueryParam != "" || p.FullURL != "")
}

func (a *Adapter) rememberMedia(r gateway.Route, id, kind string, index int, name string, size int64, m *wechatbot.IncomingMessage) gateway.Attachment {
	p := gateway.Attachment{Kind: kind, Ref: id + ":" + kind + ":" + strconv.Itoa(index), Name: name, Size: size, MessageID: id, Scope: &r, ExpiresAt: time.Now().Add(a.cfg.ReplyTTL)}
	selected := *m // retains native, unexported session generation
	if m.Raw != nil {
		raw := *m.Raw
		selected.Raw = &raw
	}
	selected.Images, selected.Files, selected.Videos, selected.Voices = nil, nil, nil, nil
	switch kind {
	case "image":
		v := m.Images[index]
		v.Media = cloneCDN(v.Media)
		v.ThumbMedia = cloneCDN(v.ThumbMedia)
		selected.Images = []wechatbot.ImageContent{v}
	case "file":
		v := m.Files[index]
		v.Media = cloneCDN(v.Media)
		selected.Files = []wechatbot.FileContent{v}
	case "video":
		v := m.Videos[index]
		v.Media = cloneCDN(v.Media)
		selected.Videos = []wechatbot.VideoContent{v}
	case "voice":
		v := m.Voices[index]
		v.Media = cloneCDN(v.Media)
		selected.Voices = []wechatbot.VoiceContent{v}
		p.MIME = "audio/silk"
	case "quote":
		q := *m.QuotedMessage
		selected.QuotedMessage = &q
		if q.Media != nil {
			v := *q.Media
			v.Media = cloneCDN(v.Media)
			q.Media = &v
		}
	}
	a.media.Put(wire.ContextKey(r, p.Ref), mediaEntry{inbound: &selected, quoted: kind == "quote"}, p.ExpiresAt)
	return p
}

func (a *Adapter) Download(ctx context.Context, r gateway.Route, p gateway.Attachment) (gateway.MediaContent, error) {
	if err := ctx.Err(); err != nil {
		return gateway.MediaContent{}, err
	}
	if r.Account != a.Account() {
		return gateway.MediaContent{}, gateway.ErrInvalidEvent
	}
	if err := p.ValidateFor(r); err != nil {
		return gateway.MediaContent{}, err
	}
	entry, ok := a.media.Get(wire.ContextKey(r, p.Ref))
	if !ok || entry.inbound == nil {
		return gateway.MediaContent{}, gateway.ErrMediaExpired
	}
	var result *wechatbot.DownloadedMedia
	var err error
	if entry.quoted {
		result, err = a.bot.DownloadQuoted(ctx, entry.inbound)
	} else {
		// Native Download caches one media payload under the entire message's
		// quote ID. Selecting a later attachment through that API would overwrite
		// the first attachment's retained payload. Raw downloads have no such side
		// effect; the host owns persistence of bridge attachment bytes.
		result, err = a.downloadRaw(ctx, entry.inbound)
	}
	if err != nil {
		return gateway.MediaContent{}, err
	}
	if result == nil {
		return gateway.MediaContent{}, gateway.ErrMediaExpired
	}
	if int64(len(result.Data)) > gateway.MaxMediaBytes {
		return gateway.MediaContent{}, gateway.ErrMediaTooLarge
	}
	return gateway.MediaContent{Data: result.Data, Name: result.FileName, MIME: result.MIMEType}, nil
}

func (a *Adapter) downloadRaw(ctx context.Context, m *wechatbot.IncomingMessage) (*wechatbot.DownloadedMedia, error) {
	var media *wechatbot.CDNMedia
	var name, typ string
	switch {
	case len(m.Images) > 0:
		data, err := a.bot.DownloadImage(ctx, m.Images[0])
		if err != nil {
			return nil, err
		}
		return &wechatbot.DownloadedMedia{Data: data, Type: "image", MIMEType: http.DetectContentType(data)}, nil
	case len(m.Files) > 0:
		media, name, typ = m.Files[0].Media, m.Files[0].FileName, "file"
		if name == "" {
			name = "file.bin"
		}
	case len(m.Videos) > 0:
		media, typ = m.Videos[0].Media, "video"
	case len(m.Voices) > 0:
		media, typ = m.Voices[0].Media, "voice"
	}
	if media == nil {
		return nil, gateway.ErrMediaExpired
	}
	data, err := a.bot.DownloadRaw(ctx, media, "")
	if err != nil {
		return nil, err
	}
	result := &wechatbot.DownloadedMedia{Data: data, FileName: name, Type: typ}
	if typ == "voice" {
		result.MIMEType, result.Format = "audio/silk", "silk"
	} else {
		result.MIMEType = mime.TypeByExtension(filepath.Ext(name))
		if result.MIMEType == "" {
			result.MIMEType = http.DetectContentType(data)
		}
	}
	return result, nil
}

func (a *Adapter) Upload(ctx context.Context, r gateway.Route, u gateway.MediaUpload) (gateway.Attachment, error) {
	if err := ctx.Err(); err != nil {
		return gateway.Attachment{}, err
	}
	if r.Account != a.Account() || r.Validate() != nil {
		return gateway.Attachment{}, gateway.ErrInvalidEvent
	}
	if r.TenantID != "" || r.Kind != "direct" {
		return gateway.Attachment{}, gateway.ErrUnsupported
	}
	if err := u.Validate(); err != nil {
		return gateway.Attachment{}, err
	}
	var inbound *wechatbot.IncomingMessage
	if u.ReplyTo != "" {
		var ok bool
		if inbound, ok = a.replies.Get(wire.ContextKey(r, u.ReplyTo)); !ok {
			return gateway.Attachment{}, gateway.ErrReplyExpired
		}
	} else if !a.cfg.EnableProactive || r.ThreadID != "" {
		return gateway.Attachment{}, gateway.ErrUnsupported
	}
	mediaType := int(wechatbot.MediaFile)
	switch u.Kind {
	case "image":
		mediaType = int(wechatbot.MediaImage)
	case "video":
		mediaType = int(wechatbot.MediaVideo)
	case "file":
	default:
		return gateway.Attachment{}, gateway.ErrUnsupported // no implicit audio transcoding or file fallback
	}
	var result *wechatbot.UploadResult
	var err error
	if inbound != nil {
		result, err = a.bot.UploadForReply(ctx, inbound, u.Data, mediaType)
	} else {
		result, err = a.bot.Upload(ctx, u.Data, r.ChatID, mediaType)
	}
	if err != nil {
		return gateway.Attachment{}, err
	}
	if result == nil || result.Media.EncryptQueryParam == "" && result.Media.FullURL == "" {
		return gateway.Attachment{}, gateway.ErrInvalidEvent
	}
	item := wechatbot.MessageItem{}
	switch u.Kind {
	case "image":
		item.Type = wechatbot.ItemImage
		item.ImageItem = &wechatbot.ImageItem{Media: cloneCDN(&result.Media), MidSize: int64(result.EncryptedFileSize)}
	case "video":
		item.Type = wechatbot.ItemVideo
		item.VideoItem = &wechatbot.VideoItem{Media: cloneCDN(&result.Media), VideoSize: int64(result.EncryptedFileSize)}
	case "file":
		item.Type = wechatbot.ItemFile
		item.FileItem = &wechatbot.FileItem{Media: cloneCDN(&result.Media), FileName: u.Name, Len: strconv.Itoa(len(u.Data))}
	}
	name := u.Name
	if name == "" && u.Kind == "file" {
		name = "file.bin"
		item.FileItem.FileName = name
	}
	mime := u.MIME
	if mime == "" {
		mime = http.DetectContentType(u.Data)
	}
	p := gateway.Attachment{Kind: u.Kind, Ref: "upload:" + rand.Text(), Name: name, MIME: mime, Size: int64(len(u.Data)), Scope: &r, ExpiresAt: time.Now().Add(a.cfg.ReplyTTL)}
	a.media.Put(wire.ContextKey(r, p.Ref), mediaEntry{outbound: &item, upload: result, replyTo: u.ReplyTo}, p.ExpiresAt)
	return p, nil
}

func (a *Adapter) addParts(e *gateway.Event, m *wechatbot.IncomingMessage) {
	indexes := map[string]int{}
	for _, item := range m.Raw.ItemList {
		kind := ""
		switch item.Type {
		case wechatbot.ItemText:
			if item.TextItem != nil {
				e.Parts = append(e.Parts, gateway.ContentPart{Text: item.TextItem.Text})
			}
		case wechatbot.ItemImage:
			if item.ImageItem != nil {
				kind = "image"
			}
		case wechatbot.ItemFile:
			if item.FileItem != nil {
				kind = "file"
			}
		case wechatbot.ItemVideo:
			if item.VideoItem != nil {
				kind = "video"
			}
		case wechatbot.ItemVoice:
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				e.Parts = append(e.Parts, gateway.ContentPart{Text: item.VoiceItem.Text})
			}
			if item.VoiceItem != nil {
				kind = "voice"
			}
		}
		if kind != "" {
			ref := e.ID + ":" + kind + ":" + strconv.Itoa(indexes[kind])
			indexes[kind]++
			for _, p := range e.Attachments {
				if p.Ref == ref {
					part := p
					e.Parts = append(e.Parts, gateway.ContentPart{Attachment: &part})
					break
				}
			}
		}
	}
}
