package imbot

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Icatme/imbot-go/internal/crypto"
	"github.com/Icatme/imbot-go/internal/store"
)

func deliverMediaWire(t *testing.T, b *Bot, wire WireMessage) *IncomingMessage {
	t.Helper()
	wire.FromUserID, wire.MessageType = "peer", MessageTypeUser
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return deliverQuoteTestWire(t, b, string(raw))
}

func quoteMediaItem(kind string, media *CDNMedia) MessageItem {
	switch kind {
	case "image":
		return MessageItem{Type: ItemImage, ImageItem: &ImageItem{Media: media}}
	case "video":
		return MessageItem{Type: ItemVideo, VideoItem: &VideoItem{Media: media}}
	case "voice":
		return MessageItem{Type: ItemVoice, VoiceItem: &VoiceItem{Media: media}}
	default:
		return MessageItem{Type: ItemFile, FileItem: &FileItem{Media: media, FileName: "original.pdf", Len: "8"}}
	}
}

func quotedWire(id MessageID, item *MessageItem) WireMessage {
	return WireMessage{MessageID: "2", ItemList: []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "question"}, RefMsg: &RefMessage{ServerID: id, MessageItem: item}}}}
}

var quoteTestWebMHeader = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}

func TestQuotedAttachmentsExplicitDownloadAndRestart(t *testing.T) {
	for _, kind := range []string{"image", "video", "voice", "file"} {
		t.Run(kind, func(t *testing.T) {
			payload := []byte("document")
			if kind == "video" {
				payload = quoteTestWebMHeader
			}
			key := []byte("0123456789abcdef")
			ciphertext, err := crypto.EncryptAESECB(payload, key)
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); _, _ = w.Write(ciphertext) }))
			defer server.Close()
			dir := t.TempDir()
			b := newQuoteTestBot(t, dir, "account")
			item := quoteMediaItem(kind, &CDNMedia{FullURL: server.URL, AESKey: hex.EncodeToString(key)})
			msg := deliverMediaWire(t, b, quotedWire("123", &item))
			if requests.Load() != 0 {
				t.Fatal("receiving a quote downloaded media")
			}
			if q := msg.QuotedMessage; q.Media == nil || q.Media.Type != ContentType(kind) || q.Resolution != QuoteEmbedded {
				t.Fatalf("quote=%+v", q)
			}
			if (kind == "image" || kind == "video") && msg.QuotedMessage.Media.MIMEType != "" {
				t.Fatalf("undownloaded media guessed MIME: %q", msg.QuotedMessage.Media.MIMEType)
			}
			if current, err := b.Download(t.Context(), msg); current != nil || err != nil {
				t.Fatalf("quote treated as current attachment: %+v %v", current, err)
			}
			got, err := b.DownloadQuoted(t.Context(), msg)
			if err != nil || !bytes.Equal(got.Data, payload) || got.Type != kind || got.MIMEType == "" {
				t.Fatalf("download=%+v %v", got, err)
			}
			if kind == "video" && got.MIMEType != "video/webm" {
				t.Fatalf("video MIME=%q", got.MIMEType)
			}
			if kind == "file" && got.FileName != "original.pdf" {
				t.Fatalf("file name=%q", got.FileName)
			}
			if requests.Load() != 1 {
				t.Fatalf("requests=%d", requests.Load())
			}
			restarted := newQuoteTestBot(t, dir, "account")
			idOnly := deliverMediaWire(t, restarted, quotedWire("123", nil))
			if q := idOnly.QuotedMessage; q.Media == nil || !q.Media.Cached || q.Type != ContentType(kind) || q.Resolution != QuoteCached {
				t.Fatalf("restored quote=%+v", q)
			}
			got, err = restarted.DownloadQuoted(t.Context(), idOnly)
			if err != nil || !bytes.Equal(got.Data, payload) || requests.Load() != 1 {
				t.Fatalf("cached download=%+v %v requests=%d", got, err, requests.Load())
			}
			if kind == "video" && (got.MIMEType != "video/webm" || idOnly.QuotedMessage.Media.MIMEType != "video/webm") {
				t.Fatalf("cached video MIME=%q metadata=%+v", got.MIMEType, idOnly.QuotedMessage.Media)
			}
			for _, scope := range []struct{ account, peer string }{{"other", "peer"}, {"account", "other"}} {
				other := newQuoteTestBot(t, dir, scope.account)
				wire := quotedWire("123", nil)
				wire.FromUserID, wire.MessageType = scope.peer, MessageTypeUser
				raw, _ := json.Marshal(wire)
				cross := deliverQuoteTestWire(t, other, string(raw))
				if _, err := other.DownloadQuoted(t.Context(), cross); !errors.Is(err, ErrQuoteMediaUnavailable) {
					t.Fatalf("cross-scope access: %v", err)
				}
			}
		})
	}
}

