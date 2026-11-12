package qqbot

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1" // Required by the QQ upload_prepare wire protocol, not a security signature.
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

const uploadRefPrefix = "qqbot-upload:"
const inboundRefPrefix = "qqbot-inbound:"

type uploadedResource struct{ FileInfo, Kind string }

func (a *Adapter) outboundFileInfo(r gateway.Route, p gateway.Attachment) (string, error) {
	if p.MessageID != "" || strings.HasPrefix(p.Ref, inboundRefPrefix) {
		return "", gateway.ErrUnsupported
	}
	if !strings.HasPrefix(p.Ref, uploadRefPrefix) {
		return p.Ref, nil // explicitly supplied native file_info from the trusted host
	}
	upload, ok := a.uploads.Get(wire.ContextKey(r, p.Ref))
	if !ok {
		return "", gateway.ErrMediaExpired
	}
	if upload.Kind != p.Kind {
		return "", gateway.ErrInvalidEvent
	}
	return upload.FileInfo, nil
}

func (a *Adapter) mediaRoute(r gateway.Route) error {
	if r.Validate() != nil || r.Account != a.Account() || r.TenantID != a.cfg.AppID {
		return gateway.ErrInvalidEvent
	}
	if r.ThreadID != "" || (r.Kind != "direct" && r.Kind != "group") {
		return gateway.ErrUnsupported
	}
	return nil
}

func (a *Adapter) mediaPath(r gateway.Route, operation string) string {
	kind := "users"
	if r.Kind == "group" {
		kind = "groups"
	}
	return "/v2/" + kind + "/" + wire.PathID(r.ChatID) + "/" + operation
}

// Download resolves only locators observed in an authenticated callback for this
// exact route. URLs and credentials are deliberately absent from Attachment JSON.
func (a *Adapter) Download(ctx context.Context, r gateway.Route, p gateway.Attachment) (gateway.MediaContent, error) {
	return a.download(ctx, r, p, false)
}

// DownloadVoiceWAV explicitly selects QQ's supplied WAV rendition. Download
// continues to fetch the original URL; missing renditions do not fall back.
func (a *Adapter) DownloadVoiceWAV(ctx context.Context, r gateway.Route, p gateway.Attachment) (gateway.MediaContent, error) {
	return a.download(ctx, r, p, true)
}

