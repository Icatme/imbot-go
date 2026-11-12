package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Icatme/wechatbot-go/gateway"
)

func TestUnauthorizedMediaAndInteractionRefreshOnlyOnExplicitCall(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, method, path string
	}{
		{"download", "c2c_voice_quote.json", http.MethodGet, "/current.silk"},
		{"download WAV", "c2c_voice_quote.json", http.MethodGet, "/current.wav"},
		{"interaction ACK", "interaction_button_c2c.json", http.MethodPut, "/interactions/interaction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fixture(t)
			var v envelope
			if err := json.Unmarshal(protocolFixture(t, tc.fixture), &v); err != nil {
				t.Fatal(err)
			}
			e, err := a.decode(v)
			if err != nil {
				t.Fatal(err)
			}
			tokenCalls, operationCalls := 0, 0
			a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/app/getAppAccessToken" {
					tokenCalls++
					return response(fmt.Sprintf(`{"access_token":"tok%d","expires_in":7200}`, tokenCalls)), nil
				}
				operationCalls++
				if r.Method != tc.method || r.URL.Path != tc.path || r.Header.Get("Authorization") != fmt.Sprintf("QQBot tok%d", operationCalls) {
					t.Fatalf("wrong explicit operation: method=%s path=%s token-call=%d operation-call=%d", r.Method, r.URL.Path, tokenCalls, operationCalls)
				}
				if operationCalls == 1 {
					out := response(`{"message":"private credential detail"}`)
					out.StatusCode = http.StatusUnauthorized
					return out, nil
				}
				if tc.method == http.MethodPut {
					out := response("")
					out.StatusCode = http.StatusNoContent
					return out, nil
				}
				return response("audio bytes"), nil
			})}
			call := func() error {
				if e.Interaction != nil {
					return a.AcknowledgeInteraction(context.Background(), e.Route, e.Interaction.ID, 0)
				}
				if tc.name == "download WAV" {
					_, err := a.DownloadVoiceWAV(context.Background(), e.Route, e.Attachments[0])
					return err
				}
				_, err := a.Download(context.Background(), e.Route, e.Attachments[0])
				return err
			}
			err = call()
			var api *gateway.APIError
			if !errors.As(err, &api) || api.Status != http.StatusUnauthorized || strings.Contains(err.Error(), "private") {
				t.Fatalf("401 was hidden or leaked credentials: %v", err)
			}
			if tokenCalls != 1 || operationCalls != 1 {
				t.Fatalf("401 caused automatic retry: tokens=%d operations=%d", tokenCalls, operationCalls)
			}
			if err := call(); err != nil {
				t.Fatalf("explicit call failed after token invalidation: %v", err)
			}
			if tokenCalls != 2 || operationCalls != 2 {
				t.Fatalf("explicit call did not refresh exactly once: tokens=%d operations=%d", tokenCalls, operationCalls)
			}
		})
	}
}
