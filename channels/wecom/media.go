package wecom

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

const mediaChunkBytes = 512 * 1024
const maxMediaBytes = mediaChunkBytes * 100
const uploadRefPrefix = "wecom-upload:"
const inboundRefPrefix = "wecom-inbound:"

type mediaSource struct {
	content content
	kind    string
	inline  bool
}
type uploadedMedia struct {
	session  *session
	id, kind string
}

func (a *Adapter) mediaRoute(r gateway.Route) error {
	if r.Account != a.Account() || r.Validate() != nil || r.TenantID != a.cfg.TenantID || (r.Kind != "direct" && r.Kind != "group") || r.ThreadID != "" {
		return gateway.ErrInvalidEvent
	}
	return nil
}

// Download resolves a previously observed attachment. Sensitive URLs and keys
// stay in the adapter's bounded, five-minute cache, never in the persisted Event.
func (a *Adapter) Download(ctx context.Context, r gateway.Route, p gateway.Attachment) (gateway.MediaContent, error) {
	if err := a.mediaRoute(r); err != nil {
		return gateway.MediaContent{}, err
	}
	if err := p.ValidateFor(r); err != nil {
		return gateway.MediaContent{}, err
	}
	if err := ctx.Err(); err != nil {
		return gateway.MediaContent{}, err
	}
	src, ok := a.downloads.Get(wire.ContextKey(r, p.Ref))
	if !ok {
		return gateway.MediaContent{}, gateway.ErrMediaExpired
	}
	if p.Kind != src.kind {
		return gateway.MediaContent{}, gateway.ErrInvalidEvent
	}
	var data []byte
	var h http.Header
	var err error
	if src.inline {
		encoded, ok := a.inlineMedia.Get(wire.ContextKey(r, p.Ref))
		if !ok {
			return gateway.MediaContent{}, gateway.ErrMediaExpired
		}
		if base64.StdEncoding.DecodedLen(len(encoded)) > maxMediaBytes {
			return gateway.MediaContent{}, gateway.ErrMediaTooLarge
		}
		data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return gateway.MediaContent{}, errors.New("wecom: invalid inline media")
		}
	} else {
		if src.content.URL == "" {
			return gateway.MediaContent{}, errors.New("wecom: attachment has no download locator")
		}
		data, h, err = wire.DownloadMedia(ctx, a.cfg.HTTPClient, "wecom", src.content.URL, nil, maxMediaBytes+32)
		if err != nil {
			return gateway.MediaContent{}, err
		}
		if src.content.AESKey != "" {
			keyText, err := url.PathUnescape(src.content.AESKey)
			if err != nil {
				return gateway.MediaContent{}, errors.New("wecom: invalid media key")
			}
			key, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(keyText, "="))
			if err != nil || len(key) != 32 {
				return gateway.MediaContent{}, errors.New("wecom: invalid media key")
			}
			data, err = wire.DecryptCBC(key, key[:16], data, 32)
			if err != nil {
				return gateway.MediaContent{}, errors.New("wecom: media decryption failed")
			}
		}
	}
	if len(data) > maxMediaBytes {
		return gateway.MediaContent{}, gateway.ErrMediaTooLarge
	}
	if err := ctx.Err(); err != nil {
		return gateway.MediaContent{}, err
	}
	name := src.content.Name
	if name == "" {
		_, params, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
		name = params["filename"]
	}
	// Encrypted HTTP bodies describe the ciphertext; only plaintext response
	// headers can supply the downloaded file's MIME type.
	contentType := ""
	if src.content.AESKey == "" {
		contentType, _, _ = mime.ParseMediaType(h.Get("Content-Type"))
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	return gateway.MediaContent{Data: data, Name: name, MIME: contentType}, nil
}