func (a *Adapter) download(ctx context.Context, r gateway.Route, p gateway.Attachment, voiceWAV bool) (gateway.MediaContent, error) {
	if err := ctx.Err(); err != nil {
		return gateway.MediaContent{}, err
	}
	if err := a.mediaRoute(r); err != nil {
		return gateway.MediaContent{}, err
	}
	if err := p.ValidateFor(r); err != nil {
		return gateway.MediaContent{}, err
	}
	source, ok := a.media.Get(wire.ContextKey(r, p.Ref))
	if !ok {
		return gateway.MediaContent{}, gateway.ErrMediaExpired
	}
	if p.Kind != attachmentKind(source.Type) {
		return gateway.MediaContent{}, gateway.ErrInvalidEvent
	}
	if voiceWAV {
		if p.Kind != "voice" {
			return gateway.MediaContent{}, gateway.ErrUnsupported
		}
		if source.VoiceWAV == "" {
			return gateway.MediaContent{}, gateway.ErrUnsupported
		}
		source.URL, source.Type, source.Size = source.VoiceWAV, "audio/wav", 0
		if source.Name == "" {
			source.Name = "voice.wav"
		} else {
			source.Name = strings.TrimSuffix(source.Name, path.Ext(source.Name)) + ".wav"
		}
	}
	if source.Size > gateway.MaxMediaBytes {
		return gateway.MediaContent{}, gateway.ErrMediaTooLarge
	}
	u, err := url.Parse(source.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" {
		return gateway.MediaContent{}, errors.New("qqbot: invalid media location")
	}
	host := strings.ToLower(u.Hostname())
	if host != "multimedia.nt.qq.com.cn" && host != "gchat.qpic.cn" && host != "c2cpicdw.qpic.cn" {
		return gateway.MediaContent{}, errors.New("qqbot: untrusted media host")
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.MediaContent{}, err
	}
	data, headers, err := wire.DownloadMedia(ctx, a.cfg.HTTPClient, "qqbot", source.URL, http.Header{"Authorization": {"QQBot " + token}}, gateway.MaxMediaBytes)
	if err != nil {
		return gateway.MediaContent{}, a.invalidateTokenOnUnauthorized(token, err)
	}
	mime := source.Type
	if !strings.Contains(mime, "/") {
		mime = headers.Get("Content-Type")
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	return gateway.MediaContent{Data: data, Name: source.Name, MIME: mime}, nil
}

type uploadResult struct {
	FileInfo string `json:"file_info"`
	TTL      int64  `json:"ttl"`
}

// Upload implements QQ's prepare -> COS PUT -> part_finish -> files protocol.
// It uploads caller-provided bytes and never automatically sends a message.
// Source: tencent-connect/qqbot-nodejs src/protocol/api/media-chunked.ts.
func (a *Adapter) Upload(ctx context.Context, r gateway.Route, u gateway.MediaUpload) (gateway.Attachment, error) {
	if err := u.Validate(); err != nil {
		return gateway.Attachment{}, err
	}
	if err := a.checkUpload(ctx, r, u.ReplyTo); err != nil {
		return gateway.Attachment{}, err
	}
	fileType, err := mediaType(u.Kind)
	if err != nil {
		return gateway.Attachment{}, err
	}
	data := bytes.Clone(u.Data)
	name := u.Name
	if name == "" {
		name = "file"
	}
	md5sum, sha1sum := md5.Sum(data), sha1.Sum(data)
	const firstBytes = 10002432 // QQ's md5_10m protocol window (not 10 MiB).
	firstSum := md5.Sum(data[:min(len(data), firstBytes)])
	var prepare struct {
		ID        string `json:"upload_id"`
		BlockSize int64  `json:"block_size"`
		Parts     []struct {
			Index int    `json:"index"`
			URL   string `json:"presigned_url"`
		} `json:"parts"`
	}
	err = a.operationJSON(ctx, http.MethodPost, a.mediaPath(r, "upload_prepare"), map[string]any{
		"file_type": fileType, "file_name": name, "file_size": len(data),
		"md5": hex.EncodeToString(md5sum[:]), "sha1": hex.EncodeToString(sha1sum[:]), "md5_10m": hex.EncodeToString(firstSum[:]),
	}, &prepare)
	if err != nil {
		return gateway.Attachment{}, err
	}
	if prepare.ID == "" || prepare.BlockSize <= 0 || prepare.BlockSize > gateway.MaxMediaBytes {
		return gateway.Attachment{}, invalidUpload("invalid_prepare")
	}
	count := (int64(len(data)) + prepare.BlockSize - 1) / prepare.BlockSize
	if int64(len(prepare.Parts)) != count {
		return gateway.Attachment{}, invalidUpload("incomplete_parts")
	}
	seen := make(map[int]bool, len(prepare.Parts))
	for _, part := range prepare.Parts {
		if part.Index < 1 || int64(part.Index) > count || seen[part.Index] || !validCOSURL(part.URL) {
			return gateway.Attachment{}, invalidUpload("invalid_part")
		}
		seen[part.Index] = true
	}
	// Sequential parts satisfy the server's concurrency ceiling without keeping
	// additional whole-file copies or issuing work after a failed part.
	for _, part := range prepare.Parts {
		if err := ctx.Err(); err != nil {
			return gateway.Attachment{}, err
		}
		offset := int64(part.Index-1) * prepare.BlockSize
		block := data[offset:min(offset+prepare.BlockSize, int64(len(data)))]
		_, _, err = wire.MediaTransfer(ctx, a.cfg.HTTPClient, "qqbot", http.MethodPut, part.URL, nil, bytes.NewReader(block), gateway.MaxMediaBytes)
		if err != nil {
			return gateway.Attachment{}, err
		}
		checksum := md5.Sum(block)
		if err = a.operationJSON(ctx, http.MethodPost, a.mediaPath(r, "upload_part_finish"), map[string]any{"upload_id": prepare.ID, "part_index": part.Index, "block_size": len(block), "md5": hex.EncodeToString(checksum[:])}, nil); err != nil {
			return gateway.Attachment{}, err
		}
	}
	var out uploadResult
	if err = a.operationJSON(ctx, http.MethodPost, a.mediaPath(r, "files"), map[string]string{"upload_id": prepare.ID}, &out); err != nil {
		return gateway.Attachment{}, err
	}
	return a.rememberUpload(r, u.Kind, name, u.MIME, int64(len(data)), out)
}

// URLUpload explicitly asks QQ to fetch a public URL. The SDK does not fetch it
// locally and does not substitute a text link if the platform rejects it.
type URLUpload struct{ Kind, Name, URL, ReplyTo string }

func (a *Adapter) UploadURL(ctx context.Context, r gateway.Route, u URLUpload) (gateway.Attachment, error) {
	if err := a.checkUpload(ctx, r, u.ReplyTo); err != nil {
		return gateway.Attachment{}, err
	}
	fileType, err := mediaType(u.Kind)
	if err != nil {
		return gateway.Attachment{}, err
	}
	if !validUploadURL(u.URL) || strings.ContainsAny(u.Name, "\r\n\x00") {
		return gateway.Attachment{}, gateway.ErrInvalidEvent
	}
	name := u.Name
	if name == "" {
		parsed, _ := url.Parse(u.URL)
		name = path.Base(parsed.Path)
	}
	body := map[string]any{"file_type": fileType, "url": u.URL, "srv_send_msg": false}
	if fileType == 4 && name != "" {
		body["file_name"] = name
	}
	var out uploadResult
	if err = a.operationJSON(ctx, http.MethodPost, a.mediaPath(r, "files"), body, &out); err != nil {
		return gateway.Attachment{}, err
	}
	return a.rememberUpload(r, u.Kind, name, "", 0, out)
}

func validUploadURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return false
	}
	return !strings.EqualFold(u.Hostname(), "localhost")
}

