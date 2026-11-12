package qqbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

// Fixtures are authored from the protocol, independently of the decoder structs:
// https://raw.githubusercontent.com/tencent-connect/qqbot-nodejs/main/src/protocol/types.ts
// https://raw.githubusercontent.com/tencent-connect/botgo/master/dto/interaction.go
func protocolFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPlatformASRPreservesCurrentAndQuotedVoice(t *testing.T) {
	a := fixture(t)
	var v envelope
	if err := json.Unmarshal(protocolFixture(t, "c2c_voice_quote.json"), &v); err != nil {
		t.Fatal(err)
	}
	e, err := a.decode(v)
	if err != nil {
		t.Fatal(err)
	}
	if e.Text != "current caption\ncurrent transcript" || e.Quote == nil || e.Quote.Text != "quoted caption\nquoted transcript" || len(e.Attachments) != 1 || len(e.Quote.Attachments) != 1 {
		t.Fatalf("voice transcript lost or mixed with quote: %+v", e)
	}
	if len(e.Parts) != 3 || e.Parts[0].Text != "current caption" || e.Parts[1].Attachment == nil || e.Parts[1].Attachment.Kind != "voice" || e.Parts[2].Text != "current transcript" {
		t.Fatalf("voice parts out of order: %+v", e.Parts)
	}
	encoded, err := json.Marshal(e)
	if err != nil || bytes.Contains(encoded, []byte("private=")) || bytes.Contains(encoded, []byte("https://")) {
		t.Fatalf("voice URLs leaked: %s %v", encoded, err)
	}
	// QQ may deliver a voice message without any ordinary text content.
	v.Data = bytes.Replace(v.Data, []byte(`"current caption"`), []byte(`""`), 1)
	e, err = a.decode(v)
	if err != nil || e.Text != "current transcript" || len(e.Parts) != 2 || e.Parts[0].Attachment == nil || e.Parts[1].Text != "current transcript" {
		t.Fatalf("ASR-only content lost: %+v %v", e, err)
	}
}

func TestVoiceWAVSelectionIsExplicitAndPrivate(t *testing.T) {
	a := fixture(t)
	var v envelope
	if err := json.Unmarshal(protocolFixture(t, "c2c_voice_quote.json"), &v); err != nil {
		t.Fatal(err)
	}
	e, err := a.decode(v)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		if r.URL.Host != "multimedia.nt.qq.com.cn" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "QQBot tok" {
			t.Fatalf("wrong voice download: %s %s", r.Method, r.URL)
		}
		out := response(r.URL.Path)
		out.Header.Set("Content-Type", "audio/silk")
		return out, nil
	})}
	original, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || original.MIME != "audio/silk" || string(original.Data) != "/current.silk" {
		t.Fatal(original, err)
	}
	wav, err := a.DownloadVoiceWAV(context.Background(), e.Route, e.Attachments[0])
	if err != nil || wav.MIME != "audio/wav" || wav.Name != "current.wav" || string(wav.Data) != "/current.wav" {
		t.Fatal(wav, err)
	}
	quoted, err := a.DownloadVoiceWAV(context.Background(), e.Route, e.Quote.Attachments[0])
	if err != nil || quoted.MIME != "audio/wav" || quoted.Name != "quoted.wav" || string(quoted.Data) != "/quoted.wav" {
		t.Fatal(quoted, err)
	}
	if strings.Join(paths, ",") != "/app/getAppAccessToken,/current.silk,/current.wav,/quoted.wav" {
		t.Fatalf("unexpected rendition download: %v", paths)
	}
	plain, err := a.decode(envelope{Type: "C2C_MESSAGE_CREATE", Data: json.RawMessage(`{"id":"plain","author":{"user_openid":"user"},"attachments":[{"content_type":"voice","url":"https://multimedia.nt.qq.com.cn/plain.silk"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DownloadVoiceWAV(context.Background(), plain.Route, plain.Attachments[0]); !errors.Is(err, gateway.ErrUnsupported) || len(paths) != 4 {
		t.Fatalf("missing WAV fell back or used network: %v %v", err, paths)
	}
}

func TestUnknownInteractionTypesAreReportedAndConsumed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		top, sub int
	}{
		{"future command", 99, 11},
		{"ping", 1, 11},
		{"wrong top-level button number", 11, 11},
		{"search", 2, 9},
		{"menu", 2, 12},
		{"future payload", 2, 99},
		{"missing payload type", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fixture(t)
			var root map[string]any
			if err := json.Unmarshal(protocolFixture(t, "interaction_button_c2c.json"), &root); err != nil {
				t.Fatal(err)
			}
			d := root["d"].(map[string]any)
			d["type"] = tc.top
			d["data"].(map[string]any)["type"] = tc.sub
			raw, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			reported := 0
			a.cfg.OnError = func(err error) {
				reported++
				var unsupported *gateway.UnsupportedMessageError
				if !errors.As(err, &unsupported) || unsupported.Platform != "qqbot" || !strings.HasPrefix(unsupported.MessageType, "interaction:") {
					t.Fatalf("wrong unsupported diagnostic: %v", err)
				}
			}
			w := httptest.NewRecorder()
			a.Handler(func(context.Context, gateway.Event) error { t.Fatal("unknown interaction reached sink"); return nil }).ServeHTTP(w, signRequest(a, raw))
			var ack map[string]int
			if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || reported != 1 || ack["op"] != 12 || ack["d"] != 0 {
				t.Fatalf("unknown interaction not observably consumed: %d %d %v", w.Code, reported, ack)
			}
			if _, exists := a.interactions.Get(wire.ContextKey(route(a), "interaction")); exists {
				t.Fatal("unknown interaction gained an acknowledgement cache entry")
			}
		})
	}
}
