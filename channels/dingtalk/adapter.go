// Package dingtalk implements DingTalk Stream and robot HTTP APIs without a platform SDK.
package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

const topic = "/v1.0/im/bot/messages/get"

type Config struct {
	AccountID, ClientID, ClientSecret, RobotCode, APIBase string
	SessionWebhookHost                                    string // exact host, defaults to oapi.dingtalk.com
	RespondWithoutMention                                 bool
	HTTPClient                                            *http.Client
	OnError                                               func(error)
}
type replyContext struct {
	URL     string
	Expires time.Time
}
type Adapter struct {
	cfg       Config
	guard     wire.Guard
	token     wire.TokenCache
	replies   *wire.Cache[replyContext]
	resources *wire.Cache[string]
}

func New(c Config) (*Adapter, error) {
	if c.AccountID == "" || c.ClientID == "" || c.ClientSecret == "" {
		return nil, errors.New("dingtalk: account and app credentials required")
	}
	c.APIBase = wire.Base(c.APIBase, "https://api.dingtalk.com")
	if err := wire.Endpoint(c.APIBase); err != nil {
		return nil, err
	}
	if c.SessionWebhookHost == "" {
		c.SessionWebhookHost = "oapi.dingtalk.com"
	}
	if strings.ContainsAny(c.SessionWebhookHost, "/@?#") {
		return nil, errors.New("dingtalk: webhook host must be an exact authority")
	}
	return &Adapter{cfg: c, replies: wire.NewCache[replyContext](4096), resources: wire.NewCache[string](4096)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "dingtalk", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{MaxTextBytes: 20000, Text: true, Markdown: true, Reply: true, ReceiveMedia: true, DownloadMedia: true, Proactive: a.cfg.RobotCode != ""}
}
func (a *Adapter) Run(ctx context.Context, sink gateway.Sink) error {
	if sink == nil {
		return errors.New("dingtalk: nil sink")
	}
	if err := a.guard.Enter(); err != nil {
		return err
	}
	defer a.guard.Leave()
	return wire.Retry(ctx, a.cfg.OnError, func(ctx context.Context) error { return a.connect(ctx, sink) })
}

type streamFrame struct {
	Type    string            `json:"type"`
	Headers map[string]string `json:"headers"`
	Data    string            `json:"data"`
}
type ack struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string            `json:"message"`
	Data    string            `json:"data"`
}

func response(f streamFrame, code int) ack {
	data := `{"response":"OK"}`
	if f.Type == "SYSTEM" {
		data = f.Data
	}
	if code != 200 {
		data = `{"response":"admission unavailable"}`
	}
	return ack{Code: code, Headers: map[string]string{"messageId": f.Headers["messageId"], "contentType": "application/json"}, Message: "OK", Data: data}
}

type incoming struct {
	ID        string `json:"msgId"`
	Type      string `json:"msgtype"`
	Sender    string `json:"senderId"`
	Staff     string `json:"senderStaffId"`
	Corp      string `json:"senderCorpId"`
	Chat      string `json:"conversationId"`
	Kind      string `json:"conversationType"`
	Mentioned bool   `json:"isInAtList"`
	Created   int64  `json:"createAt"`
	Webhook   string `json:"sessionWebhook"`
	Expires   int64  `json:"sessionWebhookExpiredTime"`
	Text      struct {
		Content string `json:"content"`
	} `json:"text"`
	Content json.RawMessage `json:"content"`
}

type incomingContent struct {
	DownloadCode string        `json:"downloadCode"`
	PictureCode  string        `json:"pictureDownloadCode"`
	FileName     string        `json:"fileName"`
	Recognition  string        `json:"recognition"`
	RichText     []richElement `json:"richText"`
}

