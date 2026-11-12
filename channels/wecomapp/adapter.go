// Package wecomapp implements self-built Enterprise WeChat application callbacks.
// It is intentionally separate from the smart-robot WebSocket identity.
package wecomapp

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

type Config struct {
	AccountID, CorpID, CorpSecret, Token, EncodingAESKey, APIBase string
	AgentID                                                       int
	HTTPClient                                                    *http.Client
	OnError                                                       func(error)
}
type Adapter struct {
	cfg   Config
	key   []byte
	token wire.TokenCache
	guard wire.Guard
	media *wire.Cache[mediaResource]
}

func New(c Config) (*Adapter, error) {
	if c.AccountID == "" || c.CorpID == "" || c.CorpSecret == "" || c.Token == "" || c.AgentID <= 0 || len(c.EncodingAESKey) != 43 {
		return nil, errors.New("wecomapp: app credentials and 43-character encoding AES key required")
	}
	key, err := base64.StdEncoding.DecodeString(c.EncodingAESKey + "=")
	if err != nil || len(key) != 32 {
		return nil, errors.New("wecomapp: invalid AES key")
	}
	c.APIBase = wire.Base(c.APIBase, "https://qyapi.weixin.qq.com")
	if err = wire.Endpoint(c.APIBase); err != nil {
		return nil, err
	}
	return &Adapter{cfg: c, key: key, media: wire.NewCache[mediaResource](4096)}, nil
}
func (a *Adapter) Account() gateway.Account {
	return gateway.Account{Platform: "wecom_app", ID: a.cfg.AccountID}
}
func (a *Adapter) Capabilities() gateway.Capabilities {
	return gateway.Capabilities{Text: true, Markdown: true, Reply: true, ReceiveMedia: true, SendMedia: true, DownloadMedia: true, UploadMedia: true, Proactive: true, MaxTextBytes: 2048}
}
func (a *Adapter) Run(ctx context.Context, sink gateway.Sink) error {
	if sink == nil {
		return errors.New("wecomapp: nil sink")
	}
	if err := a.guard.Enter(); err != nil {
		return err
	}
	defer a.guard.Leave()
	return wire.Wait(ctx)
}
func signature(token, ts, nonce, encrypted string) []byte {
	v := []string{token, ts, nonce, encrypted}
	sort.Strings(v)
	sum := sha1.Sum([]byte(strings.Join(v, "")))
	return sum[:]
}
func (a *Adapter) decrypt(sig, ts, nonce, encrypted string) ([]byte, error) {
	if nonce == "" || len(nonce) > 1024 || len(encrypted) > wire.MaxBody || !wire.Fresh(ts, time.Now(), 5*time.Minute) || !wire.EqualHex(sig, signature(a.cfg.Token, ts, nonce, encrypted)) {
		return nil, gateway.ErrUnauthorized
	}
	data, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, gateway.ErrUnauthorized
	}
	plain, err := wire.DecryptCBC(a.key, a.key[:16], data, 32)
	if err != nil || len(plain) < 20 {
		return nil, gateway.ErrUnauthorized
	}
	n := uint64(binary.BigEndian.Uint32(plain[16:20]))
	if n > uint64(len(plain)-20) {
		return nil, gateway.ErrUnauthorized
	}
	end := 20 + int(n)
	if subtle.ConstantTimeCompare(plain[end:], []byte(a.cfg.CorpID)) != 1 {
		return nil, gateway.ErrUnauthorized
	}
	if !utf8.Valid(plain[20:end]) {
		return nil, gateway.ErrUnauthorized
	}
	return plain[20:end], nil
}
func safeXML(raw []byte, out any) error {
	if len(raw) > wire.MaxBody {
		return errors.New("wecomapp: XML too large")
	}
	d := xml.NewDecoder(bytes.NewReader(raw))
	depth := 0
	roots := 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("wecomapp: invalid XML")
		}
		switch token := token.(type) {
		case xml.Directive:
			return errors.New("wecomapp: XML directives forbidden")
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || token.Name.Local != "xml" || token.Name.Space != "" {
					return errors.New("wecomapp: expected one XML envelope")
				}
			}
			depth++
			if depth > 64 {
				return errors.New("wecomapp: XML nesting exceeded")
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(token)) != 0 {
				return errors.New("wecomapp: data outside XML envelope")
			}
		}
	}
	if roots != 1 {
		return errors.New("wecomapp: missing XML envelope")
	}
	return xml.Unmarshal(raw, out)
}

