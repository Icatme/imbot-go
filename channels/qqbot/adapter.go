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
	"time"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

type Config struct {
	AccountID, AppID, ClientSecret, APIBase, TokenURL string
	EnableMarkdown, EnableProactive, EnableKeyboard   bool
	HTTPClient                                        *http.Client
	OnError                                           func(error)
}
type Adapter struct {
	cfg          Config
	private      ed25519.PrivateKey
	public       ed25519.PublicKey
	token        wire.TokenCache
	guard        wire.Guard
	media        *wire.Cache[incomingAttachment]
	uploads      *wire.Cache[uploadedResource]
	interactions *wire.Cache[bool]
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
	return &Adapter{cfg: c, private: private, public: private.Public().(ed25519.PublicKey), media: wire.NewCache[incomingAttachment](4096), uploads: wire.NewCache[uploadedResource](4096), interactions: wire.NewCache[bool](1024)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "qqbot", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{Text: true, MaxTextBytes: 4000, Reply: true, ReceiveMedia: true, SendMedia: true, DownloadMedia: true, UploadMedia: true, Card: a.cfg.EnableKeyboard, Markdown: a.cfg.EnableMarkdown, Proactive: a.cfg.EnableProactive}
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
			var unsupported *gateway.UnsupportedMessageError
			reportedUnsupported := errors.As(err, &unsupported)
			if reportedUnsupported {
				wire.ReportUnsupported(err, a.cfg.OnError)
			} else if err != nil && a.cfg.OnError != nil {
				a.cfg.OnError(err)
			}
			if !gateway.Consumed(err) && !reportedUnsupported {
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

// A failed operation is returned unchanged. Only a later explicit call obtains
// fresh credentials; a 401 never causes this adapter to replay a request.
func (a *Adapter) invalidateTokenOnUnauthorized(token string, err error) error {
	var api *gateway.APIError
	if errors.As(err, &api) && api.Status == http.StatusUnauthorized {
		a.token.Invalidate(token)
	}
	return err
}

func (a *Adapter) Send(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if err := wire.CheckSend(a.Account(), r, m); err != nil {
		return gateway.Receipt{}, err
	}
	if r.TenantID != a.cfg.AppID || r.ThreadID != "" {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	if a.isInteractionReply(r, m.ReplyTo) {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	if len(m.Text) > 4000 {
		return gateway.Receipt{}, gateway.ErrInvalidEvent
	}
	if m.ReplyTo == "" && !a.cfg.EnableProactive {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	body := map[string]any{"content": m.Text, "msg_type": 0}
	if len(m.Card) > 0 {
		if !a.cfg.EnableKeyboard || len(m.Attachments) > 0 || m.Text == "" {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		if err := validateKeyboard(m.Card); err != nil {
			return gateway.Receipt{}, err
		}
		body["keyboard"] = json.RawMessage(m.Card)
	}
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
		if len(m.Attachments) != 1 || m.Format == "markdown" || m.Attachments[0].Ref == "" || m.Attachments[0].MessageID != "" {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		if err := m.Attachments[0].ValidateFor(r); err != nil {
			return gateway.Receipt{}, err
		}
		fileInfo, err := a.outboundFileInfo(r, m.Attachments[0])
		if err != nil {
			return gateway.Receipt{}, err
		}
		body["msg_type"] = 7
		body["media"] = map[string]string{"file_info": fileInfo}
		// QQ's C2C/group media contract requires content even without a caption.
		if m.Text == "" {
			body["content"] = " "
		}
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
	var out struct {
		ID string `json:"id"`
	}
	if err := a.operationJSON(ctx, http.MethodPost, path+wire.PathID(r.ChatID)+"/messages", body, &out); err != nil {
		return gateway.Receipt{}, err
	}
	if out.ID == "" {
		return gateway.Receipt{}, &gateway.APIError{Platform: "qqbot", Status: 200, Code: "missing_message_id", Uncertain: true}
	}
	return gateway.Receipt{MessageID: out.ID, Accepted: true}, nil
}
