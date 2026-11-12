package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

type mediaAdapter struct {
	fakeAdapter
	calls int
}

func (a *mediaAdapter) Capabilities() Capabilities {
	return Capabilities{DownloadMedia: true, UploadMedia: true}
}
func (a *mediaAdapter) Download(context.Context, Route, Attachment) (MediaContent, error) {
	a.calls++
	return MediaContent{Data: []byte("payload")}, nil
}
func (a *mediaAdapter) Upload(_ context.Context, r Route, _ MediaUpload) (Attachment, error) {
	a.calls++
	return Attachment{Kind: "file", Ref: "uploaded", Scope: &r}, nil
}

func TestMediaValidationBeforeDispatch(t *testing.T) {
	a := &mediaAdapter{fakeAdapter: fakeAdapter{a: Account{"test", "bot"}}}
	g, err := New(Options{Handler: func(context.Context, Event) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	// fakeAdapter's Account fixture is used instead of inventing a second identity.
	if err = g.Register(a); err != nil {
		t.Fatal(err)
	}
	r := Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}
	foreign := r
	foreign.ChatID = "foreign"
	for _, p := range []Attachment{{Ref: "x", Scope: &foreign}, {Ref: "x", ExpiresAt: time.Now().Add(-time.Second)}, {}} {
		if _, err = g.Download(context.Background(), r, p); err == nil {
			t.Fatal("invalid reference accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = g.Upload(ctx, r, MediaUpload{Kind: "file", Data: []byte("x")}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = g.Upload(context.Background(), r, MediaUpload{Kind: "file"}); err == nil {
		t.Fatal("empty upload accepted")
	}
	if a.calls != 0 {
		t.Fatal("invalid input performed IO")
	}
	p, err := g.Upload(context.Background(), r, MediaUpload{Kind: "file", Data: []byte("x")})
	if err != nil || p.Scope == nil || *p.Scope != r {
		t.Fatal(p, err)
	}
	data, err := g.Download(context.Background(), r, p)
	if err != nil || string(data.Data) != "payload" {
		t.Fatal(data, err)
	}
}

func TestMediaCapabilitiesDoNotImplyByteIO(t *testing.T) {
	g, _ := New(Options{Handler: func(context.Context, Event) error { return nil }})
	a := fakeAdapter{a: Account{"test", "bot"}}
	if err := g.Register(a); err != nil {
		t.Fatal(err)
	}
	r := Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}
	if _, err := g.Download(context.Background(), r, Attachment{Ref: "x"}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}