func TestCurrentDownloadRetainsOriginalAttachment(t *testing.T) {
	key := []byte("0123456789abcdef")
	ciphertext, _ := crypto.EncryptAESECB([]byte("current document"), key)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(ciphertext) }))
	defer server.Close()
	dir := t.TempDir()
	b := newQuoteTestBot(t, dir, "account")
	item := quoteMediaItem("file", &CDNMedia{FullURL: server.URL, AESKey: hex.EncodeToString(key)})
	msg := deliverMediaWire(t, b, WireMessage{MessageID: "123", ItemList: []MessageItem{item}})
	if _, err := b.Download(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	restarted := newQuoteTestBot(t, dir, "account")
	ref := deliverMediaWire(t, restarted, quotedWire("123", nil))
	got, err := restarted.DownloadQuoted(t.Context(), ref)
	if err != nil || string(got.Data) != "current document" {
		t.Fatalf("retained current attachment=%+v %v", got, err)
	}
	// CDN credentials are never persisted in the quote index.
	raw, _ := os.ReadFile(b.quoteStore.Path())
	if strings.Contains(string(raw), server.URL) || strings.Contains(string(raw), hex.EncodeToString(key)) {
		t.Fatal("quote index contains CDN credentials")
	}
}

func TestUnnamedFileRetryPreservesQuotedAttachment(t *testing.T) {
	dir := t.TempDir()
	b := newQuoteTestBot(t, dir, "account")
	key := []byte("0123456789abcdef")
	payload := []byte("retained document")
	ciphertext, err := crypto.EncryptAESECB(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(ciphertext)
	}))
	defer server.Close()
	wire := &WireMessage{MessageID: "123", FromUserID: "peer", MessageType: MessageTypeUser,
		ItemList: []MessageItem{{Type: ItemFile, FileItem: &FileItem{Media: &CDNMedia{FullURL: server.URL, AESKey: hex.EncodeToString(key)}}}}}
	retry := errors.New("downstream temporarily unavailable")
	err = b.processWireMessage(t.Context(), MessageHandlerFunc(func(ctx context.Context, msg *IncomingMessage) MessageResult {
		got, err := b.Download(ctx, msg)
		if err != nil || !bytes.Equal(got.Data, payload) || got.FileName != "file.bin" {
			t.Fatalf("current download=%+v err=%v", got, err)
		}
		return RetryMessage(retry)
	}), 0, wire)
	if !errors.Is(err, retry) {
		t.Fatalf("first attempt=%v", err)
	}
	if err := b.processWireMessage(t.Context(), MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}), 0, wire); err != nil {
		t.Fatal(err)
	}
	restarted := newQuoteTestBot(t, dir, "account")
	msg := deliverMediaWire(t, restarted, quotedWire("123", nil))
	got, err := restarted.DownloadQuoted(t.Context(), msg)
	if err != nil || !bytes.Equal(got.Data, payload) || got.FileName != "file.bin" || requests.Load() != 1 {
		t.Fatalf("retained download=%+v err=%v requests=%d", got, err, requests.Load())
	}
}

func TestVideoMIMEFromPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"webm", quoteTestWebMHeader, "video/webm"},
		{"mp4", []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), "video/mp4"},
		{"unknown", []byte{0, 0, 0, 0}, "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := downloadedMIME(&DownloadedMedia{Type: "video", Data: tc.data}); got != tc.want {
				t.Fatalf("MIME=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestAcceptedOutboundVideoRetainsDetectedMIME(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ret":0,"message_id":"123"}`))
	}))
	defer server.Close()
	b := newQuoteTestBot(t, t.TempDir(), "account")
	b.creds.BaseURL = server.URL
	item := quoteMediaItem("video", &CDNMedia{EncryptQueryParam: "uploaded"})
	result, err := b.sendMessage(t.Context(), 0, "peer", "context", OutboundMessage{Item: item}, quoteTestWebMHeader)
	if err != nil || result.MessageID != "123" {
		t.Fatalf("send=%+v err=%v", result, err)
	}
	msg := deliverMediaWire(t, b, quotedWire("123", nil))
	got, err := b.DownloadQuoted(t.Context(), msg)
	if err != nil || !bytes.Equal(got.Data, quoteTestWebMHeader) || got.MIMEType != "video/webm" {
		t.Fatalf("retained video=%+v err=%v", got, err)
	}
}

func TestQuoteMediaMissingCancellationAndOldSession(t *testing.T) {
	b := newQuoteTestBot(t, t.TempDir(), "account")
	key := store.QuoteKey{Account: "account", Peer: "peer", Message: "123"}
	if err := b.quoteStore.PutMedia(key, store.QuoteMedia{Type: "file", FileName: "doc.pdf"}, []byte("pdf")); err != nil {
		t.Fatal(err)
	}
	msg := deliverMediaWire(t, b, quotedWire("123", nil))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.DownloadQuoted(ctx, msg); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	media, _, _ := b.quoteStore.GetMedia(key)
	if err := os.Remove(filepath.Join(b.quoteStore.MediaDir(), media.File)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DownloadQuoted(t.Context(), msg); !errors.Is(err, ErrQuoteMediaUnavailable) {
		t.Fatalf("missing payload: %v", err)
	}
	b.mu.Lock()
	b.sessionGeneration++
	b.mu.Unlock()
	if _, err := b.DownloadQuoted(t.Context(), msg); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("old session read: %v", err)
	}
}

func TestQuoteMediaCacheFailureDoesNotFailDownload(t *testing.T) {
	b := newQuoteTestBot(t, t.TempDir(), "account")
	if err := os.WriteFile(b.quoteStore.Path(), []byte("broken cache"), 0600); err != nil {
		t.Fatal(err)
	}
	var reported []error
	b.opts.OnError = func(err error) { reported = append(reported, err) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("image bytes")) }))
	defer server.Close()
	item := MessageItem{Type: ItemImage, ImageItem: &ImageItem{URL: server.URL}}
	wire := quotedWire("123", &item)
	wire.MessageType, wire.FromUserID = MessageTypeUser, "peer"
	msg := b.parseMessage(&wire)
	got, err := b.DownloadQuoted(t.Context(), msg)
	if err != nil || string(got.Data) != "image bytes" || len(reported) != 1 {
		t.Fatalf("download=%+v %v errors=%v", got, err, reported)
	}
	var cacheErr *QuoteCacheError
	if !errors.As(reported[0], &cacheErr) {
		t.Fatal(reported[0])
	}
	raw, _ := os.ReadFile(b.quoteStore.Path())
	if string(raw) != "broken cache" {
		t.Fatal("cache failure overwrote original file")
	}
}

func TestAcceptedOutboundMediaRetainedOnlyWhenHookKeepsPayload(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ret":0,"message_id":"123"}`)) }))
			defer server.Close()
			b := newQuoteTestBot(t, t.TempDir(), "account")
			b.creds.BaseURL = server.URL
			item := quoteMediaItem("file", &CDNMedia{EncryptQueryParam: "uploaded", AESKey: "original"})
			if change {
				b.Hooks().BeforeSend.Register(func(request *SendRequest) error {
					request.Message.Item.FileItem.Media.EncryptQueryParam = "different-payload"
					return nil
				})
			}
			result, err := b.sendMessage(t.Context(), 0, "peer", "context", OutboundMessage{Item: item}, []byte("document"))
			if err != nil || result.MessageID != "123" {
				t.Fatalf("send=%+v %v", result, err)
			}
			data, _, err := b.quoteStore.ReadMedia(store.QuoteKey{Account: "account", Peer: "peer", Message: "123"})
			if change {
				if !errors.Is(err, store.ErrQuoteMediaUnavailable) {
					t.Fatalf("hook changed media but original bytes retained: %q %v", data, err)
				}
			} else if err != nil || string(data) != "document" {
				t.Fatalf("outbound payload=%q %v", data, err)
			}
		})
	}
}

