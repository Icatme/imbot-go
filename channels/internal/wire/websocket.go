package wire

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Icatme/wechatbot-go/gateway"
)

// Socket is a small RFC6455 client: no compression, bounded frames/messages,
// masked client frames, fragmented server messages and ping/pong control frames.
// HTTP transport supplies TLS verification and configured proxy behavior.
type Socket struct {
	body io.ReadWriteCloser
	cancel context.CancelFunc
	once sync.Once
	write chan struct{}
	read sync.Mutex
}
func Dial(ctx context.Context, c *http.Client, endpoint string) (*Socket, error) {
	if err := Endpoint(endpoint); err != nil { return nil, err }
	u, err := url.Parse(endpoint)
	if err != nil { return nil, err }
	switch u.Scheme {
	case "wss": u.Scheme = "https"
	case "ws": u.Scheme = "http"
	default: return nil, errors.New("WebSocket scheme required")
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil { return nil, err }
	key := base64.StdEncoding.EncodeToString(nonce)
	life, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(20*time.Second, cancel)
	req, err := http.NewRequestWithContext(life, "GET", u.String(), nil)
	if err != nil { timer.Stop(); cancel(); return nil, errors.New("invalid WebSocket request") }
	req.Header = http.Header{"Connection": []string{"Upgrade"}, "Upgrade": []string{"websocket"}, "Sec-Websocket-Version": []string{"13"}, "Sec-Websocket-Key": []string{key}}
	client := Client(c)
	client.Timeout = 0 // the upgraded response body is the connection lifetime
	res, err := client.Do(req)
	timer.Stop()
	if err != nil { cancel(); return nil, &gateway.APIError{Platform: "websocket", Code: "handshake", Retryable: true} }
	fail := func(err error) (*Socket, error) { res.Body.Close(); cancel(); return nil, err }
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if res.StatusCode != 101 { return fail(&gateway.APIError{Platform: "websocket", Status: res.StatusCode, Code: "upgrade", Retryable: res.StatusCode == 429 || res.StatusCode >= 500}) }
	if res.ProtoMajor != 1 || !headerToken(res.Header.Get("Connection"), "upgrade") || !strings.EqualFold(res.Header.Get("Upgrade"), "websocket") || res.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) || res.Header.Get("Sec-WebSocket-Extensions") != "" || res.Header.Get("Sec-WebSocket-Protocol") != "" { return fail(errors.New("invalid WebSocket handshake")) }
	body, ok := res.Body.(io.ReadWriteCloser)
	if !ok { return fail(errors.New("HTTP transport cannot upgrade")) }
	s := &Socket{body: body, cancel: cancel, write: make(chan struct{}, 1)}
	context.AfterFunc(life, func() { body.Close() })
	return s, nil
}
func headerToken(s, t string) bool {
	for _, v := range strings.Split(s, ",") { if strings.EqualFold(strings.TrimSpace(v), t) { return true } }
	return false
}
func (s *Socket) Close() error {
	var err error
	s.once.Do(func() { s.cancel(); err = s.body.Close() })
	return err
}
func (s *Socket) Write(ctx context.Context, opcode byte, p []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if opcode != 1 && opcode != 2 && opcode != 8 && opcode != 9 && opcode != 10 { return errors.New("invalid WebSocket opcode") }
	if len(p) > MaxBody || (opcode >= 8 && len(p) > 125) || (opcode == 1 && !utf8.Valid(p)) { return errors.New("invalid WebSocket payload") }
	select {
	case s.write <- struct{}{}: defer func() { <-s.write }()
	case <-ctx.Done(): return ctx.Err()
	}
	if ctx.Err() != nil { return ctx.Err() }
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil { return err }
	h := []byte{0x80 | opcode}
	n := len(p)
	switch {
	case n < 126: h = append(h, 0x80|byte(n))
	case n <= 65535: h = append(h, 0x80|126, byte(n>>8), byte(n))
	default: h = append(h, 0x80|127); h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	h = append(h, mask...)
	frame := make([]byte, len(h)+n)
	copy(frame, h)
	for i, v := range p { frame[len(h)+i] = v ^ mask[i%4] }
	for len(frame) > 0 {
		n, err := s.body.Write(frame)
		if err != nil { return errors.New("WebSocket write failed") }
		if n == 0 { return io.ErrShortWrite }
		frame = frame[n:]
	}
	return nil
}

// Read must have one owner. Cancellation closes the connection to unblock IO.
func (s *Socket) Read(ctx context.Context) (byte, []byte, error) {
	s.read.Lock()
	defer s.read.Unlock()
	if ctx.Err() != nil { return 0, nil, ctx.Err() }
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	var message []byte
	var kind byte
	for fragments := 0; fragments < 4096; fragments++ {
		var h [2]byte
		if _, err := io.ReadFull(s.body, h[:]); err != nil { return 0, nil, err }
		fin := h[0]&0x80 != 0
		op := h[0] & 15
		if h[0]&0x70 != 0 || h[1]&0x80 != 0 { return 0, nil, errors.New("invalid server frame flags") }
		n := uint64(h[1] & 127)
		if n == 126 {
			var b [2]byte
			if _, err := io.ReadFull(s.body, b[:]); err != nil { return 0, nil, err }
			n = uint64(binary.BigEndian.Uint16(b[:]))
			if n < 126 { return 0, nil, errors.New("noncanonical frame length") }
		} else if n == 127 {
			var b [8]byte
			if _, err := io.ReadFull(s.body, b[:]); err != nil { return 0, nil, err }
			n = binary.BigEndian.Uint64(b[:])
			if n < 65536 || n>>63 != 0 { return 0, nil, errors.New("invalid frame length") }
		}
		if n > MaxBody || uint64(len(message))+n > MaxBody { return 0, nil, errors.New("WebSocket message too large") }
		if op >= 8 && (!fin || n > 125) { return 0, nil, errors.New("invalid control frame") }
		p := make([]byte, int(n))
		if _, err := io.ReadFull(s.body, p); err != nil { return 0, nil, err }
		switch op {
		case 8:
			if len(p) == 1 || len(p) > 2 && !utf8.Valid(p[2:]) { return 0, nil, errors.New("invalid close frame") }
			return 0, nil, io.EOF
		case 9:
			if err := s.Write(ctx, 10, p); err != nil { return 0, nil, err }
			continue
		case 10: continue
		case 0:
			if kind == 0 { return 0, nil, errors.New("unexpected continuation") }
		case 1, 2:
			if kind != 0 { return 0, nil, errors.New("interleaved data frames") }
			kind = op
		default: return 0, nil, errors.New("unknown WebSocket opcode")
		}
		message = append(message, p...)
		if fin {
			if kind == 1 && !utf8.Valid(message) { return 0, nil, errors.New("invalid UTF-8 text frame") }
			return kind, message, nil
		}
	}
	return 0, nil, errors.New("too many WebSocket fragments")
}
