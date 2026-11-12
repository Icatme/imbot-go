package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxMediaBytes is the SDK memory bound, not a claim about platform quotas.
const MaxMediaBytes int64 = 100 << 20

var (
	ErrMediaExpired  = errors.New("gateway: media reference expired or unavailable")
	ErrMediaTooLarge = errors.New("gateway: media exceeds size limit")
)

// MediaContent contains actual downloaded bytes. Persistence and document/audio
// interpretation belong to the host. Data must not be included in ordinary logs.
type MediaContent struct {
	Data []byte
	Name string
	MIME string
}

// MediaUpload supplies explicit bytes, never an arbitrary URL or filesystem path.
// ReplyTo binds uploads to a reply context on platforms that require one.
// DurationMS is required by some native audio/video protocols; no transcoding is implicit.
type MediaUpload struct {
	Kind, Name, MIME, ReplyTo string
	Data                      []byte
	DurationMS                int64
}

func (u MediaUpload) Validate() error {
	switch u.Kind {
	case "image", "file", "audio", "voice", "video":
	default:
		return ErrUnsupported
	}
	if len(u.Data) == 0 || u.DurationMS < 0 || strings.ContainsAny(u.Name+u.MIME, "\r\n\x00") {
		return ErrInvalidEvent
	}
	if int64(len(u.Data)) > MaxMediaBytes {
		return ErrMediaTooLarge
	}
	return nil
}

// ValidateFor rejects accidental cross-route reuse. Platform adapters must also
// verify their private locator cache when a reference contains credentials or
// is otherwise meaningful only in an observed message/connection.
func (a Attachment) ValidateFor(r Route) error {
	if r.Validate() != nil || a.Ref == "" || (a.Scope != nil && *a.Scope != r) {
		return ErrInvalidEvent
	}
	if !a.ExpiresAt.IsZero() && !time.Now().Before(a.ExpiresAt) {
		return ErrMediaExpired
	}
	return nil
}

type Downloader interface {
	Download(context.Context, Route, Attachment) (MediaContent, error)
}

type Uploader interface {
	Upload(context.Context, Route, MediaUpload) (Attachment, error)
}

// Download is an explicit trusted-host operation, like Send; it does not replace
// business authorization. Cached sensitive locators are process-local. A host
// needing durable consumption must persist the bytes before acknowledging its Sink.
func (g *Gateway) Download(ctx context.Context, r Route, p Attachment) (MediaContent, error) {
	if err := ctx.Err(); err != nil {
		return MediaContent{}, err
	}
	if err := p.ValidateFor(r); err != nil {
		return MediaContent{}, err
	}
	a, err := g.adapter(r.Account)
	if err != nil {
		return MediaContent{}, err
	}
	d, ok := a.(Downloader)
	if !ok || !a.Capabilities().DownloadMedia {
		return MediaContent{}, ErrUnsupported
	}
	result, err := d.Download(ctx, r, p)
	if err != nil {
		return MediaContent{}, err
	}
	if int64(len(result.Data)) > MaxMediaBytes {
		return MediaContent{}, ErrMediaTooLarge
	}
	return result, nil
}

func (g *Gateway) Upload(ctx context.Context, r Route, u MediaUpload) (Attachment, error) {
	if err := ctx.Err(); err != nil {
		return Attachment{}, err
	}
	if r.Validate() != nil {
		return Attachment{}, ErrInvalidEvent
	}
	if err := u.Validate(); err != nil {
		return Attachment{}, err
	}
	a, err := g.adapter(r.Account)
	if err != nil {
		return Attachment{}, err
	}
	up, ok := a.(Uploader)
	if !ok || !a.Capabilities().UploadMedia {
		return Attachment{}, ErrUnsupported
	}
	result, err := up.Upload(ctx, r, u)
	if err != nil {
		return Attachment{}, err
	}
	if result.Scope == nil {
		scope := r
		result.Scope = &scope
	}
	if err := result.ValidateFor(r); err != nil {
		return Attachment{}, err
	}
	return result, nil
}

// UnsupportedMessageError makes intentionally consumed unknown formats observable
// without causing unbounded redelivery. It contains no message body or credentials.
type UnsupportedMessageError struct{ Platform, MessageType string }

func (e *UnsupportedMessageError) Error() string {
	typ := e.MessageType
	if len(typ) > 128 {
		typ = typ[:128]
	}
	return fmt.Sprintf("%s: unsupported inbound message type %q", e.Platform, typ)
}
func (e *UnsupportedMessageError) Unwrap() error { return ErrUnsupported }
