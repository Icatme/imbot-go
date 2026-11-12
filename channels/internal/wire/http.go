// Package wire contains SDK-independent, bounded network primitives for channels.
package wire

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

const MaxBody = 2 << 20

// CancellationCause preserves cancellation owned by either the caller or the
// HTTP client (for example Client.Timeout). Return only standard sentinels:
// transport errors themselves may contain credentials or signed request URLs.
func CancellationCause(ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

// Client copies caller configuration; it never mutates a shared http.Client.
func Client(c *http.Client) *http.Client {
	var v http.Client
	if c != nil {
		v = *c
	}
	if v.Timeout == 0 {
		v.Timeout = 20 * time.Second
	}
	v.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &v
}
func Endpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid channel endpoint")
	}
	if u.Scheme == "https" || u.Scheme == "wss" {
		return nil
	}
	// Unencrypted endpoints are restricted to literal loopback for tests/local bridges.
	h := u.Hostname()
	if (u.Scheme == "http" || u.Scheme == "ws") && (h == "127.0.0.1" || h == "::1" || h == "localhost") {
		return nil
	}
	return errors.New("channel endpoint requires TLS")
}

// JSON never follows redirects or includes URLs/bodies in returned errors.
func JSON(ctx context.Context, c *http.Client, platform, method, endpoint string, headers http.Header, input, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := Endpoint(endpoint); err != nil {
		return err
	}
	var b []byte
	var err error
	if input != nil {
		b, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	if len(b) > MaxBody {
		return errors.New("request exceeds channel limit")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(b))
	if err != nil {
		return errors.New("invalid channel request")
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := Client(c).Do(req)
	if err != nil {
		if cause := CancellationCause(ctx, err); cause != nil {
			return errors.Join(cause, &gateway.APIError{Platform: platform, Code: "canceled", Retryable: false, Uncertain: method != "GET"})
		}
		return &gateway.APIError{Platform: platform, Code: "transport", Retryable: true, Uncertain: method != "GET"}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxBody+1))
	if err != nil || len(data) > MaxBody {
		api := &gateway.APIError{Platform: platform, Status: res.StatusCode, Code: "response_limit_or_read", Uncertain: method != "GET"}
		if cause := CancellationCause(ctx, err); cause != nil {
			return errors.Join(cause, api)
		}
		return api
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &gateway.APIError{Platform: platform, Status: res.StatusCode, Code: "http", Retryable: res.StatusCode == 429 || res.StatusCode >= 500, Uncertain: method != "GET" && res.StatusCode >= 500}
	}
	if output != nil {
		if err = json.Unmarshal(data, output); err != nil {
			return &gateway.APIError{Platform: platform, Status: res.StatusCode, Code: "invalid_json", Uncertain: method != "GET"}
		}
	}
	return nil
}
func Business(platform string, code int) error {
	if code == 0 {
		return nil
	}
	return &gateway.APIError{Platform: platform, Status: 200, Code: strconv.Itoa(code)}
}
func Bearer(token string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + token}}
}
func Body(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil, errors.New("POST required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid or oversized body", http.StatusRequestEntityTooLarge)
	}
	return b, err
}
func Respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func Fresh(s string, now time.Time, window time.Duration) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || s == "" {
		return false
	}
	d := now.Sub(time.Unix(n, 0))
	return d >= -window && d <= window
}
func EqualHex(s string, want []byte) bool {
	got, err := hex.DecodeString(s)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
func DecryptCBC(key, iv, data []byte, padBlock int) ([]byte, error) {
	if len(iv) != aes.BlockSize || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("invalid encrypted payload")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("invalid encryption key")
	}
	out := append([]byte(nil), data...)
	cipher.NewCBCDecrypter(b, iv).CryptBlocks(out, out)
	n := int(out[len(out)-1])
	if n < 1 || n > padBlock || n > len(out) {
		return nil, errors.New("invalid encrypted payload")
	}
	var bad byte
	for _, v := range out[len(out)-n:] {
		bad |= v ^ byte(n)
	}
	if bad != 0 {
		return nil, errors.New("invalid encrypted payload")
	}
	return out[:len(out)-n], nil
}

// CheckSend protects direct Adapter.Send calls as well as Gateway callers.
func CheckSend(a gateway.Account, r gateway.Route, m gateway.Message) error {
	if r.Account != a || r.Validate() != nil {
		return gateway.ErrInvalidEvent
	}
	for _, p := range m.Attachments {
		if err := p.ValidateFor(r); err != nil {
			return err
		}
	}
	if len(m.Text) > 60000 || len(m.Card) > MaxBody {
		return errors.New("message exceeds channel limit")
	}
	if m.Text == "" && len(m.Card) == 0 && len(m.Attachments) == 0 {
		return errors.New("empty message")
	}
	if m.Format != "" && m.Format != "text" && m.Format != "markdown" {
		return gateway.ErrUnsupported
	}
	return nil
}
func PathID(s string) string { return url.PathEscape(s) }
func Base(s, def string) string {
	if s == "" {
		s = def
	}
	return strings.TrimRight(s, "/")
}
func ContextKey(r gateway.Route, id string) string {
	b, _ := json.Marshal([]string{r.SessionKey(), id})
	return string(b)
}