func validCOSURL(raw string) bool {
	if !validUploadURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	return u.Port() == "" && strings.HasSuffix(strings.ToLower(u.Hostname()), ".myqcloud.com")
}

func (a *Adapter) checkUpload(ctx context.Context, r gateway.Route, replyTo string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.mediaRoute(r); err != nil {
		return err
	}
	if a.isInteractionReply(r, replyTo) {
		return gateway.ErrUnsupported
	}
	if replyTo == "" && !a.cfg.EnableProactive {
		return gateway.ErrUnsupported
	}
	return nil
}

func mediaType(kind string) (int, error) {
	switch kind {
	case "image":
		return 1, nil
	case "video":
		return 2, nil
	case "voice", "audio":
		return 3, nil
	case "file":
		return 4, nil
	default:
		return 0, gateway.ErrUnsupported
	}
}

func (a *Adapter) rememberUpload(r gateway.Route, kind, name, mime string, size int64, out uploadResult) (gateway.Attachment, error) {
	if out.FileInfo == "" || out.TTL <= 0 || out.TTL > int64((1<<63-1)/time.Second) {
		return gateway.Attachment{}, invalidUpload("invalid_file_info")
	}
	p := gateway.Attachment{Kind: kind, Ref: uploadRefPrefix + rand.Text(), Name: name, MIME: mime, Size: size, Scope: &r, ExpiresAt: time.Now().Add(time.Duration(out.TTL) * time.Second)}
	a.uploads.Put(wire.ContextKey(r, p.Ref), uploadedResource{FileInfo: out.FileInfo, Kind: kind}, p.ExpiresAt)
	return p, nil
}

func invalidUpload(code string) error {
	return &gateway.APIError{Platform: "qqbot", Status: 200, Code: code, Uncertain: true}
}
