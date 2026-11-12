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

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
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
	resources   *wire.Cache[messageResource]
	uploads     *wire.Cache[uploadedResource]
	cardRoutes  *wire.Cache[gateway.Route]
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
	return &Adapter{cfg: c, replyRoutes: wire.NewCache[bool](4096), resources: wire.NewCache[messageResource](4096), uploads: wire.NewCache[uploadedResource](4096), cardRoutes: wire.NewCache[gateway.Route](4096)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "feishu", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{MaxTextBytes: 8000, Text: true, Reply: true, ReceiveMedia: true, SendMedia: true, DownloadMedia: true, UploadMedia: true, Markdown: true, Card: true, Proactive: true}
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
	m, err := a.resolveUploads(r, m)
	if err != nil {
		return gateway.Receipt{}, err
	}
	typ, content, err := outboundContent(m)
	if err != nil {
		return gateway.Receipt{}, err
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
		Code *int `json:"code"`
		Data struct {
			ID string `json:"message_id"`
		} `json:"data"`
	}
	if err = a.apiJSON(ctx, token, endpoint, body, &out); err != nil {
		return gateway.Receipt{}, err
	}
	if out.Code == nil {
		return gateway.Receipt{}, &gateway.APIError{Platform: "feishu", Status: http.StatusOK, Code: "missing_status", Uncertain: true}
	}
	if err = wire.Business("feishu", *out.Code); err != nil {
		return gateway.Receipt{}, a.mediaAPIError(token, err)
	}
	if out.Data.ID == "" {
		return gateway.Receipt{}, &gateway.APIError{Platform: "feishu", Status: 200, Code: "missing_message_id", Uncertain: true}
	}
	if typ == "interactive" {
		a.rememberCardRoute(r, out.Data.ID)
	}
	return gateway.Receipt{MessageID: out.Data.ID, Accepted: true}, nil
}

type envelope struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	Challenge string `json:"challenge"`
	Header    struct {
		EventID   string `json:"event_id"`
		Created   string `json:"create_time"`
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
	if v.Header.AppID != "" && v.Header.AppID != a.cfg.AppID {
		return gateway.Event{}, gateway.ErrUnauthorized
	}
	if v.Header.EventType == "card.action.trigger" {
		return a.decodeCardAction(raw, v)
	}
	if v.Header.EventType != "im.message.receive_v1" {
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "feishu", MessageType: v.Header.EventType}
	}
	m := v.Event.Message
	e := gateway.Event{MessageType: m.Type, ID: v.Header.EventID, MessageID: m.ID, SenderID: v.Event.Sender.ID.OpenID, SenderIsBot: v.Event.Sender.Type == "app", Route: gateway.Route{Account: a.Account(), TenantID: v.Header.Tenant, ChatID: m.ChatID, ThreadID: m.Thread, Kind: "direct"}, ReplyTo: m.Parent, Time: time.Now()}
	if e.ID == "" {
		e.ID = m.ID
	}
	if e.ReplyTo == "" {
		e.ReplyTo = m.Root
	}
	switch m.ChatType {
	case "p2p":
	case "group":
		e.Route.Kind = "group"
	default:
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "feishu", MessageType: "chat/" + m.ChatType}
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
	if e.MessageID == "" || e.SenderID == "" || e.Route.Validate() != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	switch m.Type {
	case "post", "text", "image", "file", "audio", "media", "sticker":
	default:
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "feishu", MessageType: m.Type}
	}
	var c struct {
		Text     string `json:"text"`
		ImageKey string `json:"image_key"`
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal([]byte(m.Content), &c); err != nil {
		return gateway.Event{}, err
	}
	switch m.Type {
	case "post":
		var err error
		e.Text, e.Attachments, e.Parts, err = decodePost([]byte(m.Content))
		if err != nil {
			return gateway.Event{}, err
		}
	case "text":
		e.Text = c.Text
	case "image":
		e.Attachments = []gateway.Attachment{{Kind: "image", Ref: c.ImageKey}}
	case "file", "audio", "media", "sticker":
		e.Attachments = []gateway.Attachment{{Kind: m.Type, Ref: c.FileKey, Name: c.FileName}}
	default:
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "feishu", MessageType: m.Type}
	}
	for _, p := range e.Attachments {
		if p.Ref == "" {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
	}
	if e.ReplyTo != "" {
		e.Quote = &gateway.Quote{MessageID: e.ReplyTo}
	}
	a.bindResources(&e)
	a.replyRoutes.Put(wire.ContextKey(e.Route, e.MessageID), true, time.Now().Add(30*time.Minute))
	return e, nil
}
func (a *Adapter) admit(ctx context.Context, raw []byte, sink gateway.Sink) ([]byte, error) {
	e, err := a.Decode(raw)
	var unsupported *gateway.UnsupportedMessageError
	if errors.As(err, &unsupported) {
		wire.ReportUnsupported(unsupported, a.cfg.OnError)
	}
	if errors.Is(err, gateway.ErrUnsupported) || errors.Is(err, gateway.ErrUnauthorized) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = sink(ctx, e)
	if !gateway.Consumed(err) {
		return nil, err
	}
	if e.Interaction != nil {
		// An empty action response keeps the current card unchanged.
		return []byte("{}"), nil
	}
	return nil, nil
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
		response, err := a.admit(ctx, raw, sink)
		if err != nil {
			http.Error(w, "admission unavailable", 503)
			return
		}
		if len(response) > 0 {
			wire.Respond(w, 200, json.RawMessage(response))
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
	if err := wire.JSON(ctx, a.cfg.HTTPClient, "feishu", "POST", a.cfg.BaseURL+"/callback/ws/endpoint", http.Header{"User-Agent": []string{"imbot-go/channel"}}, map[string]string{"AppID": a.cfg.AppID, "AppSecret": a.cfg.AppSecret}, &discovery); err != nil {
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
		response := struct {
			Code int    `json:"code"`
			Data []byte `json:"data,omitempty"`
		}{Code: 200}
		if f.Headers["type"] == "event" || f.Headers["type"] == "card" {
			admitctx, done := context.WithTimeout(connctx, 2*time.Second)
			response.Data, err = a.admit(admitctx, payload, sink)
			done()
			if err != nil {
				response.Code = 500
			}
		}
		f.Payload, _ = json.Marshal(response)
		if err = s.Write(connctx, 2, f.encode()); err != nil {
			return err
		}
	}
}
