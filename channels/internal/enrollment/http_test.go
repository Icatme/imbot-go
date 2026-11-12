package enrollment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/channels/onboarding"
	"github.com/Icatme/imbot-go/gateway"
)

func TestGETCredentialsStayInRequestAndRedirectsAreRejected(t *testing.T) {
	var redirected atomic.Bool
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer dest.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodGet || len(body) != 0 || r.Header.Get("Content-Type") != "" || r.URL.Query().Get("scode") != "private & code" {
			t.Error("invalid query registration request")
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, dest.URL, http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"status":"success"}`)
	}))
	defer srv.Close()
	query := "?" + url.Values{"scode": {"private & code"}}.Encode()
	var out struct{ Status string }
	if err := GetJSON(context.Background(), nil, "wecom", srv.URL+query, &out); err != nil || out.Status != "success" {
		t.Fatal(out, err)
	}
	err := GetJSON(context.Background(), nil, "wecom", srv.URL+"/redirect"+query, &out)
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Status != http.StatusFound || redirected.Load() || strings.Contains(err.Error(), "private") {
		t.Fatal("redirected or leaked query credential", err)
	}
	// Transport errors include URLs in net/http; the helper must redact them.
	srv.Close()
	err = GetJSON(context.Background(), nil, "wecom", srv.URL+query, &out)
	if !errors.As(err, &api) || api.Code != "registration_transport" || strings.Contains(err.Error(), "private") {
		t.Fatal("query credential leaked through transport error", err)
	}
}

func TestFormPendingAndNoRedirect(t *testing.T) {
	var hit atomic.Bool
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer dest.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, dest.URL, 307)
			return
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.FormValue("device_code") != "secret & code" {
			t.Error("incorrect form encoding")
		}
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"authorization_pending"}`)
	}))
	defer srv.Close()
	var out struct{ Error string }
	status, err := PostForm(context.Background(), nil, "test", srv.URL, url.Values{"device_code": {"secret & code"}}, &out)
	if err != nil || status != 400 || out.Error != "authorization_pending" {
		t.Fatal(status, out, err)
	}
	err = PostJSON(context.Background(), nil, "test", srv.URL+"/redirect?secret=value", map[string]string{"device_code": "credential"}, &out)
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Status != 307 || hit.Load() || strings.Contains(err.Error(), "secret") {
		t.Fatal("redirected or leaked registration", err)
	}
}

func TestResponseErrorsAreBoundedAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"invalid_json", "secret-credential", 200},
		{"oversized", strings.Repeat("x", wire.MaxBody+1), 200},
		{"server_error", `{"secret":"credential"}`, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer s.Close()
			var out any
			err := PostJSON(context.Background(), nil, "test", s.URL, nil, &out)
			if err == nil || strings.Contains(err.Error(), "credential") {
				t.Fatal(err)
			}
			if tc.name == "invalid_json" && !errors.Is(err, onboarding.ErrInvalidResponse) {
				t.Fatal(err)
			}
		})
	}
}

func TestCancellationPreservesContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer s.Close()
	defer close(release)
	done := make(chan error, 1)
	go func() { var out any; done <- PostJSON(ctx, nil, "test", s.URL, nil, &out) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
