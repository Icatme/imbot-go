// Package weixin adapts the existing iLink SDK; it does not replace its state machine.
package weixin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Icatme/imbot-go"
	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

type client interface {
	Handle(imbot.MessageHandler)
	Run(context.Context) error
	ReplyMessage(context.Context, *imbot.IncomingMessage, imbot.OutboundMessage) (imbot.SendResult, error)
	SendMessage(context.Context, string, imbot.OutboundMessage) (imbot.SendResult, error)
	DownloadRaw(context.Context, *imbot.CDNMedia, string) ([]byte, error)
	DownloadImage(context.Context, imbot.ImageContent) ([]byte, error)
	DownloadQuoted(context.Context, *imbot.IncomingMessage) (*imbot.DownloadedMedia, error)
	Upload(context.Context, []byte, string, int) (*imbot.UploadResult, error)
	UploadForReply(context.Context, *imbot.IncomingMessage, []byte, int) (*imbot.UploadResult, error)
}

var _ client = (*imbot.Bot)(nil)

type Config struct {
	AccountID            string
	Bot                  *imbot.Bot // already configured/logged in by the application
	TakeHandlerOwnership bool       // required: Run installs the sole native handler
	EnableProactive      bool       // still requires the original SDK's valid peer context
	ReplyTTL             time.Duration
	OnError              func(error)
}
type Adapter struct {
	cfg     Config
	bot     client
	guard   wire.Guard
	replies *wire.Cache[*imbot.IncomingMessage]
	media   *wire.Cache[mediaEntry]
}

