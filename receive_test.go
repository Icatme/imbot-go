package imbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Icatme/imbot-go/internal/store"
	"github.com/Icatme/imbot-go/receive"
)

func TestNativeReceivePolicyDropCommitsWithoutHooksOrQuoteCache(t *testing.T) {
	dir := t.TempDir()
	b := newQuoteTestBot(t, dir, "account")
	b.opts.ReceivePolicy = receive.Policy{Disabled: []receive.Kind{receive.Image}}
	var notified int
	b.opts.OnReceiveRejected = func(ctx context.Context, msg *IncomingMessage, rejection *receive.Rejection) {
		notified++
		if rejection.Kind != receive.Image || msg.sessionBot != b || msg.ContextToken != "reply-context" {
			t.Fatalf("missing reason/reply binding: %+v", rejection)
		}
		if session, ok := ctx.Value(handlerSessionKey{}).(handlerSession); !ok || session.bot != b {
			t.Fatal("rejection callback lacks reply session")
		}
	}
	b.Hooks().AfterReceive.Register(func(*IncomingMessage) error { t.Fatal("rejected message reached hook"); return nil })
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		t.Fatal("rejected message reached handler")
		return AckMessage()
	})
	wire := WireMessage{MessageID: "blocked", FromUserID: "peer", MessageType: MessageTypeUser, ContextToken: "reply-context",
		ItemList: []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "caption"}}, {Type: ItemImage, ImageItem: &ImageItem{URL: "https://unused.invalid/image"}}}}
	batch := updateBatch{messages: []json.RawMessage{marshalWireMessage(t, wire)}, cursor: "after-drop"}
	for range 2 {
		if err := b.processUpdateBatch(t.Context(), handler, batch); err != nil {
			t.Fatal(err)
		}
	}
	if notified != 1 || b.cursorStore.Get() != "after-drop" {
		t.Fatalf("notices=%d cursor=%s", notified, b.cursorStore.Get())
	}
	key := store.QuoteKey{Account: "account", Peer: "peer", Message: "blocked"}
	if _, found, err := b.quoteStore.Get(key); err != nil || found {
		t.Fatalf("rejected text cached: %v %v", found, err)
	}
	if _, found, err := b.quoteStore.GetMedia(key); err != nil || found {
		t.Fatalf("rejected media cached: %v %v", found, err)
	}
	restarted := newQuoteTestBot(t, dir, "account")
	if err := restarted.replayStore.Load(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.processWireMessage(t.Context(), handler, 0, &wire); err != nil {
		t.Fatal(err)
	}
}

