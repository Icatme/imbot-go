// Package feishu implements Feishu/Lark HTTP and WebSocket protocols in Go.
package feishu

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

type Config struct {
	AccountID, AppID, AppSecret, BaseURL, Transport string
	VerificationToken, EncryptKey, BotOpenID        string
	RespondWithoutMention                           bool
	HTTPClient                                      *http.Client
	OnError                                         func(error)
}
type Adapter struct {
	cfg         Config
	token       wire.TokenCache
	guard       wire.Guard
	replyRoutes *wire.Cache[bool]
}

func New(c Config) (*Adapter, error) {
	if c.AccountID == "" || c.AppID == "" || c.AppSecret == "" {
		return nil, errors.New("feishu: account, app id and secret required")
	}
	c.BaseURL = wire.Base(c.BaseURL, "https://open.feishu.cn")
	if err := wire.Endpoint(c.BaseURL); err != nil {
		return nil, err
	}
	if c.Transport == "" {
		c.Transport = "websocket"
	}
	if c.Transport != "websocket" && c.Transport != "webhook" {
		return nil, gateway.ErrUnsupported
	}
	if c.Transport == "webhook" && (c.VerificationToken == "" || c.EncryptKey == "") {
		return nil, errors.New("feishu: authenticated webhook requires token and encrypt key")
	}
	return &Adapter{cfg: c, replyRoutes: wire.NewCache[bool](4096)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "feishu", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{Text: true, Reply: true, ReceiveMedia: true, SendMedia: true, Card: true, Proactive: true}
}
func (a *Adapter) Run(ctx context.Context, sink gateway.Sink) error {
	if sink == nil {
		return errors.New("feishu: nil sink")
	}
	if err := a.guard.Enter(); err != nil {
		return err
	}
	defer a.guard.Leave()
	if a.cfg.Transport == "webhook" {
		return wire.Wait(ctx)
	}
	return wire.Retry(ctx, a.cfg.OnError, func(ctx context.Context) error { return a.connect(ctx, sink) })
}
func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	return a.token.Get(ctx, func(ctx context.Context) (string, time.Duration, error) {
		var r struct {
			Code   int    `json:"code"`
			Token  string `json:"tenant_access_token"`
			Expire int    `json:"expire"`
		}
		err := wire.JSON(ctx, a.cfg.HTTPClient, "feishu", "POST", a.cfg.BaseURL+"/open-apis/auth/v3/tenant_access_token/internal", nil, map[string]string{"app_id": a.cfg.AppID, "app_secret": a.cfg.AppSecret}, &r)
		if err == nil {
			err = wire.Business("feishu", r.Code)
		}
		return r.Token, time.Duration(r.Expire) * time.Second, err
	})
}
func (a *Adapter) Send(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if err := wire.CheckSend(a.Account(), r, m); err != nil {
		return gateway.Receipt{}, err
	}
	if m.Format == "markdown" {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	typ := "text"
	content, _ := json.Marshal(map[string]string{"text": m.Text})
	if len(m.Card) > 0 {
		if !json.Valid(m.Card) || m.Text != "" || len(m.Attachments) > 0 {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		typ = "interactive"
		content = m.Card
	}
	if len(m.Attachments) > 0 {
		if len(m.Attachments) != 1 || m.Text != "" {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		p := m.Attachments[0]
		if p.Ref == "" {
			return gateway.Receipt{}, gateway.ErrInvalidEvent
		}
		switch p.Kind {
		case "image":
			typ = "image"
			content, _ = json.Marshal(map[string]string{"image_key": p.Ref})
		case "file":
			typ = "file"
			content, _ = json.Marshal(map[string]string{"file_key": p.Ref})
		default:
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
	}
	body := map[string]any{"msg_type": typ, "content": string(content)}
	endpoint := a.cfg.BaseURL + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	if m.ReplyTo != "" {
		if _, ok := a.replyRoutes.Get(wire.ContextKey(r, m.ReplyTo)); !ok {
			return gateway.Receipt{}, gateway.ErrReplyExpired
		}
		endpoint = a.cfg.BaseURL + "/open-apis/im/v1/messages/" + wire.PathID(m.ReplyTo) + "/reply"
		body["reply_in_thread"] = r.ThreadID != ""
	} else {
		if r.ThreadID != "" {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		body["receive_id"] = r.ChatID
	}
	if m.ClientID != "" {
		id := sha256.Sum256([]byte(r.SessionKey() + m.ClientID))
		body["uuid"] = hex.EncodeToString(id[:16])
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.Receipt{}, err
	}
	var out struct {
		Code int `json:"code"`
		Data struct {
			ID string `json:"message_id"`
		} `json:"data"`
	}
	if err = wire.JSON(ctx, a.cfg.HTTPClient, "feishu", "POST", endpoint, wire.Bearer(token), body, &out); err != nil {
		return gateway.Receipt{}, err
	}
	if err = wire.Business("feishu", out.Code); err != nil {
		if out.Code == 99991663 || out.Code == 99991668 {
			a.token.Invalidate()
		}
		return gateway.Receipt{}, err
	}
	if out.Data.ID == "" {
		return gateway.Receipt{}, &gateway.APIError{Platform: "feishu", Status: 200, Code: "missing_message_id", Uncertain: true}
	}
	return gateway.Receipt{MessageID: out.Data.ID, Accepted: true}, nil
}

type envelope struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	Challenge string `json:"challenge"`
	Header    struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		Tenant    string `json:"tenant_key"`
		Token     string `json:"token"`
		AppID     string `json:"app_id"`
	} `json:"header"`
	Event struct {
		Sender struct {
			Type string `json:"sender_type"`
			ID   struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
		Message struct {
			ID       string `json:"message_id"`
			ChatID   string `json:"chat_id"`
			ChatType string `json:"chat_type"`
			Type     string `json:"message_type"`
			Content  string `json:"content"`
			Created  string `json:"create_time"`
			Thread   string `json:"thread_id"`
			Root     string `json:"root_id"`
			Parent   string `json:"parent_id"`
			Mentions []struct {
				ID struct {
					OpenID string `json:"open_id"`
				} `json:"id"`
			} `json:"mentions"`
		} `json:"message"`
	} `json:"event"`
}

// Decode is for already-authenticated payloads; use Handler for public HTTP.
func (a *Adapter) Decode(raw []byte) (gateway.Event, error) {
	var v envelope
	if err := json.Unmarshal(raw, &v); err != nil {
		return gateway.Event{}, err
	}
	if v.Header.EventType != "im.message.receive_v1" {
		return gateway.Event{}, gateway.ErrUnsupported
	}
	if v.Header.AppID != "" && v.Header.AppID != a.cfg.AppID {
		return gateway.Event{}, gateway.ErrUnauthorized
	}
	m := v.Event.Message
	e := gateway.Event{ID: v.Header.EventID, MessageID: m.ID, SenderID: v.Event.Sender.ID.OpenID, SenderIsBot: v.Event.Sender.Type == "app", Route: gateway.Route{Account: a.Account(), TenantID: v.Header.Tenant, ChatID: m.ChatID, ThreadID: m.Thread, Kind: "direct"}, ReplyTo: m.Parent, Time: time.Now()}
	if e.ID == "" {
		e.ID = m.ID
	}
	if e.Route.ThreadID == "" {
		e.Route.ThreadID = m.Root
	}
	if m.ChatType != "p2p" {
		e.Route.Kind = "group"
	}
	if n, err := strconv.ParseInt(m.Created, 10, 64); err == nil && n > 0 {
		e.Time = time.UnixMilli(n)
	}
	for _, x := range m.Mentions {
		if a.cfg.BotOpenID != "" && x.ID.OpenID == a.cfg.BotOpenID {
			e.Mentioned = true
		}
	}
	if e.Route.Kind == "group" && !a.cfg.RespondWithoutMention && !e.Mentioned {
		return gateway.Event{}, gateway.ErrUnauthorized
	}
	var c map[string]json.RawMessage
	if err := json.Unmarshal([]byte(m.Content), &c); err != nil {
		return gateway.Event{}, err
	}
	text := func(k string) string { var s string; _ = json.Unmarshal(c[k], &s); return s }
	switch m.Type {
	case "text":
		e.Text = text("text")
	case "image":
		e.Attachments = []gateway.Attachment{{Kind: "image", Ref: text("image_key")}}
	case "file", "audio", "media", "sticker":
		e.Attachments = []gateway.Attachment{{Kind: m.Type, Ref: text("file_key"), Name: text("file_name")}}
	default:
		return gateway.Event{}, gateway.ErrUnsupported
	}
	if e.MessageID == "" || e.SenderID == "" || e.Route.Validate() != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	a.replyRoutes.Put(wire.ContextKey(e.Route, e.MessageID), true, time.Now().Add(30*time.Minute))
	return e, nil
}
func (a *Adapter) admit(ctx context.Context, raw []byte, sink gateway.Sink) error {
	e, err := a.Decode(raw)
	if errors.Is(err, gateway.ErrUnsupported) || errors.Is(err, gateway.ErrUnauthorized) {
		return nil
	}
	if err != nil {
		return err
	}
	err = sink(ctx, e)
	if gateway.Consumed(err) {
		return nil
	}
	return err
}

// Handler requires an encrypt key and verification token even for URL challenges.
func (a *Adapter) Handler(sink gateway.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sink == nil || a.cfg.EncryptKey == "" || a.cfg.VerificationToken == "" {
			http.Error(w, "webhook not configured", 503)
			return
		}
		raw, err := wire.Body(w, r)
		if err != nil {
			return
		}
		ts, nonce := r.Header.Get("X-Lark-Request-Timestamp"), r.Header.Get("X-Lark-Request-Nonce")
		sum := sha256.Sum256(append([]byte(ts+nonce+a.cfg.EncryptKey), raw...))
		if nonce == "" || !wire.Fresh(ts, time.Now(), 5*time.Minute) || !wire.EqualHex(r.Header.Get("X-Lark-Signature"), sum[:]) {
			http.Error(w, "invalid signature", 403)
			return
		}
		var encrypted struct {
			Encrypt string `json:"encrypt"`
		}
		if json.Unmarshal(raw, &encrypted) != nil {
			http.Error(w, "invalid payload", 400)
			return
		}
		if encrypted.Encrypt != "" {
			b, err := base64.StdEncoding.DecodeString(encrypted.Encrypt)
			if err != nil || len(b) < 32 {
				http.Error(w, "invalid encryption", 403)
				return
			}
			key := sha256.Sum256([]byte(a.cfg.EncryptKey))
			raw, err = wire.DecryptCBC(key[:], b[:16], b[16:], 16)
			if err != nil {
				http.Error(w, "invalid encryption", 403)
				return
			}
		}
		var v envelope
		if json.Unmarshal(raw, &v) != nil {
			http.Error(w, "invalid payload", 400)
			return
		}
		token := v.Header.Token
		if v.Type == "url_verification" {
			token = v.Token
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.VerificationToken)) != 1 {
			http.Error(w, "invalid token", 403)
			return
		}
		if v.Type == "url_verification" {
			wire.Respond(w, 200, map[string]string{"challenge": v.Challenge})
			return
		}
		if v.Header.AppID != a.cfg.AppID {
			http.Error(w, "wrong app", 403)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err = a.admit(ctx, raw, sink); err != nil {
			http.Error(w, "admission unavailable", 503)
			return
		}
		wire.Respond(w, 200, map[string]int{"code": 0})
	})
}
func (a *Adapter) connect(ctx context.Context, sink gateway.Sink) error {
	var discovery struct {
		Code int `json:"code"`
		Data struct {
			URL    string `json:"URL"`
			Config struct {
				Ping int `json:"PingInterval"`
			} `json:"ClientConfig"`
		} `json:"data"`
	}
	if err := wire.JSON(ctx, a.cfg.HTTPClient, "feishu", "POST", a.cfg.BaseURL+"/callback/ws/endpoint", http.Header{"User-Agent": []string{"wechatbot-go/channel"}}, map[string]string{"AppID": a.cfg.AppID, "AppSecret": a.cfg.AppSecret}, &discovery); err != nil {
		return err
	}
	if discovery.Code != 0 {
		// Endpoint-discovery errors 1 and 1000040343 are transient in the
		// official WebSocket protocol; credential/configuration errors are not.
		return &gateway.APIError{Platform: "feishu", Status: 200, Code: strconv.Itoa(discovery.Code), Retryable: discovery.Code == 1 || discovery.Code == 1000040343}
	}
	u, err := url.Parse(discovery.Data.URL)
	if err != nil {
		return errors.New("invalid Feishu endpoint")
	}
	service, err := strconv.ParseUint(u.Query().Get("service_id"), 10, 32)
	if err != nil {
		return errors.New("missing Feishu service_id")
	}
	connctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := wire.Dial(connctx, a.cfg.HTTPClient, discovery.Data.URL)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer func() { cancel(); s.Close(); wg.Wait() }()
	var ping atomic.Int64
	setPing := func(n int) {
		if n > 0 && n <= 300 {
			ping.Store(int64(n))
		}
	}
	ping.Store(120)
	setPing(discovery.Data.Config.Ping)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			p := frame{Service: service, Headers: map[string]string{"type": "ping"}}
			if s.Write(connctx, 2, p.encode()) != nil {
				s.Close()
				return
			}
			timer := time.NewTimer(time.Duration(ping.Load()) * time.Second)
			select {
			case <-connctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	var parts assembler
	for {
		readctx, stop := context.WithTimeout(connctx, time.Duration(ping.Load()*3+15)*time.Second)
		op, b, err := s.Read(readctx)
		stop()
		if err != nil {
			return err
		}
		if op != 2 {
			return errors.New("Feishu expects binary frames")
		}
		f, err := decodeFrame(b)
		if err != nil {
			return err
		}
		if f.Method == 0 {
			if f.Headers["type"] == "pong" && len(f.Payload) > 0 {
				var c struct {
					Ping int `json:"PingInterval"`
				}
				if json.Unmarshal(f.Payload, &c) == nil {
					setPing(c.Ping)
				}
			}
			continue
		}
		if f.Method != 1 {
			continue
		}
		payload, ready, err := parts.combine(f)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		code := 200
		if f.Headers["type"] == "event" {
			admitctx, done := context.WithTimeout(connctx, 2*time.Second)
			err = a.admit(admitctx, payload, sink)
			done()
			if err != nil {
				code = 500
			}
		}
		f.Payload, _ = json.Marshal(map[string]int{"code": code})
		if err = s.Write(connctx, 2, f.encode()); err != nil {
			return err
		}
	}
}