func New(c Config) (*Adapter, error) {
	if c.AccountID == "" || c.Bot == nil || !c.TakeHandlerOwnership {
		return nil, errors.New("weixin: dedicated Bot, account id and explicit handler ownership required")
	}
	if c.ReplyTTL <= 0 {
		c.ReplyTTL = 5 * time.Minute
	}
	if c.ReplyTTL > time.Hour {
		return nil, errors.New("weixin: reply TTL exceeds one hour")
	}
	return &Adapter{cfg: c, bot: c.Bot, replies: wire.NewCache[*imbot.IncomingMessage](4096), media: wire.NewCache[mediaEntry](4096)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "weixin", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{Text: true, Reply: true, ReceiveMedia: true, SendMedia: true, DownloadMedia: true, UploadMedia: true, Proactive: a.cfg.EnableProactive, MaxTextBytes: 2000}
}
func (a *Adapter) Run(ctx context.Context, sink gateway.Sink) error {
	if sink == nil {
		return errors.New("weixin: nil sink")
	}
	if err := a.guard.Enter(); err != nil {
		return err
	}
	defer a.guard.Leave()
	a.bot.Handle(imbot.MessageHandlerFunc(func(ctx context.Context, m *imbot.IncomingMessage) imbot.MessageResult {
		return a.handle(ctx, m, sink)
	}))
	return a.bot.Run(ctx) // preserve native reauthentication, cancellation and retry errors
}
func (a *Adapter) handle(ctx context.Context, m *imbot.IncomingMessage, sink gateway.Sink) imbot.MessageResult {
	e, err := a.normalize(m)
	if errors.Is(err, gateway.ErrUnsupported) {
		wire.ReportUnsupported(err, a.cfg.OnError)
	}
	if err == nil {
		err = sink(ctx, e)
	}
	if err == nil {
		return imbot.AckMessage()
	}
	if errors.Is(err, gateway.ErrUnauthorized) || errors.Is(err, gateway.ErrUnsupported) {
		return imbot.DropMessage(err)
	}
	return imbot.RetryMessage(err)
}
func (a *Adapter) normalize(m *imbot.IncomingMessage) (gateway.Event, error) {
	if m == nil || m.Raw == nil || m.UserID == "" {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	id := string(m.Raw.MessageID)
	if id == "0" {
		id = ""
	}
	if id == "" {
		for _, item := range m.Raw.ItemList {
			if item.MsgID != "" && item.MsgID != "0" {
				id = string(item.MsgID)
				break
			}
		}
	}
	if id == "" && m.Raw.ClientID != "" {
		id = "client:" + m.Raw.ClientID
	}
	if id == "" && m.Raw.Seq != 0 {
		id = "seq:" + strconv.FormatInt(m.Raw.Seq, 10)
	}
	if id == "" {
		// A renewed reply credential must not turn redelivery into a new event.
		identity := *m.Raw
		identity.ContextToken = ""
		raw, err := json.Marshal(identity)
		if err != nil {
			return gateway.Event{}, err
		}
		sum := sha256.Sum256(raw)
		id = "sha256:" + hex.EncodeToString(sum[:])
	}
	r := gateway.Route{Account: a.Account(), ChatID: m.UserID, ThreadID: m.SessionID, Kind: "direct"}
	if m.GroupID != "" {
		r.Kind = "group"
		r.ChatID = m.GroupID
	}
	// Copy the full native object so unexported session generation remains intact.
	// Native is a separate shallow copy; mutating its scalar routing fields cannot
	// change the cached reply target/context token.
	saved, native := *m, *m
	if m.Raw != nil {
		raw := *m.Raw
		saved.Raw = &raw
	}
	e := gateway.Event{ID: id, MessageID: id, Route: r, SenderID: m.UserID, Time: m.Timestamp, Native: &native}
	var text []string
	for _, p := range m.Raw.ItemList {
		if p.Type == imbot.ItemText && p.TextItem != nil {
			text = append(text, p.TextItem.Text)
		}
		if p.Type == imbot.ItemVoice && p.VoiceItem != nil && p.VoiceItem.Text != "" {
			text = append(text, p.VoiceItem.Text)
		}
	}
	// Native m.Text can contain signed image URLs; do not use it for media messages.
	if len(m.Raw.ItemList) == 0 && len(m.Images)+len(m.Voices)+len(m.Files)+len(m.Videos) == 0 {
		text = append(text, m.Text)
	}
	e.Text = strings.Join(text, "\n")
	add := func(kind string, index int, name string, size int64) {
		e.Attachments = append(e.Attachments, a.rememberMedia(r, id, kind, index, name, size, m))
	}
	for i, image := range m.Images {
		if (image.Media != nil && !hasMediaLocator(image.Media)) || (image.Media == nil && image.URL == "") {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
		add("image", i, "", 0)
	}
	for i, voice := range m.Voices {
		if voice.Media == nil {
			if voice.Text == "" {
				return gateway.Event{}, gateway.ErrInvalidEvent
			}
			continue // the platform supplied a transcript without an audio resource
		}
		if !hasMediaLocator(voice.Media) {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
		add("voice", i, "", 0)
	}
	for i, p := range m.Files {
		if !hasMediaLocator(p.Media) {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
		add("file", i, p.FileName, p.Size)
	}
	for i, video := range m.Videos {
		if !hasMediaLocator(video.Media) {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
		add("video", i, "", 0)
	}
	if m.QuotedMessage != nil {
		e.ReplyTo = string(m.QuotedMessage.MessageID)
		e.Quote = &gateway.Quote{MessageID: e.ReplyTo, Text: m.QuotedMessage.Text}
		if q := m.QuotedMessage.Media; q != nil {
			p := a.rememberMedia(r, id, "quote", 0, q.FileName, q.Size, m)
			p.Kind, p.MIME = string(q.Type), q.MIMEType
			e.Quote.Attachments = []gateway.Attachment{p}
		}
	}
	if e.Text == "" && len(e.Attachments) == 0 {
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "weixin", MessageType: string(m.Type)}
	}
	e.MessageType = string(m.Type)
	a.addParts(&e, m)
	a.replies.Put(wire.ContextKey(r, id), &saved, time.Now().Add(a.cfg.ReplyTTL))
	return e, nil
}
func (a *Adapter) Send(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if err := wire.CheckSend(a.Account(), r, m); err != nil {
		return gateway.Receipt{}, err
	}
	if r.TenantID != "" || r.Kind != "direct" || m.Format == "markdown" || len(m.Card) > 0 {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	outbound := imbot.OutboundMessage{ClientID: m.ClientID, Item: imbot.MessageItem{Type: imbot.ItemText, TextItem: &imbot.TextItem{Text: m.Text}}}
	if len(m.Attachments) > 0 {
		if len(m.Attachments) != 1 || m.Text != "" {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		entry, ok := a.media.Get(wire.ContextKey(r, m.Attachments[0].Ref))
		if !ok || entry.outbound == nil {
			return gateway.Receipt{}, gateway.ErrMediaExpired
		}
		if entry.replyTo != m.ReplyTo {
			return gateway.Receipt{}, gateway.ErrInvalidEvent
		}
		outbound = entry.copyOutbound(m.ClientID)
	}
	var result imbot.SendResult
	var err error
	if m.ReplyTo != "" {
		native, ok := a.replies.Get(wire.ContextKey(r, m.ReplyTo))
		if !ok {
			return gateway.Receipt{}, gateway.ErrReplyExpired
		}
		outbound.RunID = native.RunID
		result, err = a.bot.ReplyMessage(ctx, native, outbound)
	} else {
		if !a.cfg.EnableProactive || r.ThreadID != "" {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		result, err = a.bot.SendMessage(ctx, r.ChatID, outbound)
	}
	if err != nil {
		return gateway.Receipt{}, err
	}
	return gateway.Receipt{MessageID: string(result.MessageID), Accepted: true}, nil
}
