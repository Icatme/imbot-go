package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

func TestUploadedReferenceCannotChangeRouteKindOrLifetime(t *testing.T) {
	for _, mutation := range []string{"route", "kind", "expiry"} {
		t.Run(mutation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := fixture(t)
				sends := 0
				a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
					switch {
					case r.URL.Path == "/app/getAppAccessToken":
						return response(`{"access_token":"tok","expires_in":7200}`), nil
					case strings.HasSuffix(r.URL.Path, "/files"):
						return response(`{"file_info":"PRIVATE_FILE_INFO","ttl":1}`), nil
					case strings.HasSuffix(r.URL.Path, "/messages"):
						sends++
						return response(`{"id":"accepted"}`), nil
					default:
						t.Fatalf("unexpected request %s", r.URL.Path)
						return nil, nil
					}
				})}
				r := route(a)
				p, err := a.UploadURL(context.Background(), r, URLUpload{Kind: "file", URL: "https://example.com/file", ReplyTo: "m"})
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "route":
					r.ChatID = "other"
					p.Scope = &r
				case "kind":
					p.Kind = "image"
				case "expiry":
					time.Sleep(2 * time.Second)
					p.ExpiresAt = time.Time{}
				}
				_, err = a.Send(context.Background(), r, gateway.Message{ReplyTo: "m", Attachments: []gateway.Attachment{p}})
				if err == nil || sends != 0 {
					t.Fatalf("modified %s bypassed upload binding: err=%v, sends=%d", mutation, err, sends)
				}
			})
		})
	}
}

func TestInboundReferenceCannotBecomeOutboundByClearingMetadata(t *testing.T) {
	a := fixture(t)
	e, err := a.decode(envelope{Type: "C2C_MESSAGE_CREATE", Data: json.RawMessage(`{"id":"m","author":{"user_openid":"user"},"attachments":[{"content_type":"image/png","url":"https://gchat.qpic.cn/image"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if strings.Contains(r.URL.Path, "getAppAccessToken") {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		return response(`{"id":"accepted"}`), nil
	})}
	p := e.Attachments[0]
	p.MessageID = ""
	_, err = a.Send(context.Background(), e.Route, gateway.Message{ReplyTo: "m", Attachments: []gateway.Attachment{p}})
	if !errors.Is(err, gateway.ErrUnsupported) || calls != 0 {
		t.Fatalf("inbound locator treated as file_info: %v, calls=%d", err, calls)
	}
}
