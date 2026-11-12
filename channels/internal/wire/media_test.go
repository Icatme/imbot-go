package wire

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Icatme/wechatbot-go/gateway"
)

type mediaRoundTrip func(*http.Request) (*http.Response, error)

func (f mediaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMediaHTTPBoundsAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		length int64
	}{
		{"declared", 200, "large", 10}, {"chunked", 200, "large", -1}, {"failure", 500, "secret-response", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: tc.length}, nil
			})}
			_, _, err := MediaHTTP(context.Background(), c, "test", "GET", "https://cdn.invalid/?token=secret", nil, nil, 4)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
			if tc.status == 200 && !errors.Is(err, gateway.ErrMediaTooLarge) {
				t.Fatal(err)
			}
		})
	}
}

func TestMediaWriteResponseLimitPreservesUncertainty(t *testing.T) {
	for _, length := range []int64{10, -1} {
		c := &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("large")), ContentLength: length}, nil
		})}
		_, _, err := MediaHTTP(context.Background(), c, "test", "POST", "https://upload.invalid", nil, strings.NewReader("payload"), 4)
		var api *gateway.APIError
		if !errors.Is(err, gateway.ErrMediaTooLarge) || !errors.As(err, &api) || !api.Uncertain {
			t.Fatal(err)
		}
	}
}

func TestMediaRedirectNeverForwardsCredentials(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Redirect(w, r, "/target", 302) }))
	defer s.Close()
	_, _, err := MediaHTTP(context.Background(), nil, "test", "GET", s.URL, http.Header{"Authorization": []string{"private"}}, nil, 100)
	if err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestDownloadMediaRejectsPrivateDestinations(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer s.Close()
	if _, _, err := DownloadMedia(context.Background(), nil, "test", s.URL, nil, 100); err == nil {
		t.Fatal("private destination allowed")
	}
	if calls != 0 {
		t.Fatal("private request reached server")
	}
}

func TestMediaCancellationDoesNotStartIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &http.Client{Transport: mediaRoundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("performed IO"); return nil, nil })}
	if _, _, err := DownloadMedia(ctx, c, "test", "https://cdn.invalid", nil, 100); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMediaErrorBodyIsOptInBoundedAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		optIn    bool
		body     string
		wantBody bool
	}{
		{"default", false, `{"code":99991668,"msg":"private detail"}`, false},
		{"platform-parser", true, `{"code":99991668,"msg":"private detail"}`, true},
		{"oversized", true, strings.Repeat("secret", 12000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: mediaRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			request := MediaHTTP
			if tc.optIn {
				request = MediaHTTPWithErrorBody
			}
			data, _, err := request(context.Background(), client, "test", "POST", "https://api.invalid", nil, nil, 100)
			var api *gateway.APIError
			if !errors.As(err, &api) || api.Status != 400 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
				t.Fatal(err)
			}
			if tc.wantBody && string(data) != tc.body || !tc.wantBody && data != nil {
				t.Fatalf("unexpected error-body exposure: %d bytes", len(data))
			}
		})
	}
}

func TestMediaErrorBodyCancellationRetainsHTTPStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: mediaRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: jsonCancelBody{cancel}}, nil
	})}
	data, _, err := MediaHTTPWithErrorBody(ctx, client, "test", "POST", "https://api.invalid", nil, nil, 100)
	var api *gateway.APIError
	if data != nil || !errors.Is(err, context.Canceled) || !errors.As(err, &api) || api.Status != 401 || !api.Uncertain {
		t.Fatal(err)
	}
}
