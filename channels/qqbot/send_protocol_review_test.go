package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

// The official C2C/group send contract requires a nonempty content field for
// msg_type=7, even for a media-only message:
// https://github.com/tencent-connect/bot-docs/blob/main/docs/develop/api-v2/server-inter/message/send-receive/send.md
func TestMediaOnlySendSuppliesRequiredContent(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = io.WriteString(w, `{"access_token":"token","expires_in":7200}`)
					return
				}
				var body struct {
					Content string `json:"content"`
					Type    int    `json:"msg_type"`
					Media   struct {
						FileInfo string `json:"file_info"`
					} `json:"media"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Type != 7 || body.Media.FileInfo != "private-info" || body.Content == "" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"code":400010,"message":"content required"}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"accepted"}`)
			}))
			defer server.Close()
			a := fixture(t)
			a.cfg.APIBase, a.cfg.TokenURL = server.URL, server.URL+"/token"
			r := route(a)
			r.Kind = kind
			p, err := a.rememberUpload(r, "file", "report.pdf", "application/pdf", 3, uploadResult{FileInfo: "private-info", TTL: 300})
			if err != nil {
				t.Fatal(err)
			}
			out, err := a.Send(context.Background(), r, gateway.Message{ReplyTo: "source", Attachments: []gateway.Attachment{p}})
			if err != nil || !out.Accepted {
				t.Fatalf("media-only send violates platform content contract: %+v %v", out, err)
			}
		})
	}
}

func TestQQBatchClientTimeoutPreservesAcceptedPrefix(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"access_token":"token","expires_in":7200}`)
			return
		}
		if sends.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"id":"first"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	a := fixture(t)
	a.cfg.APIBase, a.cfg.TokenURL = server.URL, server.URL+"/token"
	a.cfg.HTTPClient = server.Client()
	a.cfg.HTTPClient.Timeout = 50 * time.Millisecond
	g, err := gateway.New(gateway.Options{Handler: func(context.Context, gateway.Event) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Register(a); err != nil {
		t.Fatal(err)
	}
	var messages []gateway.Message
	for i := 1; i <= 3; i++ {
		messages = append(messages, gateway.Message{Text: fmt.Sprint(i), ReplyTo: "source", Sequence: i})
	}
	out, err := g.SendBatch(context.Background(), route(a), messages)
	var partial *gateway.PartialSendError
	var api *gateway.APIError
	if !errors.As(err, &partial) || partial.Completed != 1 || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &api) || !api.Uncertain || len(out.Receipts) != 1 || out.Receipts[0].MessageID != "first" || sends.Load() != 2 {
		t.Fatalf("batch lost accepted prefix or timeout cause: %+v %v sends=%d", out, err, sends.Load())
	}
}
