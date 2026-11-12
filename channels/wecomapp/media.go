package wecomapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

// The SDK bounds in-memory application media to 20 MiB; platform type-specific
// limits are still enforced by the upload API and returned without conversion.
const maxMediaBytes = 20 << 20
const uploadRefPrefix = "wecomapp-upload:"
const inboundRefPrefix = "wecomapp-inbound:"

type mediaResource struct{ id, kind, name string }

func (a *Adapter) mediaRoute(r gateway.Route) error {
	if r.Account != a.Account() || r.Validate() != nil || r.TenantID != a.cfg.CorpID || r.Kind != "direct" || r.ThreadID != "" || strings.TrimSpace(r.ChatID) != r.ChatID || strings.Contains(r.ChatID, "|") || strings.EqualFold(r.ChatID, "@all") {
		return gateway.ErrInvalidEvent
	}
	return nil
}

func (a *Adapter) mediaError(token string, code int) error {
	if code == 40001 || code == 40014 || code == 42001 {
		a.token.Invalidate(token)
	}
	return wire.Business("wecom_app", code)
}

// Invalidate credentials only for an authenticated API response. A failed CDN
// URL does not use the application token and must not invalidate that token.
// The failed request is returned unchanged; only an explicit later call refreshes.
func (a *Adapter) apiError(token string, err error) error {
	var api *gateway.APIError
	if errors.As(err, &api) && api.Status == http.StatusUnauthorized {
		a.token.Invalidate(token)
	}
	return err
}

// Download fetches an observed or locally uploaded application resource. The
// host must persist the returned bytes before acknowledging durable admission.
func (a *Adapter) Download(ctx context.Context, r gateway.Route, p gateway.Attachment) (gateway.MediaContent, error) {
	if err := a.mediaRoute(r); err != nil {
		return gateway.MediaContent{}, err
	}
	if err := p.ValidateFor(r); err != nil {
		return gateway.MediaContent{}, err
	}
	resource, ok := a.media.Get(wire.ContextKey(r, p.Ref))
	if !ok {
		return gateway.MediaContent{}, gateway.ErrMediaExpired
	}
	if resource.kind != p.Kind {
		return gateway.MediaContent{}, gateway.ErrInvalidEvent
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.MediaContent{}, err
	}
	endpoint := a.cfg.APIBase + "/cgi-bin/media/get?" + url.Values{"access_token": {token}, "media_id": {resource.id}}.Encode()
	data, headers, err := wire.MediaHTTP(ctx, a.cfg.HTTPClient, "wecom_app", http.MethodGet, endpoint, nil, nil, maxMediaBytes)
	if err != nil {
		return gateway.MediaContent{}, a.apiError(token, err)
	}
	contentType, _, _ := mime.ParseMediaType(headers.Get("Content-Type"))
	if contentType == "application/json" && headers.Get("Content-Disposition") == "" {
		var response struct {
			Code     *int   `json:"errcode"`
			VideoURL string `json:"video_url"`
		}
		if json.Unmarshal(data, &response) != nil {
			return gateway.MediaContent{}, &gateway.APIError{Platform: "wecom_app", Code: "invalid_media_response"}
		} else {
			if response.Code != nil && *response.Code != 0 {
				return gateway.MediaContent{}, a.mediaError(token, *response.Code)
			}
			if response.VideoURL != "" {
				if resource.kind != "video" {
					return gateway.MediaContent{}, &gateway.APIError{Platform: "wecom_app", Code: "unexpected_video_locator"}
				}
				data, headers, err = wire.DownloadMedia(ctx, a.cfg.HTTPClient, "wecom_app", response.VideoURL, nil, maxMediaBytes)
				if err != nil {
					return gateway.MediaContent{}, err
				}
			} else {
				return gateway.MediaContent{}, &gateway.APIError{Platform: "wecom_app", Code: "missing_media_content"}
			}
		}
	}
	name := resource.name
	if name == "" {
		_, params, _ := mime.ParseMediaType(headers.Get("Content-Disposition"))
		name = params["filename"]
	}
	contentType, _, _ = mime.ParseMediaType(headers.Get("Content-Type"))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	return gateway.MediaContent{Data: data, Name: name, MIME: contentType}, nil
}

// Upload creates a temporary application media resource using multipart/form-data.
func (a *Adapter) Upload(ctx context.Context, r gateway.Route, u gateway.MediaUpload) (gateway.Attachment, error) {
	if err := a.mediaRoute(r); err != nil {
		return gateway.Attachment{}, err
	}
	if err := u.Validate(); err != nil {
		return gateway.Attachment{}, err
	}
	switch u.Kind {
	case "image", "voice", "video", "file":
	default:
		return gateway.Attachment{}, gateway.ErrUnsupported
	}
	if len(u.Data) > maxMediaBytes {
		return gateway.Attachment{}, gateway.ErrMediaTooLarge
	}
	if u.Name == "" {
		return gateway.Attachment{}, gateway.ErrInvalidEvent
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.Attachment{}, err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("media", u.Name)
	if err != nil {
		return gateway.Attachment{}, err
	}
	if _, err = part.Write(u.Data); err != nil {
		return gateway.Attachment{}, err
	}
	if err = mw.Close(); err != nil {
		return gateway.Attachment{}, err
	}
	endpoint := a.cfg.APIBase + "/cgi-bin/media/upload?" + url.Values{"access_token": {token}, "type": {u.Kind}}.Encode()
	data, _, err := wire.MediaHTTP(ctx, a.cfg.HTTPClient, "wecom_app", http.MethodPost, endpoint, http.Header{"Content-Type": {mw.FormDataContentType()}}, &body, wire.MaxBody)
	if err != nil {
		return gateway.Attachment{}, a.apiError(token, err)
	}
	var response struct {
		Code *int   `json:"errcode"`
		ID   string `json:"media_id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &response) != nil {
		return gateway.Attachment{}, &gateway.APIError{Platform: "wecom_app", Code: "invalid_media_response", Uncertain: true}
	}
	if response.Code != nil && *response.Code != 0 {
		return gateway.Attachment{}, a.mediaError(token, *response.Code)
	}
	if response.ID == "" || (response.Type != "" && response.Type != u.Kind) {
		return gateway.Attachment{}, &gateway.APIError{Platform: "wecom_app", Code: "invalid_media_result", Uncertain: true}
	}
	// The complete success response is authoritative even if cancellation races
	// with its final read; retain the media ID instead of inviting another upload.
	p := gateway.Attachment{Kind: u.Kind, Ref: uploadRefPrefix + rand.Text(), Name: u.Name, MIME: u.MIME, Size: int64(len(u.Data)), Scope: &r, ExpiresAt: time.Now().Add(30 * time.Minute)}
	a.media.Put(wire.ContextKey(r, p.Ref), mediaResource{id: response.ID, kind: u.Kind, name: u.Name}, p.ExpiresAt)
	return p, nil
}
