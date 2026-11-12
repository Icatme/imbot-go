package wire

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Icatme/wechatbot-go/gateway"
)

type jsonCancelBody struct{ cancel context.CancelFunc }

func (b jsonCancelBody) Read([]byte) (int, error) { b.cancel(); return 0, context.Canceled }
func (jsonCancelBody) Close() error               { return nil }

func TestJSONCancellationPreservesCauseAndUncertainty(t *testing.T) {
	for _, stage := range []string{"before", "transport", "body"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before" {
				cancel()
			}
			calls := 0
			client := &http.Client{Transport: mediaRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				if stage == "body" {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: jsonCancelBody{cancel}}, nil
				}
				cancel()
				return nil, context.Canceled
			})}
			err := JSON(ctx, client, "test", http.MethodPost, "https://api.invalid/send", nil, map[string]string{"text": "hello"}, new(any))
			var api *gateway.APIError
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
			if stage == "before" {
				if calls != 0 || errors.As(err, &api) {
					t.Fatalf("pre-canceled call performed IO or became uncertain: calls=%d err=%v", calls, err)
				}
			} else if calls != 1 || !errors.As(err, &api) || !api.Uncertain {
				t.Fatalf("in-flight cancellation lost uncertainty: calls=%d err=%v", calls, err)
			}
		})
	}
}
