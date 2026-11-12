package feishu

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

const imageUploadLimit = 10 << 20
const fileUploadLimit = 30 << 20

func (a *Adapter) mediaAPIError(token string, err error) error {
	var api *gateway.APIError
	if errors.As(err, &api) && (api.Status == http.StatusUnauthorized || api.Code == "99991663" || api.Code == "99991665" || api.Code == "99991668") {
		a.token.Invalidate(token)
	}
	return err
}

// net/http's content sniffer does not recognize TIFF or HEIC, both accepted by
// Feishu's image endpoint. Inspect their container signatures without decoding or
// converting them; full format and resolution validation remains platform-owned.
func imageContentType(data []byte) string {
	typ := http.DetectContentType(data)
	if strings.HasPrefix(typ, "image/") {
		return typ
	}
	if len(data) >= 8 && (bytes.Equal(data[:4], []byte{'I', 'I', 42, 0}) || bytes.Equal(data[:4], []byte{'M', 'M', 0, 42})) {
		return "image/tiff"
	}
	if len(data) >= 16 && string(data[4:8]) == "ftyp" {
		size := int64(binary.BigEndian.Uint32(data[:4]))
		if size >= 16 && size <= int64(len(data)) {
			for i := int64(8); i+4 <= size; i += 4 {
				if i == 12 {
					continue
				} // minor version, not a compatible brand
				switch string(data[i : i+4]) {
				case "heic", "heix", "hevc", "hevx":
					return "image/heic"
				}
			}
		}
	}
	return ""
}

// Download fetches an observed message resource with this bot's tenant token.
// The private lookup binds the resource to its message and complete route.
func (a *Adapter) Download(ctx context.Context, r gateway.Route, p gateway.Attachment) (gateway.MediaContent, error) {
	if r.Account != a.Account() {
		return gateway.MediaContent{}, gateway.ErrInvalidEvent
	}
	if err := p.ValidateFor(r); err != nil {
		return gateway.MediaContent{}, err
	}
	resource, ok := a.resources.Get(wire.ContextKey(r, p.MessageID+":"+p.Ref))
	if !ok {
		return gateway.MediaContent{}, gateway.ErrMediaExpired
	}
	kind := "file"
	switch resource.Kind {
	case "image":
		kind = "image"
	case "file", "audio", "voice", "video", "media":
	default:
		return gateway.MediaContent{}, gateway.ErrUnsupported
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.MediaContent{}, err
	}
	endpoint := a.cfg.BaseURL + "/open-apis/im/v1/messages/" + wire.PathID(resource.MessageID) + "/resources/" + wire.PathID(resource.Key) + "?type=" + kind
	data, headers, err := a.apiHTTP(ctx, token, http.MethodGet, endpoint, wire.Bearer(token), nil, gateway.MaxMediaBytes)
	if err != nil {
		return gateway.MediaContent{}, err
	}
	name := p.Name
	if _, params, err := mime.ParseMediaType(headers.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	return gateway.MediaContent{Data: data, Name: name, MIME: headers.Get("Content-Type")}, nil
}

// Upload sends bytes through the platform multipart API. Native audio must
// already be Opus and native video MP4; no format conversion or fallback occurs.
func (a *Adapter) Upload(ctx context.Context, r gateway.Route, u gateway.MediaUpload) (gateway.Attachment, error) {
	if r.Account != a.Account() || r.Validate() != nil {
		return gateway.Attachment{}, gateway.ErrInvalidEvent
	}
	if err := u.Validate(); err != nil {
		return gateway.Attachment{}, err
	}
	if u.Name == "" {
		return gateway.Attachment{}, gateway.ErrInvalidEvent
	}
	if len(u.Data) > fileUploadLimit || u.Kind == "image" && len(u.Data) > imageUploadLimit {
		return gateway.Attachment{}, gateway.ErrMediaTooLarge
	}
	typ := "stream"
	path, field := "/open-apis/im/v1/files", "file"
	switch u.Kind {
	case "image":
		if imageContentType(u.Data) == "" {
			return gateway.Attachment{}, gateway.ErrInvalidEvent
		}
		path, field = "/open-apis/im/v1/images", "image"
	case "audio", "voice":
		if len(u.Data) < 36 || string(u.Data[:4]) != "OggS" || !bytes.Contains(u.Data[:min(len(u.Data), 256)], []byte("OpusHead")) {
			return gateway.Attachment{}, gateway.ErrUnsupported
		}
		typ = "opus"
	case "video":
		if len(u.Data) < 12 || string(u.Data[4:8]) != "ftyp" {
			return gateway.Attachment{}, gateway.ErrUnsupported
		}
		typ = "mp4"
	case "file":
		switch strings.ToLower(filepath.Ext(u.Name)) {
		case ".pdf":
			typ = "pdf"
		case ".doc", ".docx":
			typ = "doc"
		case ".xls", ".xlsx":
			typ = "xls"
		case ".ppt", ".pptx":
			typ = "ppt"
		}
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fields := map[string]string{"file_type": typ, "file_name": u.Name}
	if u.Kind == "image" {
		fields = map[string]string{"image_type": "message"}
	} else if u.DurationMS > 0 {
		fields["duration"] = strconv.FormatInt(u.DurationMS, 10)
	}
	for key, value := range fields {
		if err := w.WriteField(key, value); err != nil {
			return gateway.Attachment{}, err
		}
	}
	part, err := w.CreateFormFile(field, u.Name)
	if err != nil {
		return gateway.Attachment{}, err
	}
	if _, err = part.Write(u.Data); err != nil {
		return gateway.Attachment{}, err
	}
	if err = w.Close(); err != nil {
		return gateway.Attachment{}, err
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.Attachment{}, err
	}
	headers := wire.Bearer(token)
	headers.Set("Content-Type", w.FormDataContentType())
	data, _, err := a.apiHTTP(ctx, token, http.MethodPost, a.cfg.BaseURL+path, headers, &body, 64<<10)
	if err != nil {
		return gateway.Attachment{}, err
	}
	var out struct {
		Code *int `json:"code"`
		Data struct {
			ImageKey string `json:"image_key"`
			FileKey  string `json:"file_key"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &out) != nil || out.Code == nil {
		return gateway.Attachment{}, &gateway.APIError{Platform: "feishu", Code: "invalid_upload_response", Uncertain: true}
	}
	if err = wire.Business("feishu", *out.Code); err != nil {
		return gateway.Attachment{}, a.mediaAPIError(token, err)
	}
	key := out.Data.FileKey
	if u.Kind == "image" {
		key = out.Data.ImageKey
	}
	if key == "" {
		return gateway.Attachment{}, &gateway.APIError{Platform: "feishu", Code: "missing_media_key", Uncertain: true}
	}
	mediaType := u.MIME
	if mediaType == "" {
		if u.Kind == "image" {
			mediaType = imageContentType(u.Data)
		} else {
			mediaType = http.DetectContentType(u.Data)
		}
	}
	p := gateway.Attachment{Kind: u.Kind, Ref: uploadRefPrefix + rand.Text(), Name: u.Name, MIME: mediaType, Size: int64(len(u.Data)), Scope: &r, ExpiresAt: time.Now().Add(30 * time.Minute)}
	a.uploads.Put(wire.ContextKey(r, p.Ref), uploadedResource{Key: key, Kind: u.Kind}, p.ExpiresAt)
	return p, nil
}
