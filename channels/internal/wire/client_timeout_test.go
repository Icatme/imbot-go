package wire

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

func TestClientTimeoutRetainsContextCause(t *testing.T) {
	for _, helper := range []string{"json", "media"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, stage := range []string{"headers", "body"} {
				t.Run(helper+"/"+method+"/"+stage, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if stage == "body" {
							w.WriteHeader(http.StatusOK)
							w.(http.Flusher).Flush()
						}
						<-r.Context().Done()
					}))
					defer server.Close()
					client := server.Client()
					client.Timeout = 50 * time.Millisecond
					ctx := context.Background() // Client.Timeout owns the deadline, not ctx.
					var err error
					if helper == "json" {
						err = JSON(ctx, client, "test", method, server.URL+"/?token=private-token", nil, nil, new(any))
					} else {
						_, _, err = MediaHTTP(ctx, client, "test", method, server.URL+"/?token=private-token", nil, nil, 1024)
					}
					var api *gateway.APIError
					if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &api) || api.Uncertain != (method == http.MethodPost) {
						t.Fatalf("client timeout lost cause or uncertainty: %v", err)
					}
					if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), server.URL) {
						t.Fatalf("timeout exposed request location: %v", err)
					}
				})
			}
		}
	}
}