// Upload uses the official init/chunk/finish protocol, with sequential 512 KiB
// chunks and no automatic retry. The caller context bounds the whole operation.
func (a *Adapter) Upload(ctx context.Context, r gateway.Route, u gateway.MediaUpload) (gateway.Attachment, error) {
	if err := a.mediaRoute(r); err != nil {
		return gateway.Attachment{}, err
	}
	if err := u.Validate(); err != nil {
		return gateway.Attachment{}, err
	}
	switch u.Kind {
	case "image", "file", "voice", "video":
	default:
		return gateway.Attachment{}, gateway.ErrUnsupported
	}
	if len(u.Data) > maxMediaBytes {
		return gateway.Attachment{}, gateway.ErrMediaTooLarge
	}
	if u.Name == "" {
		return gateway.Attachment{}, gateway.ErrInvalidEvent
	}
	s := a.active()
	if s == nil {
		return gateway.Attachment{}, errors.New("wecom: not connected")
	}
	if u.ReplyTo != "" {
		reply, err := a.reply(r, u.ReplyTo)
		if err != nil {
			return gateway.Attachment{}, err
		}
		if reply.session != s {
			return gateway.Attachment{}, gateway.ErrReplyExpired
		}
	}
	request := func(cmd string, body any, out any) error {
		f, err := s.requestFrame(ctx, cmd, requestID(cmd), body, a.cfg.MediaTimeout)
		if err != nil {
			return err
		}
		if out != nil && json.Unmarshal(f.Body, out) != nil {
			return &gateway.APIError{Platform: "wecom", Code: "invalid_media_ack", Uncertain: true}
		}
		return nil
	}
	// MD5 is an official upload protocol field, not an application fingerprint.
	digest := md5.Sum(u.Data)
	var init struct {
		ID string `json:"upload_id"`
	}
	if err := request("aibot_upload_media_init", map[string]any{"type": u.Kind, "filename": u.Name, "total_size": len(u.Data), "total_chunks": (len(u.Data) + mediaChunkBytes - 1) / mediaChunkBytes, "md5": hex.EncodeToString(digest[:])}, &init); err != nil {
		return gateway.Attachment{}, err
	}
	if init.ID == "" {
		return gateway.Attachment{}, &gateway.APIError{Platform: "wecom", Code: "missing_upload_id", Uncertain: true}
	}
	for index, start := 0, 0; start < len(u.Data); index, start = index+1, start+mediaChunkBytes {
		end := min(start+mediaChunkBytes, len(u.Data))
		if err := request("aibot_upload_media_chunk", map[string]any{"upload_id": init.ID, "chunk_index": index, "base64_data": base64.StdEncoding.EncodeToString(u.Data[start:end])}, nil); err != nil {
			return gateway.Attachment{}, err
		}
	}
	var finish struct {
		ID   string `json:"media_id"`
		Type string `json:"type"`
	}
	if err := request("aibot_upload_media_finish", map[string]string{"upload_id": init.ID}, &finish); err != nil {
		return gateway.Attachment{}, err
	}
	if finish.ID == "" || (finish.Type != "" && finish.Type != u.Kind) {
		return gateway.Attachment{}, &gateway.APIError{Platform: "wecom", Code: "invalid_media_result", Uncertain: true}
	}
	// A validated completion ACK remains successful if cancellation arrives
	// afterwards. The connection still has to own the returned media resource.
	if s != a.active() || s.ctx.Err() != nil {
		return gateway.Attachment{}, gateway.ErrMediaExpired
	}
	p := gateway.Attachment{Kind: u.Kind, Ref: uploadRefPrefix + requestID("media"), Name: u.Name, MIME: u.MIME, Size: int64(len(u.Data)), Scope: &r, ExpiresAt: time.Now().Add(30 * time.Minute)}
	a.uploads.Put(wire.ContextKey(r, p.Ref), uploadedMedia{s, finish.ID, u.Kind}, p.ExpiresAt)
	return p, nil
}

func (a *Adapter) sendMedia(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if len(m.Attachments) != 1 || m.Text != "" || len(m.Card) > 0 || m.Format == "markdown" {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	p := m.Attachments[0]
	if err := p.ValidateFor(r); err != nil {
		return gateway.Receipt{}, err
	}
	if strings.HasPrefix(p.Ref, inboundRefPrefix) {
		// An inbound download locator is not an uploaded platform media_id.
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	switch p.Kind {
	case "image", "file", "voice", "video":
	default:
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	s := a.active()
	if s == nil {
		return gateway.Receipt{}, errors.New("wecom: not connected")
	}
	id := p.Ref
	if strings.HasPrefix(id, uploadRefPrefix) {
		u, ok := a.uploads.Get(wire.ContextKey(r, id))
		if !ok || u.session != s || s.ctx.Err() != nil {
			return gateway.Receipt{}, gateway.ErrMediaExpired
		}
		if u.kind != p.Kind {
			return gateway.Receipt{}, gateway.ErrInvalidEvent
		}
		id = u.id
	}
	body := map[string]any{"msgtype": p.Kind, p.Kind: map[string]string{"media_id": id}}
	if m.ReplyTo != "" {
		reply, err := a.reply(r, m.ReplyTo)
		if err != nil {
			return gateway.Receipt{}, err
		}
		if reply.session != s {
			return gateway.Receipt{}, gateway.ErrReplyExpired
		}
		return s.request(ctx, "aibot_respond_msg", reply.RequestID, body)
	}
	body["chatid"] = r.ChatID
	return s.request(ctx, "aibot_send_msg", requestID("media"), body)
}
