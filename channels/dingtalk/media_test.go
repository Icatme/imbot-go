package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

type mediaRoundTrip func(*http.Request) (*http.Response, error)

func (f mediaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloadResolvesCodeAndTransfersBytes(t *testing.T) {
	a := testAdapter(t)
	resolved, downloaded := 0, 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"accessToken":"private-token","expireIn":7200}`
		headers := make(http.Header)
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
		case "/v1.0/robot/messageFiles/download":
			resolved++
			var req map[string]string
			if json.NewDecoder(r.Body).Decode(&req) != nil || req["robotCode"] != "robot" || req["downloadCode"] != "secret-file" || r.Header.Get("x-acs-dingtalk-access-token") != "private-token" {
				t.Error(req, r.Header)
			}
			body = `{"downloadUrl":"https://cdn.example.test/private?signature=secret"}`
		case "/private":
			downloaded++
			if r.Header.Get("Authorization") != "" || r.Header.Get("x-acs-dingtalk-access-token") != "" {
				t.Error("credential forwarded to CDN")
			}
			body = "actual file bytes"
			headers.Set("Content-Type", "application/pdf")
		default:
			t.Error(r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	e, err := a.Decode(richPayload("richText"), "e")
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Download(context.Background(), e.Route, e.Attachments[1])
	if err != nil || string(got.Data) != "actual file bytes" || got.Name != "说明.pdf" || got.MIME != "application/pdf" {
		t.Fatal(got, err)
	}
	other := e.Route
	other.ChatID = "other"
	if _, err = a.Download(context.Background(), other, e.Attachments[1]); !errors.Is(err, gateway.ErrInvalidEvent) {
		t.Fatal(err)
	}
	unknown := e.Attachments[1]
	unknown.Ref = "unknown"
	if _, err = a.Download(context.Background(), e.Route, unknown); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
	expired := e.Attachments[1]
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if _, err = a.Download(context.Background(), e.Route, expired); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = a.Download(ctx, e.Route, e.Attachments[1]); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if resolved != 1 || downloaded != 1 {
		t.Fatal(resolved, downloaded)
	}
}

func TestDownloadDoesNotFollowRedirectOrExposeLocator(t *testing.T) {
	a := testAdapter(t)
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"accessToken":"token","expireIn":7200}`
		status := 200
		headers := make(http.Header)
		if strings.HasSuffix(r.URL.Path, "/download") {
			body = `{"downloadUrl":"https://cdn.example.test/file?signature=secret"}`
		}
		if r.URL.Host == "cdn.example.test" {
			calls++
			status = 302
			headers.Set("Location", "https://other.test/leak")
			body = "secret body"
		}
		if r.URL.Host == "other.test" {
			t.Fatal("followed media redirect")
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	e, err := a.Decode(richPayload("richText"), "e")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Download(context.Background(), e.Route, e.Attachments[0])
	if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
		t.Fatal(err, calls)
	}
}
