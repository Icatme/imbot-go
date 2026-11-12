package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

type batchAdapter struct {
	fakeAdapter
	sent []Message
	fail int
}

func (a *batchAdapter) Capabilities() Capabilities {
	return Capabilities{Text: true, Reply: true, Proactive: true, MaxTextBytes: 5}
}
func (a *batchAdapter) Send(_ context.Context, _ Route, m Message) (Receipt, error) {
	if a.fail > 0 && len(a.sent) == a.fail {
		return Receipt{}, &APIError{Platform: "test", Code: "timeout", Uncertain: true}
	}
	a.sent = append(a.sent, m)
	return Receipt{Accepted: true, MessageID: m.ClientID}, nil
}
func TestSendTextPreservesUTF8AndPartialReceipts(t *testing.T) {
	a := &batchAdapter{fakeAdapter: fakeAdapter{a: Account{"test", "bot"}}}
	g, _ := New(Options{Handler: func(context.Context, Event) error { return nil }})
	g.Register(a)
	r := Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}
	text := "中文🙂long-text"
	result, err := g.SendText(context.Background(), r, Message{Text: text, ReplyTo: "source", ClientID: "operation"})
	if err != nil || len(result.Receipts) < 2 {
		t.Fatal(result, err)
	}
	var joined strings.Builder
	for _, m := range a.sent {
		if !utf8.ValidString(m.Text) || len(m.Text) > 5 || m.ReplyTo != "source" {
			t.Fatal(m)
		}
		joined.WriteString(m.Text)
	}
	if joined.String() != text {
		t.Fatal(joined.String())
	}
	a.sent = nil
	a.fail = 1
	result, err = g.SendText(context.Background(), r, Message{Text: text})
	var partial *PartialSendError
	var api *APIError
	if !errors.As(err, &partial) || partial.Completed != 1 || !errors.As(err, &api) || !api.Uncertain || len(result.Receipts) != 1 || len(a.sent) != 1 {
		t.Fatal(result, err)
	}
}

func TestSendTextValidatesEntireQQSequenceRangeBeforeSending(t *testing.T) {
	a := &batchAdapter{fakeAdapter: fakeAdapter{a: Account{"qqbot", "bot"}}}
	g, _ := New(Options{Handler: func(context.Context, Event) error { return nil }})
	if err := g.Register(a); err != nil {
		t.Fatal(err)
	}
	r := Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}
	for _, sequence := range []int{-1, 2147483647} {
		_, err := g.SendText(context.Background(), r, Message{Text: "123456", Sequence: sequence, ReplyTo: "source"})
		if !errors.Is(err, ErrInvalidEvent) || len(a.sent) != 0 {
			t.Fatalf("invalid sequence range %d sent %d parts, err=%v", sequence, len(a.sent), err)
		}
	}
	if _, err := g.SendText(context.Background(), r, Message{Text: "123456", Sequence: 2147483646, ReplyTo: "source"}); err != nil {
		t.Fatal(err)
	}
	if len(a.sent) != 2 || a.sent[0].Sequence != 2147483646 || a.sent[1].Sequence != 2147483647 {
		t.Fatal(a.sent)
	}
}
