// Package wecom implements the Enterprise WeChat smart-robot WebSocket protocol.
// Self-built applications use the separate wecomapp package.
package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

type Config struct {
	AccountID, BotID, Secret, TenantID, WebSocketURL string
	HTTPClient                                       *http.Client
	OnError                                          func(error)
}
type replyContext struct {
	session   *session
	RequestID string
}
type Adapter struct {
	cfg     Config
	guard   wire.Guard
	mu      sync.Mutex
	current *session
	replies *wire.Cache[replyContext]
}

func New(c Config) (*Adapter, error) {
	if c.AccountID == "" || c.BotID == "" || c.Secret == "" {
		return nil, errors.New("wecom: account, bot id and secret required")
	}
	if c.WebSocketURL == "" {
		c.WebSocketURL = "wss://openws.work.weixin.qq.com"
	}
	if err := wire.Endpoint(c.WebSocketURL); err != nil {
		return nil, err
	}
	return &Adapter{cfg: c, replies: wire.NewCache[replyContext](4096)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "wecom", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{Text: true, Markdown: true, Reply: true, ReceiveMedia: true, Card: true, Stream: true, Proactive: true}
}
func (a *Adapter) Run(ctx context.Context, sink gateway.Sink) error {
	if sink == nil {
		return errors.New("wecom: nil sink")
	}
	if err := a.guard.Enter(); err != nil {
		return err
	}
	defer a.guard.Leave()
	return wire.Retry(ctx, a.cfg.OnError, func(ctx context.Context) error { return a.connect(ctx, sink) })
}
func (a *Adapter) active() *session { a.mu.Lock(); defer a.mu.Unlock(); return a.current }
func (a *Adapter) connect(ctx context.Context, sink gateway.Sink) error {
	socket, err := wire.Dial(ctx, a.cfg.HTTPClient, a.cfg.WebSocketURL)
	if err != nil {
		return err
	}
	s := newSession(ctx, socket)
	defer s.close()
	authID := requestID("aibot_subscribe")
	body, _ := json.Marshal(map[string]string{"bot_id": a.cfg.BotID, "secret": a.cfg.Secret})
	auth, _ := json.Marshal(frame{Cmd: "aibot_subscribe", Headers: map[string]string{"req_id": authID}, Body: body})
	authctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	if err = socket.Write(authctx, 1, auth); err != nil {
		return errors.New("wecom: authentication transport failed")
	}
	_, raw, err := socket.Read(authctx)
	if err != nil {
		return errors.New("wecom: authentication transport failed")
	}
	var f frame
	if json.Unmarshal(raw, &f) != nil || f.Headers["req_id"] != authID || f.Code == nil {
		return &gateway.APIError{Platform: "wecom", Code: "invalid_auth_reply"}
	}
	if *f.Code != 0 {
		return &gateway.APIError{Platform: "wecom", Code: "authentication_rejected"}
	}
	cancel()
	a.mu.Lock()
	a.current = s
	a.mu.Unlock()
	jobs := make(chan gateway.Event, 64)
	var wg sync.WaitGroup
	defer func() {
		s.close()
		wg.Wait()
		a.mu.Lock()
		if a.current == s {
			a.current = nil
		}
		a.mu.Unlock()
	}()
	wg.Add(2)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.request(s.ctx, "ping", requestID("ping"), nil); err != nil {
					s.close()
					return
				}
			}
		}
	}()
	// The reader must never wait on Agent work: outbound sends need it to receive ACKs.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-s.ctx.Done():
				return
			case e := <-jobs:
				admitctx, done := context.WithTimeout(s.ctx, 2*time.Second)
				err := sink(admitctx, e)
				done()
				if !gateway.Consumed(err) {
					if a.cfg.OnError != nil {
						a.cfg.OnError(errors.New("wecom: event admission failed"))
					}
					s.close()
					return
				}
			}
		}
	}()
	for {
		readctx, done := context.WithTimeout(s.ctx, 90*time.Second)
		_, raw, err := socket.Read(readctx)
		done()
		if err != nil {
			return errors.New("wecom: connection read failed")
		}
		f = frame{} // ACKs omit cmd/body; never retain fields from a preceding callback.
		if json.Unmarshal(raw, &f) != nil {
			return errors.New("wecom: invalid frame")
		}
		if f.Cmd == "aibot_event_callback" {
			var v struct {
				Event struct {
					Type string `json:"eventtype"`
				} `json:"event"`
			}
			_ = json.Unmarshal(f.Body, &v)
			if v.Event.Type == "disconnected_event" {
				return &gateway.APIError{Platform: "wecom", Code: "connection_taken_over"}
			}
			continue
		}
		if f.Cmd != "aibot_msg_callback" {
			s.deliver(f)
			continue
		}
		e, err := a.decode(f, s)
		if errors.Is(err, gateway.ErrUnsupported) || errors.Is(err, gateway.ErrUnauthorized) {
			continue
		}
		if err != nil {
			return err
		}
		select {
		case jobs <- e:
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
			return gateway.ErrBusy
		}
	}
}

