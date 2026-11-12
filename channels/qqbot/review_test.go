package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

func TestClientTimeoutPreservesQQOperationCause(t *testing.T) {
	for _, stage := range []string{"headers", "body"} {
		t.Run(stage, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/token" {
					_, _ = io.WriteString(w, `{"access_token":"token","expires_in":7200}`)
					return
				}
				if stage == "body" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			a := fixture(t)
			a.cfg.APIBase, a.cfg.TokenURL = server.URL, server.URL+"/token"
			a.cfg.HTTPClient = server.Client()
			a.cfg.HTTPClient.Timeout = 50 * time.Millisecond
			_, err := a.Send(context.Background(), route(a), gateway.Message{Text: "hello", ReplyTo: "m"})
			var api *gateway.APIError
			if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &api) || !api.Uncertain {
				t.Fatalf("QQ client timeout lost cause or uncertainty: %v", err)
			}
		})
	}
}

func TestInteractionACKRejectsErrorAndMalformedSuccessBodies(t *testing.T) {
	for _, tc := range []struct {
		body, code string
		uncertain  bool
	}{
		{`{"code":40034005,"message":"private detail"}`, "40034005", false},
		{`{"err_code":40034005}`, "40034005", false},
		{`<html>private failure</html>`, "invalid_response", true},
		{`null`, "invalid_response", true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			a := fixture(t)
			var v envelope
			if err := json.Unmarshal(interactionPayload(t), &v); err != nil {
				t.Fatal(err)
			}
			e, err := a.decode(v)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/app/getAppAccessToken" {
					return response(`{"access_token":"tok","expires_in":7200}`), nil
				}
				calls++
				return response(tc.body), nil
			})}
			err = a.AcknowledgeInteraction(context.Background(), e.Route, e.Interaction.ID, 0)
			var api *gateway.APIError
			if !errors.As(err, &api) || api.Code != tc.code || api.Uncertain != tc.uncertain || strings.Contains(err.Error(), "private") || calls != 1 {
				t.Fatalf("error response acknowledged as success or hidden: %v calls=%d", err, calls)
			}
		})
	}
}

type canceledResponseBody struct{ cancel context.CancelFunc }

func (b canceledResponseBody) Read([]byte) (int, error) { b.cancel(); return 0, context.Canceled }
func (canceledResponseBody) Close() error               { return nil }

func TestUploadCancellationDuringResponseReadRetainsCause(t *testing.T) {
	a := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		calls++
		out := response("")
		out.Body = canceledResponseBody{cancel}
		return out, nil
	})}
	_, err := a.Upload(ctx, route(a), gateway.MediaUpload{Kind: "file", ReplyTo: "m", Data: []byte("data")})
	var api *gateway.APIError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &api) || !api.Uncertain || calls != 1 {
		t.Fatalf("lost cancellation or acceptance uncertainty: %v calls=%d", err, calls)
	}
}

func TestDownloadRejectsChangedMediaKindBeforeIO(t *testing.T) {
	a := fixture(t)
	e, err := a.decode(envelope{Type: "C2C_MESSAGE_CREATE", Data: json.RawMessage(`{"id":"m","author":{"user_openid":"user"},"attachments":[{"content_type":"image/png","url":"https://gchat.qpic.cn/image"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("image bytes"))}, nil
	})}
	p := e.Attachments[0]
	p.Kind = "file"
	if _, err := a.Download(context.Background(), e.Route, p); !errors.Is(err, gateway.ErrInvalidEvent) || calls != 0 {
		t.Fatalf("modified descriptor fetched media: %v calls=%d", err, calls)
	}
}
