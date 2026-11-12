package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Options struct {
	Handler                        Sink
	Authorize                      Authorizer
	MaxConcurrent, MaxDedupEntries int
	DedupTTL                       time.Duration
}
type admission struct {
	done    chan struct{}
	err     error
	expires time.Time
	pending bool
}
type lane struct {
	token chan struct{}
	refs  int
}

// Gateway serializes admission per session, deduplicates successful admissions in
// memory, and routes outbound messages. Durable inbox/outbox storage belongs to
// the application; this type never claims crash-safe or exactly-once delivery.
type Gateway struct {
	mu       sync.Mutex
	adapters map[string]Adapter
	seen     map[string]*admission
	lanes    map[string]*lane
	slots    chan struct{}
	opts     Options
	running  bool
}

func New(o Options) (*Gateway, error) {
	if o.Handler == nil {
		return nil, errors.New("gateway: Handler is required")
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 32
	}
	if o.MaxConcurrent > 1024 {
		return nil, errors.New("gateway: MaxConcurrent exceeds 1024")
	}
	if o.MaxDedupEntries <= 0 {
		o.MaxDedupEntries = 10000
	}
	if o.DedupTTL <= 0 {
		o.DedupTTL = 10 * time.Minute
	}
	return &Gateway{opts: o, adapters: map[string]Adapter{}, seen: map[string]*admission{}, lanes: map[string]*lane{}, slots: make(chan struct{}, o.MaxConcurrent)}, nil
}
func (g *Gateway) Register(a Adapter) error {
	if a == nil || a.Account().Validate() != nil {
		return ErrInvalidEvent
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return ErrAlreadyRunning
	}
	k := a.Account().Key()
	if _, ok := g.adapters[k]; ok {
		return fmt.Errorf("gateway: account already registered: %s", k)
	}
	g.adapters[k] = a
	return nil
}
func (g *Gateway) adapter(a Account) (Adapter, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.adapters[a.Key()]
	if !ok {
		return nil, ErrUnknownAccount
	}
	return v, nil
}

// Bind rejects accidental cross-account injection even from another adapter.
func (g *Gateway) Bind(a Account) Sink {
	return func(ctx context.Context, e Event) error {
		if e.Route.Account != a {
			return ErrUnauthorized
		}
		return g.Accept(ctx, e)
	}
}
func (g *Gateway) Accept(ctx context.Context, e Event) (err error) {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if e.ID == "" || e.MessageID == "" || e.SenderID == "" || e.Route.Validate() != nil {
		return ErrInvalidEvent
	}
	if _, err = g.adapter(e.Route.Account); err != nil {
		return err
	}
	if g.opts.Authorize == nil {
		return ErrUnauthorized
	}
	if err = g.opts.Authorize(ctx, e); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return ErrUnauthorized
		}
		return err
	}
	if e.SenderIsBot {
		return nil
	}
	k := key(e.Route.SessionKey(), e.ID)
	g.mu.Lock()
	now := time.Now()
	for id, v := range g.seen {
		if !v.pending && !now.Before(v.expires) {
			delete(g.seen, id)
		}
	}
	if old, ok := g.seen[k]; ok {
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-old.done:
			return old.err
		}
	}
	if len(g.seen) >= g.opts.MaxDedupEntries {
		g.mu.Unlock()
		return ErrBusy
	}
	a := &admission{done: make(chan struct{}), pending: true}
	g.seen[k] = a
	g.mu.Unlock()
	defer func() {
		if recover() != nil {
			err = errors.New("gateway: admission handler panicked")
		}
		g.mu.Lock()
		a.pending = false
		a.err = err
		a.expires = time.Now().Add(g.opts.DedupTTL)
		if err != nil {
			delete(g.seen, k)
		}
		close(a.done)
		g.mu.Unlock()
	}()
	// Acquire the per-session lane BEFORE the global execution slot: a busy
	// conversation cannot occupy every execution slot while waiting for itself.
	session := e.Route.SessionKey()
	g.mu.Lock()
	l := g.lanes[session]
	if l == nil {
		l = &lane{token: make(chan struct{}, 1)}
		g.lanes[session] = l
	}
	l.refs++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(g.lanes, session)
		}
		g.mu.Unlock()
	}()
	select {
	case l.token <- struct{}{}:
		defer func() { <-l.token }()
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.opts.Handler(ctx, e)
}
func (g *Gateway) Send(ctx context.Context, r Route, m Message) (Receipt, error) {
	if r.Validate() != nil {
		return Receipt{}, ErrInvalidEvent
	}
	a, err := g.adapter(r.Account)
	if err != nil {
		return Receipt{}, err
	}
	c := a.Capabilities()
	if !c.Text || (m.Format == "markdown" && !c.Markdown) || (len(m.Attachments) > 0 && !c.SendMedia) || (len(m.Card) > 0 && !c.Card) || (m.ReplyTo != "" && !c.Reply) || (m.ReplyTo == "" && !c.Proactive) {
		return Receipt{}, ErrUnsupported
	}
	if m.Format != "" && m.Format != "text" && m.Format != "markdown" {
		return Receipt{}, ErrUnsupported
	}
	return a.Send(ctx, r, m) // no blind retries of non-idempotent sends
}
func (g *Gateway) UpdateStream(ctx context.Context, r Route, u StreamUpdate) (Receipt, error) {
	if r.Validate() != nil || u.ReplyTo == "" || u.StreamID == "" {
		return Receipt{}, ErrInvalidEvent
	}
	a, err := g.adapter(r.Account)
	if err != nil {
		return Receipt{}, err
	}
	s, ok := a.(StreamSender)
	if !ok || !a.Capabilities().Stream {
		return Receipt{}, ErrUnsupported
	}
	return s.UpdateStream(ctx, r, u)
}

// Run starts adapters concurrently. HTTP adapters are mounted by the host using
// their Handler(g.Bind(account)); they do not silently bind a public listener.
func (g *Gateway) Run(ctx context.Context) error {
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return ErrAlreadyRunning
	}
	g.running = true
	as := make([]Adapter, 0, len(g.adapters))
	for _, a := range g.adapters {
		as = append(as, a)
	}
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.running = false; g.mu.Unlock() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan error, len(as))
	var wg sync.WaitGroup
	for _, a := range as {
		wg.Add(1)
		go func(a Adapter) { defer wg.Done(); ch <- a.Run(ctx, g.Bind(a.Account())) }(a)
	}
	var err error
	if len(as) == 0 {
		err = ErrUnknownAccount
	} else {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case err = <-ch:
			if err == nil {
				err = errors.New("gateway: adapter stopped unexpectedly")
			}
		}
	}
	cancel()
	wg.Wait()
	return err
}
