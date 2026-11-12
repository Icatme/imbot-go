// Package receive defines transport-independent inbound content policies.
// Policies inspect descriptors, never download media or validate file contents.
package receive

import (
	"errors"
	"fmt"
	"mime"
	"path"
	"slices"
	"strings"
)

// Kind describes the original content, including voice messages whose only
// available representation is a transcript. Audio attachments count as Voice.
type Kind string

const (
	Text        Kind = "text"
	Image       Kind = "image"
	Voice       Kind = "voice"
	Video       Kind = "video"
	File        Kind = "file"
	Interaction Kind = "interaction"
	ToolCall    Kind = "tool_call"
)

// Policy's zero value accepts all content supported by the transport. It cannot
// enable unsupported platform features. Rejection applies to the entire message,
// including its quotes; no partial text/attachment rewriting is performed.
type Policy struct {
	Disabled []Kind
	// FileExtensions is a case-insensitive allowlist for File names. Entries may
	// start with a dot (".pdf" or "pdf"). Empty means unrestricted. An active list
	// rejects missing names. Only the final extension is matched.
	FileExtensions []string
	// FileMIMETypes is an exact, case-insensitive allowlist (no wildcards).
	// Parameters in platform MIME metadata are ignored. An active list rejects
	// missing/invalid MIME metadata. Both allowlists must pass when configured.
	FileMIMETypes []string
}

// Attachment contains metadata only.
type Attachment struct {
	Kind Kind
	Name string
	MIME string
}

// Content is the semantic projection of a message plus its quoted content.
// Kinds and attachment descriptors may repeat across content projections.
type Content struct {
	Kinds       []Kind
	Attachments []Attachment
}

var (
	ErrRejected      = errors.New("receive: content rejected by policy")
	ErrInvalidPolicy = errors.New("receive: invalid policy")
)

type Reason string

const (
	DisabledKind  Reason = "disabled_kind"
	FileExtension Reason = "file_extension"
	FileMIME      Reason = "file_mime"
)

// Rejection is a deliberate, consumable drop, not a transient transport error.
// It contains no filenames, message text, resource locators or credentials.
type Rejection struct {
	Reason Reason
	Kind   Kind
}

func (r *Rejection) Error() string {
	return fmt.Sprintf("receive: rejected content (%s, %s)", r.Reason, r.Kind)
}
func (r *Rejection) Unwrap() error { return ErrRejected }

// Clone isolates the configuration from later caller slice mutations.
func (p Policy) Clone() Policy {
	p.Disabled = slices.Clone(p.Disabled)
	p.FileExtensions = slices.Clone(p.FileExtensions)
	p.FileMIMETypes = slices.Clone(p.FileMIMETypes)
	return p
}

func (p Policy) Validate() error {
	for _, kind := range p.Disabled {
		switch kind {
		case Text, Image, Voice, Video, File, Interaction, ToolCall:
		default:
			return fmt.Errorf("%w: unknown disabled kind", ErrInvalidPolicy)
		}
	}
	for _, ext := range p.FileExtensions {
		ext = strings.TrimPrefix(strings.TrimSpace(ext), ".")
		if ext == "" || strings.ContainsAny(ext, ". /\\\t\r\n\x00*?:<>|\"") {
			return fmt.Errorf("%w: invalid file extension", ErrInvalidPolicy)
		}
	}
	for _, value := range p.FileMIMETypes {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil || len(params) != 0 || !strings.Contains(mediaType, "/") || strings.Contains(mediaType, "*") {
			return fmt.Errorf("%w: invalid file MIME type", ErrInvalidPolicy)
		}
	}
	return nil
}

// Check evaluates metadata without modifying it. Invalid configuration is never
// reported as a consumable rejection. Callers should Validate before receiving.
func (p Policy) Check(c Content) error {
	if err := p.Validate(); err != nil {
		return err
	}
	for _, kind := range c.Kinds {
		if slices.Contains(p.Disabled, kind) {
			return &Rejection{Reason: DisabledKind, Kind: kind}
		}
	}
	if slices.Contains(c.Kinds, File) && !slices.ContainsFunc(c.Attachments, func(a Attachment) bool { return a.Kind == File }) {
		if len(p.FileExtensions) > 0 {
			return &Rejection{Reason: FileExtension, Kind: File}
		}
		if len(p.FileMIMETypes) > 0 {
			return &Rejection{Reason: FileMIME, Kind: File}
		}
	}
	for _, a := range c.Attachments {
		if slices.Contains(p.Disabled, a.Kind) {
			return &Rejection{Reason: DisabledKind, Kind: a.Kind}
		}
		if a.Kind != File {
			continue
		}
		if len(p.FileExtensions) > 0 {
			ext := path.Ext(strings.ReplaceAll(a.Name, "\\", "/"))
			if strings.ContainsAny(a.Name, "\x00\r\n") || !slices.ContainsFunc(p.FileExtensions, func(v string) bool {
				return strings.EqualFold(ext, "."+strings.TrimPrefix(strings.TrimSpace(v), "."))
			}) {
				return &Rejection{Reason: FileExtension, Kind: File}
			}
		}
		if len(p.FileMIMETypes) > 0 {
			mediaType, _, err := mime.ParseMediaType(a.MIME)
			if err != nil || !slices.ContainsFunc(p.FileMIMETypes, func(v string) bool {
				return strings.EqualFold(mediaType, strings.TrimSpace(v))
			}) {
				return &Rejection{Reason: FileMIME, Kind: File}
			}
		}
	}
	return nil
}
