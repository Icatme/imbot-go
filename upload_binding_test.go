package wechatbot

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func newBoundUploadTestBot(t *testing.T) (*Bot, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	b, _ := newAuthenticatedSessionTestBot(t, "https://old.example")
	uploads, sends := new(atomic.Int32), new(atomic.Int32)
	b.client.HTTP = reauthenticationTestClient(rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"ret":0}`
		header := make(http.Header)
		switch req.URL.Path {
		case "/ilink/bot/getuploadurl":
			uploads.Add(1)
			body = `{"ret":0,"upload_full_url":"https://cdn.example/upload"}`
		case "/upload":
			header.Set("x-encrypted-param", "uploaded-resource")
		case "/ilink/bot/sendmessage":
			sends.Add(1)
		default:
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}), "fresh-token", "bot-1", "https://fresh.example")
	return b, uploads, sends
}

func nativeUploadMessage(t *testing.T, b *Bot) OutboundMessage {
	t.Helper()
	u, err := b.Upload(context.Background(), []byte("document"), "user-1", int(MediaFile))
	if err != nil {
		t.Fatal(err)
	}
	media := u.Media
	return OutboundMessage{Upload: u, Item: MessageItem{Type: ItemFile, FileItem: &FileItem{Media: &media, FileName: "document.txt", Len: "8"}}}
}

func nativeBoundInbound(b *Bot) *IncomingMessage {
	return &IncomingMessage{UserID: "user-1", ContextToken: "context-1", sessionBot: b, sessionGeneration: b.sessionGeneration}
}

func TestUploadForReplyRejectsOldNativeGenerationBeforeIO(t *testing.T) {
	b, uploads, _ := newBoundUploadTestBot(t)
	inbound := nativeBoundInbound(b)
	if _, err := b.UploadForReply(context.Background(), inbound, []byte("document"), int(MediaFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reauthenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.UploadForReply(context.Background(), inbound, []byte("document"), int(MediaFile)); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("stale reply upload: %v", err)
	}
	if uploads.Load() != 1 {
		t.Fatalf("stale reply used new credentials: %d upload requests", uploads.Load())
	}
}

func TestUploadForReplyRechecksNativeGenerationAfterPreparation(t *testing.T) {
	b, uploads, _ := newBoundUploadTestBot(t)
	inbound := nativeBoundInbound(b)
	original := cryptorand.Reader
	started, release := make(chan struct{}), make(chan struct{})
	cryptorand.Reader = &blockingRandomReader{delegate: original, started: started, release: release}
	defer func() { cryptorand.Reader = original }()
	done := make(chan error, 1)
	go func() {
		_, err := b.UploadForReply(context.Background(), inbound, []byte("document"), int(MediaFile))
		done <- err
	}()
	waitForSignal(t, started, "reply upload preparation")
	if _, err := b.Reauthenticate(context.Background()); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("reauth during preparation: %v", err)
	}
	if uploads.Load() != 0 {
		t.Fatal("old reply upload started IO after reauthentication")
	}
}

func TestBoundUploadRejectsCrossSessionAndReauthenticationDuringHook(t *testing.T) {
	for _, duringHook := range []bool{false, true} {
		t.Run(fmt.Sprint(duringHook), func(t *testing.T) {
			b, _, sends := newBoundUploadTestBot(t)
			b.opts.NotifyErrors = true
			msg := nativeUploadMessage(t, b)
			if duringHook {
				b.Hooks().BeforeSend.Register(func(*SendRequest) error {
					_, err := b.Reauthenticate(context.Background())
					return err
				})
			} else {
				if _, err := b.Reauthenticate(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := b.contextTokens.Set("user-1", "fresh-context"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.SendMessage(context.Background(), "user-1", msg); !errors.Is(err, ErrSessionChanged) {
				t.Fatalf("bound send after reauth: %v", err)
			}
			if sends.Load() != 0 {
				t.Fatal("stale upload reached sendmessage")
			}
		})
	}
}

func TestBoundUploadRejectsRetargetingAndLostBinding(t *testing.T) {
	for _, mutation := range []string{"user", "bot", "json", "resource", "hook_remove", "hook_replace", "hook_resource"} {
		t.Run(mutation, func(t *testing.T) {
			b, _, sends := newBoundUploadTestBot(t)
			msg := nativeUploadMessage(t, b)
			user := "user-1"
			switch mutation {
			case "user":
				user = "user-2"
				if err := b.contextTokens.Set(user, "other-context"); err != nil {
					t.Fatal(err)
				}
			case "bot":
				b, _, sends = newBoundUploadTestBot(t)
			case "json":
				data, err := json.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				msg = OutboundMessage{}
				if err = json.Unmarshal(data, &msg); err != nil {
					t.Fatal(err)
				}
			case "resource":
				msg.Item.FileItem.Media.EncryptQueryParam = "other"
			case "hook_remove":
				b.Hooks().BeforeSend.Register(func(r *SendRequest) error { r.Message.Upload = nil; return nil })
			case "hook_replace":
				b.Hooks().BeforeSend.Register(func(r *SendRequest) error { copy := *r.Message.Upload; r.Message.Upload = &copy; return nil })
			case "hook_resource":
				b.Hooks().BeforeSend.Register(func(r *SendRequest) error { r.Message.Item.FileItem.Media.EncryptQueryParam = "other"; return nil })
			}
			if _, err := b.SendMessage(context.Background(), user, msg); err == nil {
				t.Fatal("accepted invalid upload binding")
			}
			if sends.Load() != 0 {
				t.Fatal("invalid binding reached sendmessage")
			}
		})
	}
}

func TestBoundUploadSendsThroughNativeReplyAndProactivePaths(t *testing.T) {
	b, _, sends := newBoundUploadTestBot(t)
	msg := nativeUploadMessage(t, b)
	if _, err := b.SendMessage(context.Background(), "user-1", msg); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReplyMessage(context.Background(), nativeBoundInbound(b), msg); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 2 {
		t.Fatal(sends.Load())
	}
}

func TestBoundUploadFailedHookDoesNotPoisonCallerRetry(t *testing.T) {
	b, _, sends := newBoundUploadTestBot(t)
	msg := nativeUploadMessage(t, b)
	blocked := errors.New("hook blocked attempt")
	calls := 0
	b.Hooks().BeforeSend.Register(func(r *SendRequest) error {
		calls++
		if calls == 1 {
			r.Message.Item.FileItem.Media.EncryptQueryParam = "hook-mutated"
			*r.Message.Upload = UploadResult{}
			return blocked
		}
		return nil
	})
	if _, err := b.SendMessage(context.Background(), "user-1", msg); !errors.Is(err, blocked) {
		t.Fatal(err)
	}
	if _, err := b.SendMessage(context.Background(), "user-1", msg); err != nil {
		t.Fatalf("failed hook poisoned caller retry: %v", err)
	}
	if sends.Load() != 1 {
		t.Fatal(sends.Load())
	}
}

func TestBoundUploadHookCannotReplacePrivateBindingThroughValueAssignment(t *testing.T) {
	b, _, sends := newBoundUploadTestBot(t)
	msg := nativeUploadMessage(t, b)
	replacement := nativeUploadMessage(t, b)
	b.Hooks().BeforeSend.Register(func(r *SendRequest) error {
		*r.Message.Upload = *replacement.Upload
		r.Message.Item = replacement.Item
		return nil
	})
	if _, err := b.SendMessage(context.Background(), "user-1", msg); err == nil || sends.Load() != 0 {
		t.Fatalf("hook replaced the upload binding without replacing its wrapper: err=%v sends=%d", err, sends.Load())
	}
}

func TestCanceledNativeMediaSendDoesNotRunHooksOrPersistReplyToken(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(fmt.Sprint(reply), func(t *testing.T) {
			b, _, sends := newBoundUploadTestBot(t)
			msg := nativeUploadMessage(t, b)
			hooks := 0
			b.Hooks().BeforeSend.Register(func(*SendRequest) error { hooks++; return nil })
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var err error
			if reply {
				inbound := nativeBoundInbound(b)
				inbound.ContextToken = "canceled-context"
				_, err = b.ReplyMessage(ctx, inbound, msg)
			} else {
				_, err = b.SendMessage(ctx, "user-1", msg)
			}
			if !errors.Is(err, context.Canceled) || hooks != 0 || sends.Load() != 0 {
				t.Fatalf("canceled send had effects: err=%v hooks=%d sends=%d", err, hooks, sends.Load())
			}
			if got := b.contextTokens.Get("user-1"); got != "context-1" {
				t.Fatalf("canceled reply persisted context token: %q", got)
			}
		})
	}
}

func TestCanceledNativeUploadDoesNotStartPreparationOrIO(t *testing.T) {
	b, uploads, _ := newBoundUploadTestBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := b.Upload(ctx, []byte("document"), "user-1", int(MediaFile))
	if !errors.Is(err, context.Canceled) || result != nil || uploads.Load() != 0 {
		t.Fatalf("canceled upload succeeded: result=%+v err=%v requests=%d", result, err, uploads.Load())
	}
}

func TestNativeUploadCanceledDuringPreparationDoesNotStartIO(t *testing.T) {
	b, uploads, _ := newBoundUploadTestBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original := cryptorand.Reader
	started, release := make(chan struct{}), make(chan struct{})
	cryptorand.Reader = &blockingRandomReader{delegate: original, started: started, release: release}
	defer func() { cryptorand.Reader = original }()
	done := make(chan error, 1)
	go func() {
		_, err := b.Upload(ctx, []byte("document"), "user-1", int(MediaFile))
		done <- err
	}()
	waitForSignal(t, started, "upload preparation")
	cancel()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) || uploads.Load() != 0 {
		t.Fatalf("canceled preparation reached upload request: err=%v requests=%d", err, uploads.Load())
	}
}

func TestNativeMediaKnownSuccessSurvivesConcurrentCancellation(t *testing.T) {
	for _, operation := range []string{"upload", "send"} {
		t.Run(operation, func(t *testing.T) {
			b, _, _ := newBoundUploadTestBot(t)
			msg := nativeUploadMessage(t, b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := b.client.HTTP.Transport
			b.client.HTTP.Transport = rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(req)
				if (operation == "upload" && req.URL.Path == "/upload") || (operation == "send" && req.URL.Path == "/ilink/bot/sendmessage") {
					cancel() // the complete, valid success response is already available
				}
				return response, err
			})
			if operation == "upload" {
				result, err := b.Upload(ctx, []byte("document"), "user-1", int(MediaFile))
				if err != nil || result == nil {
					t.Fatalf("known upload success became an error: %v", err)
				}
			} else if _, err := b.SendMessage(ctx, "user-1", msg); err != nil {
				t.Fatalf("known send success became an error: %v", err)
			}
		})
	}
}
