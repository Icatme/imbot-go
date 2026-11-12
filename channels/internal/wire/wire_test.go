package wire

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

func TestHTTPRedirectAndRedaction(t *testing.T) {
	var hit atomic.Bool
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer dest.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 307) }))
	defer src.Close()
	err := JSON(context.Background(), nil, "x", "POST", src.URL+"?secret=credential", Bearer("secret"), map[string]string{"x": "y"}, nil)
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Status != 307 || hit.Load() || strings.Contains(err.Error(), "credential") { t.Fatal(err) }
}
func TestHTTPSizeLimit(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", MaxBody+1)) }))
	defer s.Close()
	var out any
	if JSON(context.Background(), nil, "x", "GET", s.URL, nil, nil, &out) == nil { t.Fatal("unbounded") }
}
func TestEndpoints(t *testing.T) {
	for _, s := range []string{"http://evil.test", "https://u:p@example.com", "file:///etc/passwd", "https://example.com/#secret"} { if Endpoint(s) == nil { t.Fatal(s) } }
	if Endpoint("http://127.0.0.1:1234") != nil { t.Fatal("loopback rejected") }
}
func TestTokenCacheConcurrent(t *testing.T) {
	var cache TokenCache
	var n atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := cache.Get(context.Background(), func(context.Context) (string, time.Duration, error) { n.Add(1); return "token", time.Hour, nil })
			if err != nil || v != "token" { t.Error(v, err) }
		}()
	}
	wg.Wait()
	if n.Load() != 1 { t.Fatal(n.Load()) }
}
func TestCacheExpiryAndBound(t *testing.T) {
	c := NewCache[string](1)
	c.Put("a", "secret", time.Now().Add(time.Minute))
	c.Put("b", "next", time.Now().Add(time.Hour))
	if _, ok := c.Get("a"); ok { t.Fatal("not bounded") }
	c.Put("c", "expired", time.Now().Add(-time.Second))
	if _, ok := c.Get("c"); ok { t.Fatal("expired") }
}
func TestFresh(t *testing.T) {
	now := time.Unix(1000, 0)
	if !Fresh("1000", now, time.Minute) || Fresh("1", now, time.Minute) || Fresh("broken", now, time.Minute) { t.Fatal("timestamp") }
}
func TestCBCRejectsMalformed(t *testing.T) {
	for _, b := range [][]byte{nil, {1}, make([]byte, 16)} { if _, err := DecryptCBC(make([]byte, 32), make([]byte, 16), b, 16); err == nil { t.Fatal("bad ciphertext accepted") } }
}
type memDuplex struct { r *bytes.Reader; w bytes.Buffer; closed bool }
func (m *memDuplex) Read(p []byte) (int, error) { return m.r.Read(p) }
func (m *memDuplex) Write(p []byte) (int, error) { return m.w.Write(p) }
func (m *memDuplex) Close() error { m.closed = true; return nil }
func socketFor(b []byte) (*Socket, *memDuplex) {
	m := &memDuplex{r: bytes.NewReader(b)}
	return &Socket{body: m, cancel: func() {}, write: make(chan struct{}, 1)}, m
}
func TestWSFragmentsAndPing(t *testing.T) {
	s, m := socketFor([]byte{1, 2, 'h', 'e', 0x89, 1, 'p', 0x80, 3, 'l', 'l', 'o'})
	op, b, err := s.Read(context.Background())
	if err != nil || op != 1 || string(b) != "hello" { t.Fatal(op, string(b), err) }
	if m.w.Len() < 7 || m.w.Bytes()[0] != 0x8a || m.w.Bytes()[1]&0x80 == 0 { t.Fatal("missing masked pong") }
}
func TestWSRejectsFrames(t *testing.T) {
	for _, b := range [][]byte{{0x81, 0x80}, {0xc1, 0}, {0x80, 0}, {0x09, 0}, {0x83, 0}, {0x81, 1, 0xff}, {0x81, 126, 0, 1, 'a'}} {
		s, _ := socketFor(b)
		if _, _, err := s.Read(context.Background()); err == nil { t.Fatalf("accepted %x", b) }
	}
}
func TestWSMasksOutbound(t *testing.T) {
	s, m := socketFor(nil)
	if err := s.Write(context.Background(), 1, []byte("test")); err != nil { t.Fatal(err) }
	b := m.w.Bytes()
	if b[1] != 0x84 { t.Fatal("unmasked") }
	for i := 0; i < 4; i++ { if b[6+i]^b[2+i] != []byte("test")[i] { t.Fatal("wrong mask") } }
}
func TestWSUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil { return }
		defer conn.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		_, _ = rw.Write([]byte{0x81, 2, 'o', 'k'})
		_ = rw.Flush()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := Dial(ctx, nil, strings.Replace(srv.URL, "http:", "ws:", 1))
	if err != nil { t.Fatal(err) }
	defer s.Close()
	_, b, err := s.Read(ctx)
	if err != nil || string(b) != "ok" { t.Fatal(string(b), err) }
}
