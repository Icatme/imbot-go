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

	wechatbot "github.com/Icatme/wechatbot-go"
	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

type client interface {
	Handle(wechatbot.MessageHandler)
	Run(context.Context) error
	ReplyMessage(context.Context, *wechatbot.IncomingMessage, wechatbot.OutboundMessage) (wechatbot.SendResult, error)
	SendMessage(context.Context, string, wechatbot.OutboundMessage) (wechatbot.SendResult, error)
}

var _ client = (*wechatbot.Bot)(nil)

type Config struct {
	AccountID            string
	Bot                  *wechatbot.Bot // already configured/logged in by the application
	TakeHandlerOwnership bool           // required: Run installs the sole native handler
	EnableProactive      bool           // still requires the original SDK's valid peer context
	ReplyTTL             time.Duration
}
type Adapter struct {
	cfg     Config
	bot     client
	guard   wire.Guard
	replies *wire.Cache[*wechatbot.IncomingMessage]
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
	return &Adapter{cfg: c, bot: c.Bot, replies: wire.NewCache[*wechatbot.IncomingMessage](4096)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "weixin", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{Text: true, Reply: true, ReceiveMedia: true, Proactive: a.cfg.EnableProactive}
}
func (a *Adapter) Run(ctx context.Context, sink gateway.Sink) error {
	if sink == nil {
		return errors.New("weixin: nil sink")
	}
	if err := a.guard.Enter(); err != nil {
		return err
	}
	defer a.guard.Leave()
	a.bot.Handle(wechatbot.MessageHandlerFunc(func(ctx context.Context, m *wechatbot.IncomingMessage) wechatbot.MessageResult {
		return a.handle(ctx, m, sink)
	}))
	return a.bot.Run(ctx) // preserve native reauthentication, cancellation and retry errors
}
func (a *Adapter) handle(ctx context.Context, m *wechatbot.IncomingMessage, sink gateway.Sink) wechatbot.MessageResult {
	e, err := a.normalize(m)
	if err == nil {
		err = sink(ctx, e)
	}
	if err == nil {
		return wechatbot.AckMessage()
	}
	if errors.Is(err, gateway.ErrUnauthorized) || errors.Is(err, gateway.ErrUnsupported) {
		return wechatbot.DropMessage(err)
	}
	return wechatbot.RetryMessage(err)
}
func (a *Adapter) normalize(m *wechatbot.IncomingMessage) (gateway.Event, error) {
	if m == nil || m.Raw == nil || m.UserID == "" {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	id := string(m.Raw.MessageID)
	if id == "" {
		for _, item := range m.Raw.ItemList {
			if item.MsgID != "" {
				id = string(item.MsgID)
				break
			}
		}
	}
	if id == "" {
		raw, err := json.Marshal(m.Raw)
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
	e := gateway.Event{ID: id, MessageID: id, Route: r, SenderID: m.UserID, Time: m.Timestamp, Native: &native}
	var text []string
	for _, p := range m.Raw.ItemList {
		if p.Type == wechatbot.ItemText && p.TextItem != nil {
			text = append(text, p.TextItem.Text)
		}
		if p.Type == wechatbot.ItemVoice && p.VoiceItem != nil && p.VoiceItem.Text != "" {
			text = append(text, p.VoiceItem.Text)
		}
	}
	// Native m.Text can contain signed image URLs; do not use it for media messages.
	if len(m.Raw.ItemList) == 0 && len(m.Images)+len(m.Voices)+len(m.Files)+len(m.Videos) == 0 {
		text = append(text, m.Text)
	}
	e.Text = strings.Join(text, "\n")
	add := func(kind string, index int, name string, size int64) {
		e.Attachments = append(e.Attachments, gateway.Attachment{Kind: kind, Ref: id + ":" + kind + ":" + strconv.Itoa(index), Name: name, Size: size})
	}
	for i := range m.Images {
		add("image", i, "", 0)
	}
	for i := range m.Voices {
		add("voice", i, "", 0)
	}
	for i, p := range m.Files {
		add("file", i, p.FileName, p.Size)
	}
	for i := range m.Videos {
		add("video", i, "", 0)
	}
	if m.QuotedMessage != nil {
		e.ReplyTo = string(m.QuotedMessage.MessageID)
	}
	if e.Text == "" && len(e.Attachments) == 0 {
		return gateway.Event{}, gateway.ErrUnsupported
	}
	a.replies.Put(wire.ContextKey(r, id), &saved, time.Now().Add(a.cfg.ReplyTTL))
	return e, nil
}
func (a *Adapter) Send(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if err := wire.CheckSend(a.Account(), r, m); err != nil {
		return gateway.Receipt{}, err
	}
	if r.TenantID != "" || r.Kind != "direct" || m.Format == "markdown" || len(m.Card) > 0 || len(m.Attachments) > 0 {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	outbound := wechatbot.OutboundMessage{ClientID: m.ClientID, Item: wechatbot.MessageItem{Type: wechatbot.ItemText, TextItem: &wechatbot.TextItem{Text: m.Text}}}
	var result wechatbot.SendResult
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
