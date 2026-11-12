package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeAdapter struct{ a Account }

func (f fakeAdapter) Account() Account { return f.a }
func (f fakeAdapter) Capabilities() Capabilities {
	return Capabilities{Text: true, Reply: true, Proactive: true}
}
func (f fakeAdapter) Run(ctx context.Context, _ Sink) error { <-ctx.Done(); return ctx.Err() }
func (f fakeAdapter) Send(context.Context, Route, Message) (Receipt, error) {
	return Receipt{MessageID: "out", Accepted: true}, nil
}
func event(id string) Event {
	return Event{ID: id, MessageID: id, SenderID: "u", Route: Route{Account: Account{"test", "bot"}, ChatID: "chat", Kind: "direct"}}
}
func newTest(t *testing.T, h Sink) *Gateway {
	t.Helper()
	g, err := New(Options{Handler: h, Authorize: AllowAll})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Register(fakeAdapter{event("x").Route.Account}); err != nil {
		t.Fatal(err)
	}
	return g
}
func TestIsolationKeys(t *testing.T) {
	a := Route{Account: Account{"a:b", "c"}, ChatID: "d"}
	b := Route{Account: Account{"a", "b:c"}, ChatID: "d"}
	if a.SessionKey() == b.SessionKey() {
		t.Fatal("delimiter collision")
	}
	for _, field := range []string{"account", "tenant", "thread", "kind"} {
		c := a
		switch field {
		case "account":
			c.ID = "other"
		case "tenant":
			c.TenantID = "other"
		case "thread":
			c.ThreadID = "other"
		case "kind":
			c.Kind = "group"
		}
		if a.SessionKey() == c.SessionKey() {
			t.Fatal(field)
		}
	}
}
func TestFailClosed(t *testing.T) {
	g, _ := New(Options{Handler: func(context.Context, Event) error { t.Fatal("called"); return nil }})
	_ = g.Register(fakeAdapter{event("x").Route.Account})
	if !errors.Is(g.Accept(context.Background(), event("x")), ErrUnauthorized) {
		t.Fatal("allowed by default")
	}
}
func TestDedupAndRetry(t *testing.T) {
	n := 0
	g := newTest(t, func(context.Context, Event) error {
		n++
		if n == 1 {
			return errors.New("persist failed")
		}
		return nil
	})
	if g.Accept(context.Background(), event("1")) == nil {
		t.Fatal("accepted failure")
	}
	for i := 0; i < 3; i++ {
		if err := g.Accept(context.Background(), event("1")); err != nil {
			t.Fatal(err)
		}
	}
	if n != 2 {
		t.Fatalf("calls %d", n)
	}
}
func TestConcurrentDuplicate(t *testing.T) {
	var n atomic.Int32
	g := newTest(t, func(context.Context, Event) error { n.Add(1); time.Sleep(time.Millisecond); return nil })
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Accept(context.Background(), event("same")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatal(n.Load())
	}
}
func TestTenantDedupIsolation(t *testing.T) {
	n := 0
	g := newTest(t, func(context.Context, Event) error { n++; return nil })
	a := event("1")
	b := a
	b.Route.TenantID = "other"
	_ = g.Accept(context.Background(), a)
	_ = g.Accept(context.Background(), b)
	if n != 2 {
		t.Fatal(n)
	}
}

func TestDedupUsesFullRoute(t *testing.T) {
	var accepted int
	g := newTest(t, func(context.Context, Event) error { accepted++; return nil })
	base := event("same-platform-id")
	thread := base
	thread.Route.ThreadID = "thread"
	group := base
	group.Route.Kind = "group"
	for _, e := range []Event{base, thread, group, base, thread, group} {
		if err := g.Accept(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if accepted != 3 {
		t.Fatalf("accepted %d independent route events, want 3", accepted)
	}
}
func TestBoundAccount(t *testing.T) {
	g := newTest(t, func(context.Context, Event) error { return nil })
	a := event("1")
	a.Route.ID = "other"
	if !errors.Is(g.Bind(event("1").Route.Account)(context.Background(), a), ErrUnauthorized) {
		t.Fatal("cross-account accepted")
	}
}
func TestPanicReleasesAdmission(t *testing.T) {
	n := 0
	g := newTest(t, func(context.Context, Event) error {
		n++
		if n == 1 {
			panic("secret")
		}
		return nil
	})
	if err := g.Accept(context.Background(), event("1")); err == nil {
		t.Fatal("panic swallowed")
	}
	if err := g.Accept(context.Background(), event("1")); err != nil {
		t.Fatal(err)
	}
}
func TestPerSessionSerialization(t *testing.T) {
	var active atomic.Int32
	g := newTest(t, func(context.Context, Event) error {
		if active.Add(1) != 1 {
			t.Error("concurrent same session")
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return nil
	})
	var wg sync.WaitGroup
	for _, id := range []string{"1", "2", "3"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); _ = g.Accept(context.Background(), event(id)) }(id)
	}
	wg.Wait()
}
func TestRunCancellation(t *testing.T) {
	g := newTest(t, func(context.Context, Event) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(g.Run(ctx), context.Canceled) {
		t.Fatal("not canceled")
	}
}
func TestCapabilitiesAndUnknown(t *testing.T) {
	g := newTest(t, func(context.Context, Event) error { return nil })
	r := event("1").Route
	if _, err := g.Send(context.Background(), r, Message{Format: "markdown"}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	r.ID = "missing"
	if _, err := g.Send(context.Background(), r, Message{Text: "hi"}); !errors.Is(err, ErrUnknownAccount) {
		t.Fatal(err)
	}
}