func TestNativeReceivePolicyContentKinds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  receive.Policy
		items   []MessageItem
		dropped bool
	}{
		{"default", receive.Policy{}, []MessageItem{{Type: ItemFile, FileItem: &FileItem{FileName: "program.exe"}}}, false},
		{"voice_transcript", receive.Policy{Disabled: []receive.Kind{receive.Voice}}, []MessageItem{{Type: ItemVoice, VoiceItem: &VoiceItem{Text: "transcript"}}}, true},
		{"voice_not_text", receive.Policy{Disabled: []receive.Kind{receive.Text}}, []MessageItem{{Type: ItemVoice, VoiceItem: &VoiceItem{Text: "transcript"}}}, false},
		{"mixed", receive.Policy{Disabled: []receive.Kind{receive.Voice}}, []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "text"}}, {Type: ItemVoice, VoiceItem: &VoiceItem{Text: "transcript"}}}, true},
		{"file_extension", receive.Policy{FileExtensions: []string{"pdf"}}, []MessageItem{{Type: ItemFile, FileItem: &FileItem{FileName: "document.PDF"}}}, false},
		{"missing_name", receive.Policy{FileExtensions: []string{"pdf"}}, []MessageItem{{Type: ItemFile, FileItem: &FileItem{Media: &CDNMedia{FullURL: "https://unused.invalid/file"}}}}, true},
		{"missing_mime", receive.Policy{FileMIMETypes: []string{"application/pdf"}}, []MessageItem{{Type: ItemFile, FileItem: &FileItem{FileName: "document.pdf"}}}, true},
		{"quote", receive.Policy{Disabled: []receive.Kind{receive.File}}, []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "explain"}, RefMsg: &RefMessage{MessageItem: &MessageItem{Type: ItemFile, FileItem: &FileItem{FileName: "a.pdf"}}}}}, true},
		{"quote_extension", receive.Policy{FileExtensions: []string{"pdf"}}, []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "explain"}, RefMsg: &RefMessage{MessageItem: &MessageItem{Type: ItemFile, FileItem: &FileItem{FileName: "a.exe"}}}}}, true},
		{"nested_quote", receive.Policy{Disabled: []receive.Kind{receive.Image}}, []MessageItem{{Type: ItemText, RefMsg: &RefMessage{MessageItem: &MessageItem{Type: ItemText, RefMsg: &RefMessage{MessageItem: &MessageItem{Type: ItemImage}}}}}}, true},
		{"inconsistent_type", receive.Policy{Disabled: []receive.Kind{receive.Image}}, []MessageItem{{Type: ItemText, ImageItem: &ImageItem{URL: "image"}}}, true},
		{"tool", receive.Policy{Disabled: []receive.Kind{receive.ToolCall}}, []MessageItem{{Type: ItemToolCallStart, ToolCallStartItem: &ToolCallStartItem{ToolName: "tool"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newQuoteTestBot(t, t.TempDir(), "account")
			b.opts.ReceivePolicy = tc.policy
			var called, notified bool
			b.opts.OnReceiveRejected = func(context.Context, *IncomingMessage, *receive.Rejection) { notified = true }
			wire := &WireMessage{MessageID: "m", FromUserID: "peer", MessageType: MessageTypeUser, ItemList: tc.items}
			err := b.processWireMessage(t.Context(), MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult { called = true; return AckMessage() }), 0, wire)
			if err != nil {
				t.Fatal(err)
			}
			if called == tc.dropped || notified != tc.dropped {
				t.Fatalf("called=%v notified=%v wantDrop=%v", called, notified, tc.dropped)
			}
		})
	}
}

func TestNativeReceivePolicyChecksRestoredQuote(t *testing.T) {
	b := newQuoteTestBot(t, t.TempDir(), "account")
	key := store.QuoteKey{Account: "account", Peer: "peer", Message: "old"}
	b.cacheQuoteMedia("account", "peer", "old", &QuotedMedia{Type: ContentFile, FileName: "old.exe"}, nil)
	if _, found, err := b.quoteStore.GetMedia(key); err != nil || !found {
		t.Fatalf("seed quote=%v %v", found, err)
	}
	b.opts.ReceivePolicy = receive.Policy{FileExtensions: []string{"pdf"}}
	var rejected bool
	b.opts.OnReceiveRejected = func(_ context.Context, msg *IncomingMessage, r *receive.Rejection) {
		rejected = true
		if msg.QuotedMessage.Resolution != QuoteCached || r.Reason != receive.FileExtension {
			t.Fatalf("quote=%+v rejection=%v", msg.QuotedMessage, r)
		}
	}
	wire := &WireMessage{MessageID: "new", FromUserID: "peer", MessageType: MessageTypeUser, ItemList: []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "explain"}, RefMsg: &RefMessage{ServerID: "old"}}}}
	if err := b.processWireMessage(t.Context(), MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		t.Fatal("quoted file bypassed policy")
		return AckMessage()
	}), 0, wire); err != nil {
		t.Fatal(err)
	}
	if !rejected {
		t.Fatal("missing rejection")
	}
}

func TestNativeReceivePolicyValidationSnapshotAndCallbackPanic(t *testing.T) {
	disabled := []receive.Kind{receive.Image}
	b := New(Options{ReceivePolicy: receive.Policy{Disabled: disabled}})
	disabled[0] = receive.Text
	if b.opts.ReceivePolicy.Disabled[0] != receive.Image {
		t.Fatal("constructor retained caller slice")
	}
	b.opts.ReceivePolicy.Disabled = []receive.Kind{"typo"}
	if err := b.Run(t.Context()); !errors.Is(err, receive.ErrInvalidPolicy) {
		t.Fatalf("Run invalid policy=%v", err)
	}
	b = newQuoteTestBot(t, t.TempDir(), "account")
	b.opts.ReceivePolicy = receive.Policy{Disabled: []receive.Kind{receive.Text}}
	b.opts.OnReceiveRejected = func(context.Context, *IncomingMessage, *receive.Rejection) { panic("notice failed") }
	wire := &WireMessage{MessageID: "m", FromUserID: "peer", MessageType: MessageTypeUser, ItemList: []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "hello"}}}}
	h := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		t.Fatal("unexpected handler")
		return AckMessage()
	})
	if err := b.processWireMessage(t.Context(), h, 0, wire); err == nil {
		t.Fatal("callback panic consumed")
	}
	if b.replayStore.SeenAny(replayKeys(wire)...) {
		t.Fatal("callback panic committed replay")
	}
	b.opts.OnReceiveRejected = nil
	if err := b.processWireMessage(t.Context(), h, 0, wire); err != nil {
		t.Fatal(err)
	}
}