func TestQuoteMediaDirectoryCannotContainOtherState(t *testing.T) {
	quotePath := filepath.Join(t.TempDir(), "quotes.json")
	b := New(Options{CredPath: filepath.Join(quotePath+".media", "credentials.json"), QuoteCache: QuoteCacheOptions{Path: quotePath}})
	if err := b.PruneQuoteCache(); err == nil || !strings.Contains(err.Error(), "outside quote media") {
		t.Fatalf("state path collision: %v", err)
	}
	if _, err := os.Stat(quotePath + ".media"); !os.IsNotExist(err) {
		t.Fatalf("collision created media directory: %v", err)
	}
}

func TestAcceptedOutboundMediaCacheFailureRemainsSuccessful(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ret":0,"message_id":"123"}`)) }))
	defer server.Close()
	b := newQuoteTestBot(t, t.TempDir(), "account")
	b.creds.BaseURL = server.URL
	if err := os.WriteFile(b.quoteStore.Path(), []byte("broken cache"), 0600); err != nil {
		t.Fatal(err)
	}
	var reported []error
	b.opts.OnError = func(err error) { reported = append(reported, err) }
	var sentErr error
	var sentID MessageID
	b.Hooks().AfterSend.Register(func(outcome SendOutcome) error { sentErr, sentID = outcome.Err, outcome.Result.MessageID; return nil })
	item := quoteMediaItem("file", &CDNMedia{EncryptQueryParam: "uploaded"})
	result, err := b.sendMessage(t.Context(), 0, "peer", "context", OutboundMessage{Item: item}, []byte("document"))
	if err != nil || result.MessageID != "123" || sentErr != nil || sentID != "123" || len(reported) != 1 {
		t.Fatalf("accepted send=%+v %v hook=%s %v errors=%v", result, err, sentID, sentErr, reported)
	}
	if _, err := os.Stat(b.quoteStore.MediaDir()); !os.IsNotExist(err) {
		t.Fatalf("corrupt cache created media files: %v", err)
	}
}

func TestQuoteMediaDirectoryAliasCannotContainOtherState(t *testing.T) {
	dir := t.TempDir()
	quotePath := filepath.Join(dir, "quotes.json")
	if err := os.Mkdir(quotePath+".media", 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(quotePath+".media", alias); err != nil {
		t.Skipf("directory links unavailable: %v", err)
	}
	b := New(Options{CredPath: filepath.Join(alias, "credentials.json"), QuoteCache: QuoteCacheOptions{Path: quotePath}})
	if err := b.PruneQuoteCache(); err == nil || !strings.Contains(err.Error(), "outside quote media") {
		t.Fatalf("linked state path collision: %v", err)
	}
}
