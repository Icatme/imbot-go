// Package qqbot implements authenticated QQ Bot API v2 webhooks and REST sends.
// It does not depend on legacy WebSocket availability or a platform SDK.
package qqbot

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

type Config struct {
	AccountID, AppID, ClientSecret, APIBase, TokenURL string
	EnableMarkdown, EnableProactive                   bool
	HTTPClient                                        *http.Client
}
type Adapter struct {
	cfg     Config
	private ed25519.PrivateKey
	public  ed25519.PublicKey
	token   wire.TokenCache
	guard   wire.Guard
}

func key(secret string) (ed25519.PrivateKey, error) {
	if secret == "" {
		return nil, errors.New("qqbot: empty secret")
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = secret[i%len(secret)]
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
func New(c Config) (*Adapter, error) {
	if c.AccountID == "" || c.AppID == "" {
		return nil, errors.New("qqbot: account and app id required")
	}
	private, err := key(c.ClientSecret)
	if err != nil {
		return nil, err
	}
	c.APIBase = wire.Base(c.APIBase, "https://api.bot.qq.com")
	if c.TokenURL == "" {
		c.TokenURL = c.APIBase + "/app/getAppAccessToken"
	}
	for _, u := range []string{c.APIBase, c.TokenURL} {
		if err = wire.Endpoint(u); err != nil {
			return nil, err
		}
	}
	return &Adapter{cfg: c, private: private, public: private.Public().(ed25519.PublicKey)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "qqbot", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{Text: true, Reply: true, ReceiveMedia: true, SendMedia: true, Markdown: a.cfg.EnableMarkdown, Proactive: a.cfg.EnableProactive}
}
func (a *Adapter) Run(ctx context.Context, sink gateway.Sink) error {
	if sink == nil {
		return errors.New("qqbot: nil sink")
	}
	if err := a.guard.Enter(); err != nil {
		return err
	}
	defer a.guard.Leave()
	return wire.Wait(ctx)
}
func (a *Adapter) verify(ts, sig string, body []byte) bool {
	decoded, err := hex.DecodeString(sig)
	return err == nil && len(decoded) == ed25519.SignatureSize && ed25519.Verify(a.public, append([]byte(ts), body...), decoded)
}

type envelope struct {
	Op   int             `json:"op"`
	ID   string          `json:"id"`
	Type string          `json:"t"`
	Data json.RawMessage `json:"d"`
}
type incoming struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Group     string `json:"group_openid"`
	Author    struct {
		User   string `json:"user_openid"`
		Member string `json:"member_openid"`
		Bot    bool   `json:"bot"`
	} `json:"author"`
	Attachments []struct {
		ID   string `json:"id"`
		Type string `json:"content_type"`
		Name string `json:"filename"`
		Size int64  `json:"size"`
		URL  string `json:"url"`
	} `json:"attachments"`
}

func (a *Adapter) decode(v envelope) (gateway.Event, error) {
	if v.Type != "C2C_MESSAGE_CREATE" && v.Type != "GROUP_AT_MESSAGE_CREATE" {
		return gateway.Event{}, gateway.ErrUnsupported
	}
	var m incoming
	if json.Unmarshal(v.Data, &m) != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	sender := m.Author.User
	kind := "direct"
	chat := sender
	if v.Type == "GROUP_AT_MESSAGE_CREATE" {
		sender = m.Author.Member
		chat = m.Group
		kind = "group"
	}
	e := gateway.Event{ID: v.ID, MessageID: m.ID, SenderID: sender, SenderIsBot: m.Author.Bot, Text: strings.TrimSpace(m.Content), Mentioned: kind == "group", Route: gateway.Route{Account: a.Account(), TenantID: a.cfg.AppID, ChatID: chat, Kind: kind}, Time: time.Now(), Native: json.RawMessage(append([]byte(nil), v.Data...))}
	if e.ID == "" {
		e.ID = m.ID
	}
	if ts, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil {
		e.Time = ts
	}
	for i, p := range m.Attachments {
		typ := "file"
		for _, prefix := range []string{"image/", "audio/", "video/"} {
			if strings.HasPrefix(p.Type, prefix) {
				typ = strings.TrimSuffix(prefix, "/")
				break
			}
		}
		ref := p.ID
		if ref == "" {
			ref = m.ID + ":" + strconv.Itoa(i)
		}
		e.Attachments = append(e.Attachments, gateway.Attachment{Kind: typ, Ref: ref, Name: p.Name, Size: p.Size})
	}
	if e.ID == "" || e.MessageID == "" || sender == "" || e.Route.Validate() != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	return e, nil
}
func (a *Adapter) Handler(sink gateway.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sink == nil {
			http.Error(w, "not configured", 503)
			return
		}
		raw, err := wire.Body(w, r)
		if err != nil {
			return
		}
		ts := r.Header.Get("X-Signature-Timestamp")
		if !wire.Fresh(ts, time.Now(), 5*time.Minute) || !a.verify(ts, r.Header.Get("X-Signature-Ed25519"), raw) {
			http.Error(w, "invalid signature", 403)
			return
		}
		if app := r.Header.Get("X-Bot-Appid"); app != "" && app != a.cfg.AppID {
			http.Error(w, "wrong app", 403)
			return
		}
		var v envelope
		if json.Unmarshal(raw, &v) != nil {
			http.Error(w, "invalid payload", 400)
			return
		}
		switch v.Op {
		case 13:
			var c struct {
				Token string `json:"plain_token"`
				Time  string `json:"event_ts"`
			}
			if json.Unmarshal(v.Data, &c) != nil || c.Token == "" || len(c.Token) > 4096 || !wire.Fresh(c.Time, time.Now(), 5*time.Minute) {
				http.Error(w, "invalid challenge", 400)
				return
			}
			sig := ed25519.Sign(a.private, []byte(c.Time+c.Token))
			wire.Respond(w, 200, map[string]string{"plain_token": c.Token, "signature": hex.EncodeToString(sig)})
		case 1:
			var seq uint64
			if json.Unmarshal(v.Data, &seq) != nil {
				http.Error(w, "invalid heartbeat", 400)
				return
			}
			wire.Respond(w, 200, map[string]any{"op": 11, "d": seq})
		case 0:
			e, err := a.decode(v)
			result := 0
			if err == nil {
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				err = sink(ctx, e)
				cancel()
			}
			if !gateway.Consumed(err) && !errors.Is(err, gateway.ErrUnsupported) {
				result = 1
			}
			wire.Respond(w, 200, map[string]int{"op": 12, "d": result}) // d=1 requests redelivery
		default:
			http.Error(w, "unsupported opcode", 400)
		}
	})
}
func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	return a.token.Get(ctx, func(ctx context.Context) (string, time.Duration, error) {
		var out struct {
			Token   string          `json:"access_token"`
			Expires json.RawMessage `json:"expires_in"`
			Code    int             `json:"code"`
		}
		err := wire.JSON(ctx, a.cfg.HTTPClient, "qqbot", "POST", a.cfg.TokenURL, nil, map[string]string{"appId": a.cfg.AppID, "clientSecret": a.cfg.ClientSecret}, &out)
		if err == nil {
			err = wire.Business("qqbot", out.Code)
		}
		var ttl int64
		if len(out.Expires) > 0 && out.Expires[0] == '"' {
			var text string
			_ = json.Unmarshal(out.Expires, &text)
			ttl, _ = strconv.ParseInt(text, 10, 64)
		} else {
			_ = json.Unmarshal(out.Expires, &ttl)
		}
		if ttl <= 0 || ttl > 86400 {
			if err == nil {
				err = errors.New("qqbot: invalid token lifetime")
			}
		}
		return out.Token, time.Duration(ttl) * time.Second, err
	})
}
func (a *Adapter) Send(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if err := wire.CheckSend(a.Account(), r, m); err != nil {
		return gateway.Receipt{}, err
	}
	if r.TenantID != a.cfg.AppID || r.ThreadID != "" || len(m.Card) > 0 {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	if m.ReplyTo == "" && !a.cfg.EnableProactive {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	body := map[string]any{"content": m.Text, "msg_type": 0}
	seq := m.Sequence
	if seq == 0 {
		seq = 1
	}
	if seq < 1 || seq > 2147483647 {
		return gateway.Receipt{}, gateway.ErrInvalidEvent
	}
	if m.ReplyTo != "" {
		body["msg_id"] = m.ReplyTo
		body["msg_seq"] = seq
	}
	if m.Format == "markdown" {
		if !a.cfg.EnableMarkdown {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		body["msg_type"] = 2
		body["markdown"] = map[string]string{"content": m.Text}
		delete(body, "content")
	}
	if len(m.Attachments) > 0 {
		if len(m.Attachments) != 1 || m.Format == "markdown" || m.Attachments[0].Ref == "" {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		body["msg_type"] = 7
		body["media"] = map[string]string{"file_info": m.Attachments[0].Ref}
	}
	path := ""
	switch r.Kind {
	case "direct":
		path = "/v2/users/"
	case "group":
		path = "/v2/groups/"
	default:
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.Receipt{}, err
	}
	var out struct {
		ID   string `json:"id"`
		Code int    `json:"code"`
	}
	headers := http.Header{"Authorization": []string{"QQBot " + token}, "X-Union-Appid": []string{a.cfg.AppID}}
	err = wire.JSON(ctx, a.cfg.HTTPClient, "qqbot", "POST", a.cfg.APIBase+path+wire.PathID(r.ChatID)+"/messages", headers, body, &out)
	if err != nil {
		var api *gateway.APIError
		if errors.As(err, &api) && api.Status == 401 {
			a.token.Invalidate()
		}
		return gateway.Receipt{}, err
	}
	if err = wire.Business("qqbot", out.Code); err != nil {
		return gateway.Receipt{}, err
	}
	if out.ID == "" {
		return gateway.Receipt{}, &gateway.APIError{Platform: "qqbot", Status: 200, Code: "missing_message_id", Uncertain: true}
	}
	return gateway.Receipt{MessageID: out.ID, Accepted: true}, nil
}
