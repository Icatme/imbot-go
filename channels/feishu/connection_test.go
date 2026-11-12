package feishu

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

// Exercise endpoint discovery, the actual WebSocket upgrade, protobuf
// fragmentation and admission ACKs together, independently of frame round trips.
func TestWebSocketAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		fail, post, unknown, card, cardFrame bool
	}{
		{name: "text"}, {name: "sink_failure", fail: true}, {name: "post", post: true}, {name: "unknown", unknown: true},
		{name: "card_event", card: true}, {name: "card_frame", card: true, cardFrame: true}, {name: "card_sink_failure", card: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fail := tc.fail
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			peerDone := make(chan error, 1)
			var serverURL string
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/callback/ws/endpoint" {
					json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"URL": "ws" + strings.TrimPrefix(serverURL, "http") + "/socket?service_id=123"}})
					return
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					peerDone <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				err = func() error {
					sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
					fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
					if err := rw.Flush(); err != nil {
						return err
					}
					ping, err := readClientFrame(rw.Reader)
					if err != nil {
						return err
					}
					if ping.Method != 0 || ping.Service != 123 || ping.Headers["type"] != "ping" {
						return fmt.Errorf("invalid ping: %+v", ping)
					}
					body := payload()
					if tc.post {
						body = messagePayload("post", mixedPost)
					}
					if tc.unknown {
						body = messagePayload("future-format", `{}`)
					}
					if tc.card {
						body = cardPayload()
					}
					frameType := "event"
					if tc.cardFrame {
						frameType = "card"
					}
					for i, p := range [][]byte{body[:len(body)/2], body[len(body)/2:]} {
						f := frame{Seq: uint64(40 + i), Log: 55, Service: 123, Method: 1, Headers: map[string]string{"type": frameType, "message_id": "delivery", "sum": "2", "seq": fmt.Sprint(i)}, Payload: p}
						b := f.encode()
						h := []byte{0x82, 126, byte(len(b) >> 8), byte(len(b))}
						if len(b) < 126 {
							h = []byte{0x82, byte(len(b))}
						}
						if _, err := rw.Write(append(h, b...)); err != nil {
							return err
						}
						if err := rw.Flush(); err != nil {
							return err
						}
					}
					ack, err := readClientFrame(rw.Reader)
					if err != nil {
						return err
					}
					var result struct {
						Code int    `json:"code"`
						Data []byte `json:"data"`
					}
					if err := json.Unmarshal(ack.Payload, &result); err != nil {
						return err
					}
					want := 200
					if fail {
						want = 500
					}
					if tc.card && !fail && string(result.Data) != "{}" {
						return fmt.Errorf("missing card action response: %s", ack.Payload)
					}
					if ack.Seq != 41 || ack.Log != 55 || ack.Service != 123 || ack.Headers["message_id"] != "delivery" || result.Code != want {
						return fmt.Errorf("invalid ACK: %+v payload=%s", ack, ack.Payload)
					}
					return nil
				}()
				peerDone <- err
				<-ctx.Done()
			}))
			defer s.Close()
			serverURL = s.URL
			a := testAdapter(t)
			a.cfg.BaseURL = s.URL
			a.rememberCardRoute(cardRoute(a), "msg")
			reports := 0
			a.cfg.OnError = func(err error) {
				var unsupported *gateway.UnsupportedMessageError
				if !errors.As(err, &unsupported) || unsupported.MessageType != "future-format" {
					t.Errorf("unexpected diagnostic: %v", err)
				}
				reports++
			}
			clientDone := make(chan error, 1)
			calls := 0
			go func() {
				clientDone <- a.connect(ctx, func(_ context.Context, e gateway.Event) error {
					calls++
					wantText := "你好"
					if tc.post {
						wantText = "主题\n看图和文件\n链接 (https://example.com)"
					}
					if tc.card {
						wantText = ""
						if e.Interaction == nil || e.Route != cardRoute(a) {
							return errors.New("card interaction lost its route")
						}
					}
					if e.MessageID != "msg" || e.Text != wantText {
						return fmt.Errorf("incorrect assembled event: %+v", e)
					}
					if fail {
						return errors.New("inbox unavailable")
					}
					return nil
				})
			}()
			select {
			case err := <-peerDone:
				cancel()
				if err != nil {
					t.Error(err)
				}
			case <-time.After(6 * time.Second):
				cancel()
				t.Error("WebSocket peer timed out")
			}
			select {
			case <-clientDone:
				wantCalls, wantReports := 1, 0
				if tc.unknown {
					wantCalls, wantReports = 0, 1
				}
				if calls != wantCalls || reports != wantReports {
					t.Errorf("admissions=%d diagnostics=%d, want %d/%d", calls, reports, wantCalls, wantReports)
				}
			case <-time.After(time.Second):
				t.Fatal("connect did not exit on cancellation")
			}
		})
	}
}

func readClientFrame(r *bufio.Reader) (frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return frame{}, err
	}
	if h[0] != 0x82 || h[1]&0x80 == 0 {
		return frame{}, errors.New("expected masked binary client frame")
	}
	n := int(h[1] & 0x7f)
	if n == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return frame{}, err
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	} else if n == 127 {
		return frame{}, errors.New("unexpected oversized fixture frame")
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return frame{}, err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return frame{}, err
	}
	for i := range b {
		b[i] ^= mask[i%4]
	}
	return decodeFrame(b)
}
