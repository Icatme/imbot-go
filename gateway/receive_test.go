package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Icatme/imbot-go/receive"
)

func TestReceivePolicyRejectsAllMessageProjections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy receive.Policy
		change func(*Event)
	}{
		{"attachment", receive.Policy{Disabled: []receive.Kind{receive.Image}}, func(e *Event) { e.Attachments = []Attachment{{Kind: "image"}} }},
		{"part", receive.Policy{Disabled: []receive.Kind{receive.Image}}, func(e *Event) { e.Parts = []ContentPart{{Text: "caption"}, {Attachment: &Attachment{Kind: "image"}}} }},
		{"quote", receive.Policy{Disabled: []receive.Kind{receive.File}}, func(e *Event) { e.Quote = &Quote{Attachments: []Attachment{{Kind: "file", Name: "secret.pdf"}}} }},
		{"quote_voice", receive.Policy{Disabled: []receive.Kind{receive.Voice}}, func(e *Event) { e.Quote = &Quote{Text: "transcript", ContentKinds: []receive.Kind{receive.Voice}} }},
		{"transcript", receive.Policy{Disabled: []receive.Kind{receive.Voice}}, func(e *Event) { e.Text = "transcript"; e.ContentKinds = []receive.Kind{receive.Voice} }},
		{"audio", receive.Policy{Disabled: []receive.Kind{receive.Voice}}, func(e *Event) { e.Attachments = []Attachment{{Kind: "audio"}} }},
		{"text_part", receive.Policy{Disabled: []receive.Kind{receive.Text}}, func(e *Event) { e.Text = ""; e.Parts = []ContentPart{{Text: "only part"}} }},
		{"interaction", receive.Policy{Disabled: []receive.Kind{receive.Interaction}}, func(e *Event) { e.Interaction = &Interaction{ID: "action"} }},
		{"extension", receive.Policy{FileExtensions: []string{"pdf"}}, func(e *Event) { e.Attachments = []Attachment{{Kind: "file", Name: "program.exe"}} }},
		{"quote_extension", receive.Policy{FileExtensions: []string{"pdf"}}, func(e *Event) { e.Quote = &Quote{Attachments: []Attachment{{Kind: "file", Name: "program.exe"}}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var called, rejected int
			g, err := New(Options{Authorize: AllowAll, ReceivePolicy: tc.policy,
				Handler: func(context.Context, Event) error { called++; return nil },
				OnReceiveRejected: func(_ context.Context, e Event, r *receive.Rejection) {
					rejected++
					if e.ID != "test" || r == nil {
						t.Fatal("missing rejection context")
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			e := event("test")
			e.Text = "caption"
			tc.change(&e)
			if err := g.Register(fakeAdapter{e.Route.Account}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				err := g.Accept(t.Context(), e)
				if !errors.Is(err, receive.ErrRejected) || !Consumed(fmt.Errorf("wrapped: %w", err)) {
					t.Fatalf("drop=%v", err)
				}
			}
			if called != 0 || rejected != 1 {
				t.Fatalf("handler=%d rejection=%d", called, rejected)
			}
		})
	}
}

func TestReceivePolicyPerAccountAndSnapshot(t *testing.T) {
	base := event("image")
	other := base
	other.Route.ID = "other"
	disabled := []receive.Kind{receive.Image}
	policies := map[Account]receive.Policy{other.Route.Account: {}}
	var calls int
	g, err := New(Options{Authorize: AllowAll, Handler: func(context.Context, Event) error { calls++; return nil },
		ReceivePolicy: receive.Policy{Disabled: disabled}, AccountReceivePolicies: policies})
	if err != nil {
		t.Fatal(err)
	}
	disabled[0] = receive.Text
	policies[other.Route.Account] = receive.Policy{Disabled: []receive.Kind{receive.Image}}
	for _, e := range []Event{base, other} {
		if err := g.Register(fakeAdapter{e.Route.Account}); err != nil {
			t.Fatal(err)
		}
	}
	base.Attachments = []Attachment{{Kind: "image"}}
	other.Attachments = base.Attachments
	if err := g.Accept(t.Context(), base); !errors.Is(err, receive.ErrRejected) {
		t.Fatal(err)
	}
	if err := g.Accept(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestReceivePolicyTranscriptIsNotIndependentText(t *testing.T) {
	g := newTest(t, func(context.Context, Event) error { return nil })
	g.opts.ReceivePolicy = receive.Policy{Disabled: []receive.Kind{receive.Text}}
	e := event("voice")
	e.Text = "transcription"
	e.Parts = []ContentPart{{Text: e.Text}}
	e.ContentKinds = []receive.Kind{receive.Voice}
	e.Quote = &Quote{Text: "quoted transcript", ContentKinds: []receive.Kind{receive.Voice}}
	if err := g.Accept(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	e.ID = "mixed"
	e.ContentKinds = append(e.ContentKinds, receive.Text)
	if err := g.Accept(t.Context(), e); !errors.Is(err, receive.ErrRejected) {
		t.Fatal(err)
	}
}

func TestReceivePolicyAuthorizationAndErrors(t *testing.T) {
	var notices int
	g, err := New(Options{ReceivePolicy: receive.Policy{Disabled: []receive.Kind{receive.Text}},
		Handler:           func(context.Context, Event) error { t.Fatal("unauthorized handler"); return nil },
		OnReceiveRejected: func(context.Context, Event, *receive.Rejection) { notices++ }})
	if err != nil {
		t.Fatal(err)
	}
	e := event("unauthorized")
	e.Text = "text"
	if err := g.Register(fakeAdapter{e.Route.Account}); err != nil {
		t.Fatal(err)
	}
	if err := g.Accept(t.Context(), e); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if notices != 0 {
		t.Fatal("rejected content notice preceded authorization")
	}
	if Consumed(receive.ErrInvalidPolicy) || Consumed(context.Canceled) {
		t.Fatal("configuration/cancellation treated as drop")
	}
	if _, err := New(Options{Handler: func(context.Context, Event) error { return nil }, ReceivePolicy: receive.Policy{Disabled: []receive.Kind{"bad"}}}); !errors.Is(err, receive.ErrInvalidPolicy) {
		t.Fatal(err)
	}
}

func TestReceivePolicyConcurrentDropAndCallbackPanic(t *testing.T) {
	var notices atomic.Int32
	g, err := New(Options{Authorize: AllowAll, ReceivePolicy: receive.Policy{Disabled: []receive.Kind{receive.Text}},
		Handler:           func(context.Context, Event) error { t.Error("dropped handler"); return nil },
		OnReceiveRejected: func(context.Context, Event, *receive.Rejection) { notices.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	e := event("drop")
	e.Text = "text"
	if err := g.Register(fakeAdapter{e.Route.Account}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Accept(t.Context(), e); !Consumed(err) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if notices.Load() != 1 {
		t.Fatal(notices.Load())
	}
	g.opts.OnReceiveRejected = func(context.Context, Event, *receive.Rejection) { panic("callback failed") }
	e.ID = "panic"
	if err := g.Accept(t.Context(), e); err == nil || Consumed(err) {
		t.Fatalf("panic consumed: %v", err)
	}
	g.opts.OnReceiveRejected = nil
	if err := g.Accept(t.Context(), e); !errors.Is(err, receive.ErrRejected) {
		t.Fatalf("panic leaked admission: %v", err)
	}
}

func TestReceivePolicyMissingFileMetadataCannotUseAnotherAttachment(t *testing.T) {
	for _, quoteUnknown := range []bool{false, true} {
		t.Run(fmt.Sprint(quoteUnknown), func(t *testing.T) {
			g := newTest(t, func(context.Context, Event) error { t.Fatal("missing filename bypassed policy"); return nil })
			g.opts.ReceivePolicy = receive.Policy{FileExtensions: []string{"pdf"}}
			e := event("quote")
			e.ContentKinds = []receive.Kind{receive.File}
			e.Quote = &Quote{ContentKinds: []receive.Kind{receive.File}}
			known := []Attachment{{Kind: "file", Name: "known.pdf"}}
			if quoteUnknown {
				e.Attachments = known
			} else {
				e.Quote.Attachments = known
			}
			if err := g.Accept(t.Context(), e); !errors.Is(err, receive.ErrRejected) {
				t.Fatal(err)
			}
		})
	}
}