func (a *Adapter) validWebhook(raw string) bool {
	if wire.Endpoint(raw) != nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.EqualFold(u.Host, a.cfg.SessionWebhookHost) || u.Path != "/robot/sendBySession" {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	return err == nil && len(query["session"]) == 1 && query.Get("session") != ""
}

// Decode is only for payloads received on the authenticated Stream connection.
func (a *Adapter) Decode(data []byte, eventID string) (gateway.Event, error) {
	var v incoming
	if err := json.Unmarshal(data, &v); err != nil {
		return gateway.Event{}, err
	}
	sender := v.Staff
	if sender == "" {
		sender = v.Sender
	}
	e := gateway.Event{MessageType: v.Type, ID: eventID, MessageID: v.ID, SenderID: sender, Text: v.Text.Content, Mentioned: v.Mentioned, Time: time.UnixMilli(v.Created), Route: gateway.Route{Account: a.Account(), TenantID: v.Corp, ChatID: v.Chat, Kind: "group"}}
	if e.ID == "" {
		e.ID = v.ID
	}
	switch v.Kind {
	case "1":
		e.Route.Kind = "direct"
		e.Route.ChatID = sender
	case "2":
	default:
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "dingtalk", MessageType: "conversation/" + v.Kind}
	}
	if e.Route.Kind == "group" && !a.cfg.RespondWithoutMention && !v.Mentioned {
		return gateway.Event{}, gateway.ErrUnauthorized
	}
	if e.MessageID == "" || sender == "" || e.Route.Validate() != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	var content incomingContent
	switch v.Type {
	case "richText", "picture", "image", "file", "audio", "video":
		if err := json.Unmarshal(v.Content, &content); err != nil {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
	}
	switch v.Type {
	case "text":
	case "richText":
		var err error
		e.Text, e.Attachments, e.Parts, err = decodeRichText(content.RichText)
		if err != nil {
			return gateway.Event{}, err
		}
	case "picture", "image", "file", "audio", "video":
		ref := content.DownloadCode
		if ref == "" {
			ref = content.PictureCode
		}
		e.Text = content.Recognition
		if ref == "" {
			if v.Type == "audio" && e.Text != "" {
				break
			}
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
		kind := v.Type
		if kind == "picture" {
			kind = "image"
		}
		e.Attachments = []gateway.Attachment{{Kind: kind, Ref: ref, Name: content.FileName}}
	default:
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "dingtalk", MessageType: v.Type}
	}
	if v.Webhook != "" {
		if !a.validWebhook(v.Webhook) {
			return gateway.Event{}, errors.New("dingtalk: invalid session webhook destination")
		}
		expires := time.UnixMilli(v.Expires)
		if expires.After(time.Now().Add(time.Hour)) {
			expires = time.Now().Add(time.Hour)
		}
		a.replies.Put(wire.ContextKey(e.Route, e.MessageID), replyContext{v.Webhook, expires}, expires)
	}
	a.bindResources(&e)
	return e, nil
}
func (a *Adapter) handle(ctx context.Context, f streamFrame, sink gateway.Sink) (ack, bool) {
	code := 200
	if f.Type == "CALLBACK" && f.Headers["topic"] == topic {
		e, err := a.Decode([]byte(f.Data), f.Headers["messageId"])
		var unsupported *gateway.UnsupportedMessageError
		if errors.As(err, &unsupported) {
			wire.ReportUnsupported(unsupported, a.cfg.OnError)
		}
		if err == nil {
			err = sink(ctx, e)
		}
		if err != nil && !gateway.Consumed(err) && !errors.Is(err, gateway.ErrUnsupported) {
			code = 500
		}
	}
	return response(f, code), f.Type == "SYSTEM" && f.Headers["topic"] == "disconnect"
}
func (a *Adapter) connect(ctx context.Context, sink gateway.Sink) error {
	var out struct {
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	body := map[string]any{"clientId": a.cfg.ClientID, "clientSecret": a.cfg.ClientSecret, "subscriptions": []map[string]string{{"type": "CALLBACK", "topic": topic}}, "ua": "wechatbot-go/stream", "localIp": ""}
	if err := wire.JSON(ctx, a.cfg.HTTPClient, "dingtalk", "POST", a.cfg.APIBase+"/v1.0/gateway/connections/open", nil, body, &out); err != nil {
		return err
	}
	if out.Endpoint == "" || out.Ticket == "" {
		return &gateway.APIError{Platform: "dingtalk", Code: "invalid_discovery"}
	}
	u, err := url.Parse(out.Endpoint)
	if err != nil {
		return errors.New("dingtalk: invalid stream endpoint")
	}
	q := u.Query()
	q.Set("ticket", out.Ticket)
	u.RawQuery = q.Encode()
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := wire.Dial(life, a.cfg.HTTPClient, u.String())
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer func() { cancel(); s.Close(); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-life.Done():
				return
			case <-t.C:
				if s.Write(life, 9, nil) != nil {
					s.Close()
					return
				}
			}
		}
	}()
	for {
		readctx, stop := context.WithTimeout(life, 3*time.Minute)
		op, b, err := s.Read(readctx)
		stop()
		if err != nil {
			return err
		}
		if op != 1 {
			return errors.New("dingtalk: expected JSON text frame")
		}
		var f streamFrame
		if json.Unmarshal(b, &f) != nil {
			return errors.New("dingtalk: invalid stream frame")
		}
		admitctx, done := context.WithTimeout(life, 2*time.Second)
		ack, disconnect := a.handle(admitctx, f, sink)
		done()
		raw, _ := json.Marshal(ack)
		if err = s.Write(life, 1, raw); err != nil {
			return err
		}
		if disconnect {
			return errors.New("dingtalk: server requested reconnect")
		}
	}
}
func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	return a.token.Get(ctx, func(ctx context.Context) (string, time.Duration, error) {
		var out struct {
			Token  string `json:"accessToken"`
			Expire int    `json:"expireIn"`
		}
		err := wire.JSON(ctx, a.cfg.HTTPClient, "dingtalk", "POST", a.cfg.APIBase+"/v1.0/oauth2/accessToken", nil, map[string]string{"appKey": a.cfg.ClientID, "appSecret": a.cfg.ClientSecret}, &out)
		return out.Token, time.Duration(out.Expire) * time.Second, err
	})
}
func (a *Adapter) Send(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if err := wire.CheckSend(a.Account(), r, m); err != nil {
		return gateway.Receipt{}, err
	}
	if r.ThreadID != "" || len(m.Card) > 0 || len(m.Attachments) > 0 || len(m.Text) > 20000 {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	if m.ReplyTo != "" {
		rc, ok := a.replies.Get(wire.ContextKey(r, m.ReplyTo))
		if !ok {
			return gateway.Receipt{}, gateway.ErrReplyExpired
		}
		body := map[string]any{"msgtype": "text", "text": map[string]string{"content": m.Text}}
		if m.Format == "markdown" {
			body = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": "Reply", "text": m.Text}}
		}
		var out struct {
			Code *int `json:"errcode"`
		}
		if err := wire.JSON(ctx, a.cfg.HTTPClient, "dingtalk", "POST", rc.URL, nil, body, &out); err != nil {
			return gateway.Receipt{}, err
		}
		if out.Code == nil {
			return gateway.Receipt{}, &gateway.APIError{Platform: "dingtalk", Status: 200, Code: "missing_errcode", Uncertain: true}
		}
		if err := wire.Business("dingtalk", *out.Code); err != nil {
			return gateway.Receipt{}, err
		}
		return gateway.Receipt{Accepted: true}, nil // session webhook has no message id
	}
	if a.cfg.RobotCode == "" {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	key := "sampleText"
	param := map[string]string{"content": m.Text}
	if m.Format == "markdown" {
		key = "sampleMarkdown"
		param = map[string]string{"title": "Message", "text": m.Text}
	}
	data, _ := json.Marshal(param)
	body := map[string]any{"robotCode": a.cfg.RobotCode, "msgKey": key, "msgParam": string(data)}
	path := "/v1.0/robot/groupMessages/send"
	switch r.Kind {
	case "group":
		body["openConversationId"] = r.ChatID
	case "direct":
		path = "/v1.0/robot/oToMessages/batchSend"
		body["userIds"] = []string{r.ChatID}
	default:
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.Receipt{}, err
	}
	var out struct {
		ID string `json:"processQueryKey"`
	}
	err = wire.JSON(ctx, a.cfg.HTTPClient, "dingtalk", "POST", a.cfg.APIBase+path, http.Header{"x-acs-dingtalk-access-token": []string{token}}, body, &out)
	if err != nil {
		var api *gateway.APIError
		if errors.As(err, &api) && api.Status == 401 {
			a.token.Invalidate(token)
		}
		return gateway.Receipt{}, err
	}
	if out.ID == "" {
		return gateway.Receipt{}, &gateway.APIError{Platform: "dingtalk", Status: 200, Code: "missing_process_key", Uncertain: true}
	}
	return gateway.Receipt{MessageID: out.ID, Accepted: true}, nil
}
