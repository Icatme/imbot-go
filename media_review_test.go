package wechatbot

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestFailedBeforeSendDoesNotMutateUnboundMessage(t *testing.T) {
	b, _, sends := newBoundUploadTestBot(t)
	msg := OutboundMessage{Item: MessageItem{
		Type: ItemText, TextItem: &TextItem{Text: "original"},
		RefMsg: &RefMessage{Title: "original quote", MessageItem: &MessageItem{
			Type: ItemFile, FileItem: &FileItem{FileName: "original.txt", Media: &CDNMedia{EncryptQueryParam: "original-resource"}},
		}},
	}}
	blocked := errors.New("attempt rejected")
	first := true
	b.Hooks().BeforeSend.Register(func(req *SendRequest) error {
		if first {
			first = false
			req.Message.Item.TextItem.Text = "rejected edit"
			req.Message.Item.RefMsg.Title = "rejected quote"
			req.Message.Item.RefMsg.MessageItem.FileItem.Media.EncryptQueryParam = "rejected-resource"
			return blocked
		}
		if req.Message.Item.TextItem.Text != "original" || req.Message.Item.RefMsg.Title != "original quote" || req.Message.Item.RefMsg.MessageItem.FileItem.Media.EncryptQueryParam != "original-resource" {
			t.Error("failed attempt poisoned the caller's reusable item graph")
		}
		return nil
	})
	if _, err := b.SendMessage(context.Background(), "user-1", msg); !errors.Is(err, blocked) || sends.Load() != 0 {
		t.Fatalf("failed hook: err=%v sends=%d", err, sends.Load())
	}
	if _, err := b.SendMessage(context.Background(), "user-1", msg); err != nil || sends.Load() != 1 {
		t.Fatalf("explicit next attempt: err=%v sends=%d", err, sends.Load())
	}
}

func TestNativeHandlerContextCannotCrossBotWithMatchingGeneration(t *testing.T) {
	for _, operation := range []string{"send", "upload", "start_typing", "stop_typing"} {
		t.Run(operation, func(t *testing.T) {
			origin, _, _ := newBoundUploadTestBot(t)
			other, _, _ := newBoundUploadTestBot(t)
			var calls atomic.Int32
			transport := other.client.HTTP.Transport
			other.client.HTTP.Transport = rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return transport.RoundTrip(req)
			})
			err := origin.processWireMessage(context.Background(), MessageHandlerFunc(func(ctx context.Context, _ *IncomingMessage) MessageResult {
				var err error
				switch operation {
				case "send":
					_, err = other.SendMessage(ctx, "user-1", OutboundMessage{Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "reply"}}})
				case "upload":
					_, err = other.Upload(ctx, []byte("document"), "user-1", int(MediaFile))
				case "start_typing":
					err = other.SendTyping(ctx, "user-1")
				case "stop_typing":
					err = other.StopTyping(ctx, "user-1")
				}
				if !errors.Is(err, ErrSessionChanged) || calls.Load() != 0 {
					t.Errorf("foreign native handler context accepted: err=%v calls=%d", err, calls.Load())
				}
				return AckMessage()
			}), origin.sessionGeneration, &WireMessage{
				MessageID: "source-message", MessageType: MessageTypeUser,
				FromUserID: "user-1", ContextToken: "source-context",
				ItemList: []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "hello"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeInboundCannotCrossBotWithMatchingGeneration(t *testing.T) {
	for _, operation := range []string{"reply", "upload"} {
		t.Run(operation, func(t *testing.T) {
			origin, _, _ := newBoundUploadTestBot(t)
			other, uploads, sends := newBoundUploadTestBot(t)
			var inbound *IncomingMessage
			err := origin.processWireMessage(context.Background(), MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
				inbound = msg
				return AckMessage()
			}), origin.sessionGeneration, &WireMessage{
				MessageID: "source-message", MessageType: MessageTypeUser,
				FromUserID: "user-1", ContextToken: "source-context",
				ItemList: []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "hello"}}},
			})
			if err != nil || inbound == nil {
				t.Fatalf("native delivery: %v", err)
			}
			if operation == "reply" {
				_, err = other.ReplyMessage(context.Background(), inbound, OutboundMessage{Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "reply"}}})
			} else {
				_, err = other.UploadForReply(context.Background(), inbound, []byte("document"), int(MediaFile))
			}
			if !errors.Is(err, ErrSessionChanged) || uploads.Load() != 0 || sends.Load() != 0 {
				t.Fatalf("foreign native message accepted: err=%v uploads=%d sends=%d", err, uploads.Load(), sends.Load())
			}
			if token := other.contextTokens.Get("user-1"); token != "context-1" {
				t.Fatalf("foreign context persisted in destination Bot: %q", token)
			}
		})
	}
}