func TestNativeReceivePolicyRestoredMixedQuoteKeepsOriginalKinds(t *testing.T) {
	text := MessageItem{Type: ItemText, TextItem: &TextItem{Text: "caption"}}
	voice := MessageItem{Type: ItemVoice, VoiceItem: &VoiceItem{Text: "transcript"}}
	img := MessageItem{Type: ItemImage, ImageItem: &ImageItem{URL: "https://unused.invalid/image"}}
	file := MessageItem{Type: ItemFile, FileItem: &FileItem{FileName: "a.pdf"}}
	for _, tc := range []struct {
		name     string
		items    []MessageItem
		disabled receive.Kind
		drop     bool
	}{
		{"caption_file", []MessageItem{text, file}, receive.Text, true},
		{"image_voice", []MessageItem{img, voice}, receive.Voice, true},
		{"image_transcript_not_text", []MessageItem{img, voice}, receive.Text, false},
		{"voice_transcript_not_text", []MessageItem{voice}, receive.Text, false},
		{"voice_caption", []MessageItem{text, voice}, receive.Text, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newQuoteTestBot(t, t.TempDir(), "account")
			original := &WireMessage{MessageID: "old", FromUserID: "peer", MessageType: MessageTypeUser, ItemList: tc.items}
			if err := b.processWireMessage(t.Context(), MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult { return AckMessage() }), 0, original); err != nil {
				t.Fatal(err)
			}
			opts := b.opts
			opts.ReceivePolicy = receive.Policy{Disabled: []receive.Kind{tc.disabled}}
			restarted := New(opts)
			restarted.creds = b.creds
			replyItem := img
			replyItem.RefMsg = &RefMessage{ServerID: "old"}
			reply := &WireMessage{MessageID: "new", FromUserID: "peer", MessageType: MessageTypeUser, ItemList: []MessageItem{replyItem}}
			var called, rejected bool
			restarted.opts.OnReceiveRejected = func(context.Context, *IncomingMessage, *receive.Rejection) { rejected = true }
			err := restarted.processWireMessage(t.Context(), MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
				called = true
				if msg.QuotedMessage.Resolution != QuoteCached {
					t.Fatal("quote not restored")
				}
				return AckMessage()
			}), 0, reply)
			if err != nil || called == tc.drop || rejected != tc.drop {
				t.Fatalf("handler=%v rejected=%v err=%v", called, rejected, err)
			}
		})
	}
}

func TestNativeReceivePolicyUnresolvedFileQuoteNeedsMetadata(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(fmt.Sprint(resolved), func(t *testing.T) {
			b := newQuoteTestBot(t, t.TempDir(), "account")
			b.opts.ReceivePolicy = receive.Policy{FileExtensions: []string{"pdf"}}
			if resolved {
				b.cacheQuoteMedia("account", "peer", "original", &QuotedMedia{Type: ContentFile, FileName: "a.pdf"}, nil)
			}
			wire := &WireMessage{MessageID: "new", FromUserID: "peer", MessageType: MessageTypeUser, ItemList: []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "look"}, RefMsg: &RefMessage{ServerID: "original", MessageItem: &MessageItem{Type: ItemFile, FileItem: &FileItem{}}}}}}
			var called, rejected bool
			b.opts.OnReceiveRejected = func(context.Context, *IncomingMessage, *receive.Rejection) { rejected = true }
			if err := b.processWireMessage(t.Context(), MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult { called = true; return AckMessage() }), 0, wire); err != nil {
				t.Fatal(err)
			}
			if called != resolved || rejected == resolved {
				t.Fatalf("handler=%v rejected=%v resolved=%v", called, rejected, resolved)
			}
		})
	}
}
