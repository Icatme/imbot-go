package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

type socket interface {
	Write(context.Context, byte, []byte) error
	Read(context.Context) (byte, []byte, error)
	Close() error
}
type frame struct {
	Cmd     string            `json:"cmd,omitempty"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body,omitempty"`
	Code    *int              `json:"errcode,omitempty"`
}
type requestLane struct {
	gate chan struct{}
	refs int
}
type session struct {
	ctx     context.Context
	cancel  context.CancelFunc
	socket  socket
	timeout time.Duration
	mu      sync.Mutex
	pending map[string]chan frame
	lanes   map[string]*requestLane
	slots   chan struct{}
}

var sequence atomic.Uint64

func requestID(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), sequence.Add(1))
}
func newSession(ctx context.Context, s socket) *session {
	life, cancel := context.WithCancel(ctx)
	return &session{ctx: life, cancel: cancel, socket: s, timeout: 5 * time.Second, pending: map[string]chan frame{}, lanes: map[string]*requestLane{}, slots: make(chan struct{}, 128)}
}
func (s *session) close() { s.cancel(); _ = s.socket.Close() }
func (s *session) deliver(f frame) {
	s.mu.Lock()
	ch := s.pending[f.Headers["req_id"]]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- f:
		default:
		}
	}
}

// request serializes repeated req_ids. A timeout closes this connection so a
// late ACK cannot be attributed to a newer update using the same reply req_id.
func (s *session) request(ctx context.Context, cmd, id string, body any) (gateway.Receipt, error) {
	f, err := s.requestFrame(ctx, cmd, id, body, s.timeout)
	if err != nil {
		return gateway.Receipt{}, err
	}
	var b struct {
		ID string `json:"msgid"`
	}
	_ = json.Unmarshal(f.Body, &b)
	return gateway.Receipt{MessageID: b.ID, Accepted: true}, nil
}

func (s *session) requestFrame(ctx context.Context, cmd, id string, body any, timeout time.Duration) (frame, error) {
	if err := ctx.Err(); err != nil {
		return frame{}, err
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return frame{}, gateway.ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s.mu.Lock()
	l := s.lanes[id]
	if l == nil {
		l = &requestLane{gate: make(chan struct{}, 1)}
		s.lanes[id] = l
	}
	l.refs++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.lanes, id)
		}
		s.mu.Unlock()
	}()
	select {
	case l.gate <- struct{}{}:
		defer func() { <-l.gate }()
	case <-ctx.Done():
		return frame{}, ctx.Err()
	case <-s.ctx.Done():
		return frame{}, errors.New("wecom: connection closed")
	}
	// A cancelled waiter may win the gate select. It has not sent anything and
	// must not tear down the connection or report an uncertain delivery.
	if err := ctx.Err(); err != nil {
		return frame{}, err
	}
	if s.ctx.Err() != nil {
		return frame{}, errors.New("wecom: connection closed")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return frame{}, err
	}
	if body == nil {
		data = nil
	}
	raw, err := json.Marshal(frame{Cmd: cmd, Headers: map[string]string{"req_id": id}, Body: data})
	if err != nil {
		return frame{}, err
	}
	ch := make(chan frame, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	if err = s.socket.Write(ctx, 1, raw); err != nil {
		s.close()
		return frame{}, errors.Join(&gateway.APIError{Platform: "wecom", Code: "write", Uncertain: true}, ctx.Err())
	}
	select {
	case f := <-ch:
		if f.Code == nil {
			s.close()
			return frame{}, &gateway.APIError{Platform: "wecom", Code: "missing_ack_code", Uncertain: true}
		}
		if *f.Code != 0 {
			return frame{}, &gateway.APIError{Platform: "wecom", Code: fmt.Sprint(*f.Code)}
		}
		return f, nil
	case <-ctx.Done():
		s.close()
		return frame{}, errors.Join(&gateway.APIError{Platform: "wecom", Code: "ack_timeout", Uncertain: true}, ctx.Err())
	case <-s.ctx.Done():
		return frame{}, errors.Join(&gateway.APIError{Platform: "wecom", Code: "disconnected", Uncertain: true}, ctx.Err())
	}
}
