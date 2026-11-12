package wecom

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

func readClient(r *bufio.Reader) (frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return frame{}, err
	}
	n := int(h[1] & 127)
	if n == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return frame{}, err
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	if n > 65535 || h[1]&128 == 0 {
		return frame{}, fmt.Errorf("invalid test frame")
	}
	var key [4]byte
	if _, err := io.ReadFull(r, key[:]); err != nil {
		return frame{}, err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return frame{}, err
	}
	for i := range b {
		b[i] ^= key[i%4]
	}
	var f frame
	err := json.Unmarshal(b, &f)
	return f, err
}
func writeServer(w *bufio.ReadWriter, v frame) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h := []byte{0x81}
	if len(b) < 126 {
		h = append(h, byte(len(b)))
	} else {
		h = append(h, 126, byte(len(b)>>8), byte(len(b)))
	}
	if _, err = w.Write(append(h, b...)); err != nil {
		return err
	}
	return w.Flush()
}
func TestConnectionReadsACKWhileSinkSends(t *testing.T) {
	errs := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			errs <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		_ = rw.Flush()
		auth, err := readClient(rw.Reader)
		if err != nil {
			errs <- err
			return
		}
		zero := 0
		if err = writeServer(rw, frame{Headers: auth.Headers, Code: &zero}); err != nil {
			errs <- err
			return
		}
		unknown := pushed()
		unknown.Body = []byte(strings.Replace(string(unknown.Body), `"msgtype":"text"`, `"msgtype":"future"`, 1))
		if err = writeServer(rw, unknown); err != nil {
			errs <- err
			return
		}
		if err = writeServer(rw, cardEventFrame()); err != nil {
			errs <- err
			return
		}
		if err = writeServer(rw, pushed()); err != nil {
			errs <- err
			return
		}
		reply, err := readClient(rw.Reader)
		if err != nil {
			errs <- err
			return
		}
		if reply.Cmd != "aibot_respond_msg" {
			errs <- fmt.Errorf("wrong reply command")
			return
		}
		errs <- writeServer(rw, frame{Headers: reply.Headers, Code: &zero})
		_, _ = rw.ReadByte()
	}))
	defer srv.Close()
	reported := make(chan error, 1)
	a, err := New(Config{AccountID: "a", BotID: "bot", Secret: "secret", WebSocketURL: strings.Replace(srv.URL, "http:", "ws:", 1), OnError: func(err error) { reported <- err }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	admitted := make(chan error, 1)
	interactions := make(chan gateway.Event, 1)
	stopped := make(chan error, 1)
	go func() {
		stopped <- a.connect(ctx, func(ctx context.Context, e gateway.Event) error {
			if e.Interaction != nil {
				interactions <- e
				return nil
			}
			out, err := a.Send(ctx, e.Route, e.Reply("hello"))
			if err == nil && !out.Accepted {
				err = fmt.Errorf("not accepted")
			}
			admitted <- err
			return err
		})
	}()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("reader deadlocked behind sink send")
	}
	select {
	case err := <-reported:
		if unsupported, ok := err.(*gateway.UnsupportedMessageError); !ok || unsupported.MessageType != "future" {
			t.Fatal("unknown format was not reported", err)
		}
	default:
		t.Fatal("unknown format was silently consumed")
	}
	select {
	case e := <-interactions:
		if e.Interaction.Name != "template_card_event" {
			t.Fatal(e.Interaction)
		}
	default:
		t.Fatal("template action bypassed the admission sink")
	}
	cancel()
	<-stopped
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}
func TestNegativeACK(t *testing.T) {
	_, s, f := fixture(t)
	f.onWrite = func(b []byte) {
		var v frame
		_ = json.Unmarshal(b, &v)
		code := 400
		s.deliver(frame{Headers: v.Headers, Code: &code})
	}
	if out, err := s.request(context.Background(), "x", "id", nil); err == nil || out.Accepted {
		t.Fatal(out, err)
	}
}