type message struct {
	To          string `xml:"ToUserName"`
	From        string `xml:"FromUserName"`
	Type        string `xml:"MsgType"`
	ID          string `xml:"MsgId"`
	AgentID     int    `xml:"AgentID"`
	Created     int64  `xml:"CreateTime"`
	Content     string `xml:"Content"`
	MediaID     string `xml:"MediaId"`
	Recognition string `xml:"Recognition"`
}

// Decode accepts only already-decrypted/authenticated XML; use Handler publicly.
func (a *Adapter) Decode(raw []byte) (gateway.Event, error) {
	var m message
	if err := safeXML(raw, &m); err != nil {
		return gateway.Event{}, err
	}
	if m.To != a.cfg.CorpID || m.AgentID != a.cfg.AgentID {
		return gateway.Event{}, gateway.ErrUnauthorized
	}
	e := gateway.Event{ID: m.ID, MessageID: m.ID, SenderID: m.From, Time: time.Unix(m.Created, 0), Text: m.Content, MessageType: m.Type, Route: gateway.Route{Account: a.Account(), TenantID: a.cfg.CorpID, ChatID: m.From, Kind: "direct"}}
	switch m.Type {
	case "text":
	case "image", "voice", "video", "file":
		if m.MediaID == "" {
			return gateway.Event{}, gateway.ErrInvalidEvent
		}
		e.Attachments = []gateway.Attachment{{Kind: m.Type, Ref: inboundRefPrefix + m.ID, MessageID: m.ID, Scope: &e.Route, ExpiresAt: time.Now().Add(30 * time.Minute)}}
		e.Text = m.Recognition
	default:
		return gateway.Event{}, &gateway.UnsupportedMessageError{Platform: "wecom_app", MessageType: m.Type}
	}
	if e.ID == "" || e.SenderID == "" || e.Route.Validate() != nil {
		return gateway.Event{}, gateway.ErrInvalidEvent
	}
	if e.Text != "" {
		e.Parts = append(e.Parts, gateway.ContentPart{Text: e.Text})
	}
	for _, p := range e.Attachments {
		a.media.Put(wire.ContextKey(e.Route, p.Ref), mediaResource{id: m.MediaID, kind: m.Type}, p.ExpiresAt)
		e.Parts = append(e.Parts, gateway.ContentPart{Attachment: &p})
	}
	return e, nil
}
func (a *Adapter) Handler(sink gateway.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sink == nil {
			http.Error(w, "not configured", 503)
			return
		}
		q := r.URL.Query()
		enc := ""
		if r.Method == "GET" {
			enc = q.Get("echostr")
		} else {
			raw, err := wire.Body(w, r)
			if err != nil {
				return
			}
			var v struct {
				Encrypted string `xml:"Encrypt"`
			}
			if safeXML(raw, &v) != nil {
				http.Error(w, "invalid callback", 400)
				return
			}
			enc = v.Encrypted
		}
		plain, err := a.decrypt(q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce"), enc)
		if err != nil {
			http.Error(w, "invalid callback authentication", 403)
			return
		}
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write(plain)
			return
		}
		e, err := a.Decode(plain)
		if errors.Is(err, gateway.ErrUnauthorized) {
			http.Error(w, "wrong application", 403)
			return
		}
		if err != nil && !errors.Is(err, gateway.ErrUnsupported) {
			http.Error(w, "invalid message", 400)
			return
		}
		if errors.Is(err, gateway.ErrUnsupported) {
			wire.ReportUnsupported(err, a.cfg.OnError)
		}
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			err = sink(ctx, e)
			cancel()
			if !gateway.Consumed(err) {
				http.Error(w, "admission unavailable", 503)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "success")
	})
}
func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	return a.token.Get(ctx, func(ctx context.Context) (string, time.Duration, error) {
		q := url.Values{"corpid": []string{a.cfg.CorpID}, "corpsecret": []string{a.cfg.CorpSecret}}
		var out struct {
			Code    *int   `json:"errcode"`
			Token   string `json:"access_token"`
			Expires int    `json:"expires_in"`
		}
		err := wire.JSON(ctx, a.cfg.HTTPClient, "wecom_app", "GET", a.cfg.APIBase+"/cgi-bin/gettoken?"+q.Encode(), nil, nil, &out)
		if err == nil {
			if out.Code == nil {
				err = errors.New("wecomapp: missing token status")
			} else {
				err = wire.Business("wecom_app", *out.Code)
			}
		}
		return out.Token, time.Duration(out.Expires) * time.Second, err
	})
}
func (a *Adapter) Send(ctx context.Context, r gateway.Route, m gateway.Message) (gateway.Receipt, error) {
	if err := wire.CheckSend(a.Account(), r, m); err != nil {
		return gateway.Receipt{}, err
	}
	if r.TenantID != a.cfg.CorpID || r.Kind != "direct" || r.ThreadID != "" || strings.TrimSpace(r.ChatID) != r.ChatID || strings.Contains(r.ChatID, "|") || strings.EqualFold(r.ChatID, "@all") {
		return gateway.Receipt{}, gateway.ErrInvalidEvent
	}
	if len(m.Card) > 0 || len(m.Text) > 2048 {
		return gateway.Receipt{}, gateway.ErrUnsupported
	}
	typ := "text"
	if m.Format == "markdown" {
		typ = "markdown"
	}
	body := map[string]any{"touser": r.ChatID, "agentid": a.cfg.AgentID, "msgtype": typ, typ: map[string]string{"content": m.Text}}
	if len(m.Attachments) > 0 {
		if len(m.Attachments) != 1 || m.Text != "" {
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		p := m.Attachments[0]
		if strings.HasPrefix(p.Ref, uploadRefPrefix) || strings.HasPrefix(p.Ref, inboundRefPrefix) {
			resource, ok := a.media.Get(wire.ContextKey(r, p.Ref))
			if !ok {
				return gateway.Receipt{}, gateway.ErrMediaExpired
			}
			if resource.kind != p.Kind {
				return gateway.Receipt{}, gateway.ErrInvalidEvent
			}
			p.Ref = resource.id
		}
		switch p.Kind {
		case "image", "file", "voice", "video":
		default:
			return gateway.Receipt{}, gateway.ErrUnsupported
		}
		if p.Ref == "" {
			return gateway.Receipt{}, gateway.ErrInvalidEvent
		}
		body = map[string]any{"touser": r.ChatID, "agentid": a.cfg.AgentID, "msgtype": p.Kind, p.Kind: map[string]string{"media_id": p.Ref}}
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.Receipt{}, err
	}
	var out struct {
		Code        *int   `json:"errcode"`
		ID          string `json:"msgid"`
		InvalidUser string `json:"invaliduser"`
		Unlicensed  string `json:"unlicenseduser"`
	}
	err = wire.JSON(ctx, a.cfg.HTTPClient, "wecom_app", "POST", a.cfg.APIBase+"/cgi-bin/message/send?access_token="+url.QueryEscape(token), nil, body, &out)
	if err != nil {
		return gateway.Receipt{}, a.apiError(token, err)
	}
	if out.Code == nil {
		return gateway.Receipt{}, &gateway.APIError{Platform: "wecom_app", Status: 200, Code: "missing_status", Uncertain: true}
	}
	if err = a.mediaError(token, *out.Code); err != nil {
		return gateway.Receipt{}, err
	}
	if out.InvalidUser != "" || out.Unlicensed != "" {
		return gateway.Receipt{}, &gateway.APIError{Platform: "wecom_app", Status: 200, Code: "recipient_rejected"}
	}
	return gateway.Receipt{MessageID: out.ID, Accepted: true}, nil
}