type content struct {
	Content string `json:"content"`
	URL     string `json:"url"`
	AESKey  string `json:"aeskey"`
}
type incoming struct {
	ID       string `json:"msgid"`
	BotID    string `json:"aibotid"`
	ChatID   string `json:"chatid"`
	ChatType string `json:"chattype"`
	Type     string `json:"msgtype"`
	Created  int64  `json:"create_time"`
	From     struct {
		ID string `json:"userid"`
	} `json:"from"`
	Text  content `json:"text"`
	Voice content `json:"voice"`
	Image content `json:"image"`
	File  content `json:"file"`
	Video content `json:"video"`
	Mixed struct {
		Items []struct {
			Type  string  `json:"msgtype"`
			Text  content `json:"text"`
			Image content `json:"image"`
		} `json:"msg_item"`
	} `json:"mixed"`
}

func (a *Adapter) decode(f frame, s *session) (gateway.Event, error) {
	var v incoming
	if json.Unmarshal(f.Body, &v) != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if v.BotID != "" && v.BotID != a.cfg.BotID {
		return gateway.Event{}, gateway.ErrUnauthorized
	}
	e := gateway.Event{ID: v.ID, MessageID: v.ID, SenderID: v.From.ID, Time: time.Now(), Route: gateway.Route{Account: a.Account(), TenantID: a.cfg.TenantID, ChatID: v.ChatID, Kind: "group"}, Native: json.RawMessage(append([]byte(nil), f.Body...))}
	switch v.ChatType {
	case "single":
		e.Route.Kind = "direct"
		e.Route.ChatID = v.From.ID
	case "group":
	default:
		return gateway.Event{}, gateway.ErrUnsupported
	}
	if v.Created > 0 {
		e.Time = time.Unix(v.Created, 0)
	}
	// Encrypted URLs and keys remain Native-only, not in default persistence/log JSON.
	add := func(kind string) {
		e.Attachments = append(e.Attachments, gateway.Attachment{Kind: kind, Ref: v.ID + ":" + kind})
	}
	switch v.Type {
	case "text":
		e.Text = v.Text.Content
	case "voice":
		e.Text = v.Voice.Content
	case "image", "file", "video":
		add(v.Type)
	case "mixed":
		for _, x := range v.Mixed.Items {
			switch x.Type {
			case "text":
				if e.Text != "" {
					e.Text += "\n"
				}
				e.Text += x.Text.Content
			case "image":
				add("image")
			}
		}
	default:
		return gateway.Event{}, gateway.ErrUnsupported
	}
	if e.ID == "" || e.SenderID == "" || e.Route.Validate() != nil || f.Headers["req_id"] == "" {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	a.replies.Put(wire.ContextKey(e.Route, e.MessageID), replyContext{s, f.Headers["req_id"]}, time.Now().Add(5*time.Minute))
	return e, nil
}
func (a *Adapter) reply(r gateway.Route, id string) (replyContext, error) {
	v, ok := a.replies.Get(wire.ContextKey(r, id))
	if !ok || v.session == nil || v.session != a.active() || v.session.ctx.Err() != nil {
		return replyContext{}, gateway.ErrReplyExpired
	}
	return v, nil
}
func (a *Adapter) UpdateStream(ctx context.Context, r gateway.Route, u gateway.StreamUpdate) (gateway.Receipt, error) {
	if r.Account != a.Account() || r.Validate() != nil || r.TenantID != a.cfg.TenantID || (r.Kind != "direct" && r.Kind != "group") || r.ThreadID != "" || u.ReplyTo == "" || u.StreamID == "" {
		return gateway.Receipt{}, gateway.ErrInvalidEvent
	}
	if len(u.Content) > 20480 || !utf8.ValidString(u.Content) {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	v, err := a.reply(r, u.ReplyTo)
	if err != nil {
		return gateway.Receipt{}, err
	}
	return v.session.request(ctx, "aibot_respond_msg", v.RequestID, map[string]any{"msgtype": "stream", "stream": map[string]any{"id": u.StreamID, "finish": u.Finished, "content": u.Content}})
}
func (a *Adapter) Send(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if err := wire.CheckSend(a.Account(), r, m); err != nil {
		return gateway.Receipt{}, err
	}
	if r.TenantID != a.cfg.TenantID || (r.Kind != "direct" && r.Kind != "group") {
		return gateway.Receipt{}, gateway.ErrInvalidEvent
	}
	if r.ThreadID != "" || len(m.Attachments) > 0 || len(m.Text) > 20480 {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	if len(m.Card) > 0 && (!json.Valid(m.Card) || m.Text != "") {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	if m.ReplyTo != "" {
		if len(m.Card) == 0 {
			streamID := m.ClientID
			if streamID == "" {
				streamID = requestID("stream")
			}
			return a.UpdateStream(ctx, r, gateway.StreamUpdate{ReplyTo: m.ReplyTo, StreamID: streamID, Content: m.Text, Finished: true})
		}
		v, err := a.reply(r, m.ReplyTo)
		if err != nil {
			return gateway.Receipt{}, err
		}
		return v.session.request(ctx, "aibot_respond_msg", v.RequestID, map[string]any{"msgtype": "template_card", "template_card": m.Card})
	}
	s := a.active()
	if s == nil {
		return gateway.Receipt{}, errors.New("wecom: not connected")
	}
	body := map[string]any{"chatid": r.ChatID, "msgtype": "markdown", "markdown": map[string]string{"content": m.Text}}
	if len(m.Card) > 0 {
		body = map[string]any{"chatid": r.ChatID, "msgtype": "template_card", "template_card": m.Card}
	}
	return s.request(ctx, "aibot_send_msg", requestID("aibot_send_msg"), body)
}
