// Package gateway normalizes messaging transports without depending on an Agent runtime.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Account identifies one configured bot, not a credential or a human identity.
type Account struct {
	Platform string `json:"platform"`
	ID       string `json:"account_id"`
}

func (a Account) Key() string { return key(a.Platform, a.ID) }
func (a Account) Validate() error {
	if strings.TrimSpace(a.Platform) == "" || strings.TrimSpace(a.ID) == "" {
		return ErrInvalidEvent
	}
	return nil
}

// Route scopes conversations across bot accounts, organizations and threads.
type Route struct {
	Account
	TenantID string `json:"tenant_id,omitempty"`
	ChatID   string `json:"chat_id"`
	ThreadID string `json:"thread_id,omitempty"`
	Kind     string `json:"kind"` // direct, group or channel
}

func (r Route) SessionKey() string {
	return key(r.Platform, r.ID, r.TenantID, r.Kind, r.ChatID, r.ThreadID)
}
func (r Route) Validate() error {
	if r.Account.Validate() != nil || strings.TrimSpace(r.ChatID) == "" {
		return ErrInvalidEvent
	}
	return nil
}
func key(parts ...string) string { b, _ := json.Marshal(parts); return string(b) }

// Attachment is a descriptor, never an instruction to fetch an arbitrary URL.
// Ref is platform-owned (image_key, media_id, downloadCode, etc.).
type Attachment struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// Event contains only normalized, persistable metadata. Native may contain secrets;
// it is deliberately excluded from JSON and must never be logged blindly.
type Event struct {
	ID          string       `json:"id"`
	MessageID   string       `json:"message_id"`
	Route       Route        `json:"route"`
	SenderID    string       `json:"sender_id"`
	SenderIsBot bool         `json:"sender_is_bot,omitempty"`
	Mentioned   bool         `json:"mentioned,omitempty"`
	Text        string       `json:"text,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	ReplyTo     string       `json:"reply_to,omitempty"`
	Time        time.Time    `json:"time"`
	Native      any          `json:"-"`
}

func (e Event) Reply(text string) Message { return Message{Text: text, ReplyTo: e.MessageID} }

// Message represents one outbound operation. ClientID is an application-generated
// idempotency key where the platform supports it; it is not an exactly-once promise.
type Message struct {
	Text        string          `json:"text,omitempty"`
	Format      string          `json:"format,omitempty"` // empty/text, markdown
	ReplyTo     string          `json:"reply_to,omitempty"`
	ClientID    string          `json:"client_id,omitempty"`
	Sequence    int             `json:"sequence,omitempty"` // QQ reply sequence; default 1
	Attachments []Attachment    `json:"attachments,omitempty"`
	Card        json.RawMessage `json:"card,omitempty"`
}

// Receipt means the platform accepted the request, not that a person read it.
type Receipt struct {
	MessageID string `json:"message_id,omitempty"`
	Accepted  bool   `json:"accepted"`
}
type Capabilities struct{ Text, Markdown, Reply, ReceiveMedia, SendMedia, Card, Stream, Proactive bool }

// Sink is a bounded-latency admission callback. Return nil only after completing
// processing OR durably accepting the event. Do not run a slow LLM inside a webhook.
type Sink func(context.Context, Event) error
type Authorizer func(context.Context, Event) error

// Adapter implementations own transport authentication. Sink must only receive
// authenticated events, with Account populated from configuration, never from input.
type Adapter interface {
	Account() Account
	Capabilities() Capabilities
	Run(context.Context, Sink) error
	Send(context.Context, Route, Message) (Receipt, error)
}

// StreamSender is optional: StreamID stays stable and Content is a full snapshot.
type StreamUpdate struct {
	ReplyTo, StreamID, Content string
	Finished                   bool
}
type StreamSender interface {
	UpdateStream(context.Context, Route, StreamUpdate) (Receipt, error)
}

var (
	ErrUnauthorized   = errors.New("gateway: unauthorized")
	ErrInvalidEvent   = errors.New("gateway: invalid event or route")
	ErrUnknownAccount = errors.New("gateway: unknown account")
	ErrUnsupported    = errors.New("gateway: unsupported capability")
	ErrBusy           = errors.New("gateway: capacity exhausted")
	ErrAlreadyRunning = errors.New("gateway: already running")
	ErrReplyExpired   = errors.New("gateway: reply context expired; do not silently send to a different target")
)

// Consumed distinguishes deliberate policy drops from retryable admission failures.
func Consumed(err error) bool { return err == nil || errors.Is(err, ErrUnauthorized) }

// AllowAll must be selected explicitly. Production applications should supply
// their own account/tenant/user authorization policy instead.
func AllowAll(context.Context, Event) error { return nil }

// APIError deliberately excludes request URLs, credentials and response bodies.
type APIError struct {
	Platform             string
	Status               int
	Code                 string
	Retryable, Uncertain bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s API failure (HTTP %d, code %s, delivery uncertain=%t)", e.Platform, e.Status, e.Code, e.Uncertain)
}
