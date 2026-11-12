package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

type mediaTransport func(*http.Request) (*http.Response, error)

func (f mediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMixedMediaAndQuoteKeepSeparateLocations(t *testing.T) {
	a, s, _ := fixture(t)
	f := pushed()
	f.Body = json.RawMessage(`{"msgid":"m","chattype":"single","from":{"userid":"u"},"msgtype":"mixed","mixed":{"msg_item":[{"msgtype":"text","text":{"content":"first"}},{"msgtype":"image","image":{"url":"https://a.test/one","aeskey":"private"}},{"msgtype":"text","text":{"content":"second"}},{"msgtype":"image","image":{"url":"https://a.test/two"}}]},"quote":{"msgtype":"image","image":{"url":"https://a.test/quoted"}}}`)
	e, err := a.decode(f, s)
	if err != nil {
		t.Fatal(err)
	}
	if e.Text != "first\nsecond" || len(e.Parts) != 4 || len(e.Attachments) != 2 || e.Quote == nil || len(e.Quote.Attachments) != 1 {
		t.Fatalf("unexpected content: %+v", e)
	}
	refs := map[string]bool{}
	for _, p := range append(e.Attachments, e.Quote.Attachments...) {
		if refs[p.Ref] {
			t.Fatal("duplicate reference", p.Ref)
		}
		refs[p.Ref] = true
		if p.Scope == nil || *p.Scope != e.Route {
			t.Fatal("missing scope")
		}
	}
	raw, _ := json.Marshal(e)
	if bytes.Contains(raw, []byte("a.test")) || bytes.Contains(raw, []byte("private")) {
		t.Fatal("sensitive media in event JSON")
	}
	f.Body = json.RawMessage(`{"msgid":"n","chattype":"single","from":{"userid":"u"},"msgtype":"appmsg","appmsg":{"title":"report.pdf","file":{"url":"https://a.test/report"}},"quote":{"msgtype":"text","text":{"content":"quoted text"}}}`)
	e, err = a.decode(f, s)
	if err != nil || len(e.Attachments) != 1 || e.Attachments[0].Name != "report.pdf" || e.Quote.Text != "quoted text" {
		t.Fatal(e, err)
	}
}

func TestUnknownInboundTypeIsTyped(t *testing.T) {
	a, s, _ := fixture(t)
	f := pushed()
	f.Body = []byte(strings.Replace(string(f.Body), `"msgtype":"text"`, `"msgtype":"future"`, 1))
	_, err := a.decode(f, s)
	var unsupported *gateway.UnsupportedMessageError
	if !errors.As(err, &unsupported) || !errors.Is(err, gateway.ErrUnsupported) || unsupported.MessageType != "future" {
		t.Fatal(err)
	}
}

func TestInboundDownloadLocatorLifetimeMatchesOfficialFiveMinuteWindow(t *testing.T) {
	// The official SDK's ImageContent/FileContent/VideoContent URL fields expire
	// after five minutes: WecomTeam/aibot-node-sdk src/types/message.ts.
	a, s, _ := fixture(t)
	f := pushed()
	f.Body = json.RawMessage(`{"msgid":"media","chattype":"single","from":{"userid":"u"},"msgtype":"image","image":{"url":"https://cdn.test/image"},"quote":{"msgtype":"file","file":{"url":"https://cdn.test/quoted"}}}`)
	before := time.Now()
	e, err := a.decode(f, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(e.Attachments, e.Quote.Attachments...) {
		if p.ExpiresAt.Before(before) || p.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
			t.Fatalf("attachment advertises a lifetime longer than the platform URL: %s", p.ExpiresAt.Sub(before))
		}
	}
}

func TestMissingMediaLocationRejectsAdmission(t *testing.T) {
	a, s, _ := fixture(t)
	for _, body := range []string{
		`"msgtype":"image","image":{}`,
		`"msgtype":"file","file":{"filename":"missing.pdf"}`,
		`"msgtype":"video","video":{}`,
		`"msgtype":"mixed","mixed":{"msg_item":[{"msgtype":"image","image":{}}]}`,
		`"msgtype":"appmsg","appmsg":{"file":{}}`,
		`"msgtype":"text","text":{"content":"hello"},"quote":{"msgtype":"image","image":{}}`,
	} {
		f := pushed()
		f.Body = json.RawMessage(`{"msgid":"m","chattype":"single","from":{"userid":"u"},` + body + `}`)
		if _, err := a.decode(f, s); !errors.Is(err, gateway.ErrInvalidEvent) {
			t.Fatalf("missing location accepted: %s: %v", body, err)
		}
	}
}

func TestDownloadDecryptsObservedMediaAndRejectsScopeForgery(t *testing.T) {
	a, s, _ := fixture(t)
	plain := []byte("the exact file bytes")
	key := bytes.Repeat([]byte{0xfb}, 32)
	n := 32 - len(plain)%32
	encrypted := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(n)}, n)...)
	block, _ := aes.NewCipher(key)
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(encrypted, encrypted)
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://cdn.test/private" {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Disposition": {`attachment; filename="report.txt"`}}, Body: io.NopCloser(bytes.NewReader(encrypted))}, nil
	})}
	f := pushed()
	keyText := url.PathEscape(strings.TrimRight(base64.StdEncoding.EncodeToString(key), "="))
	f.Body, _ = json.Marshal(map[string]any{"msgid": "m", "chattype": "single", "from": map[string]string{"userid": "u"}, "msgtype": "file", "file": map[string]string{"url": "https://cdn.test/private", "aeskey": keyText}})
	e, err := a.decode(f, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || !bytes.Equal(got.Data, plain) || got.Name != "report.txt" {
		t.Fatal(got, err)
	}
	foreign := e.Route
	foreign.ChatID = "another"
	p := e.Attachments[0]
	p.Scope = &foreign
	if _, err = a.Download(context.Background(), foreign, p); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unexpected download", calls)
	}
	if _, err = a.Send(context.Background(), e.Route, gateway.Message{Attachments: e.Attachments}); !errors.Is(err, gateway.ErrUnsupported) {
		t.Fatal("download locator used as media_id", err)
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err = a.Download(context.Background(), e.Route, e.Attachments[0]); err == nil {
		t.Fatal("corrupt ciphertext accepted")
	}
}

func TestUploadChunksAndSendUsePlatformMediaID(t *testing.T) {
	a, s, socket := fixture(t)
	route := gateway.Route{Account: a.Account(), ChatID: "u", Kind: "direct"}
	payload := bytes.Repeat([]byte{7}, mediaChunkBytes+19)
	var commands []string
	var reconstructed []byte
	socket.onWrite = func(raw []byte) {
		var f frame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, f.Cmd)
		code := 0
		reply := frame{Headers: f.Headers, Code: &code}
		switch f.Cmd {
		case "aibot_upload_media_init":
			var obj map[string]any
			_ = json.Unmarshal(f.Body, &obj)
			if obj["total_size"] != float64(len(payload)) || obj["total_chunks"] != float64(2) || obj["md5"] == "" {
				t.Fatal(obj)
			}
			reply.Body = json.RawMessage(`{"upload_id":"upload-one"}`)
		case "aibot_upload_media_chunk":
			var b struct {
				ID    string `json:"upload_id"`
				Index int    `json:"chunk_index"`
				Data  string `json:"base64_data"`
			}
			_ = json.Unmarshal(f.Body, &b)
			if b.ID != "upload-one" || b.Index != len(reconstructed)/mediaChunkBytes {
				t.Fatal(b)
			}
			chunk, err := base64.StdEncoding.DecodeString(b.Data)
			if err != nil || len(chunk) > mediaChunkBytes {
				t.Fatal(err)
			}
			reconstructed = append(reconstructed, chunk...)
		case "aibot_upload_media_finish":
			reply.Body = json.RawMessage(`{"media_id":"platform-id","type":"file"}`)
		case "aibot_send_msg":
			if !bytes.Contains(f.Body, []byte(`"media_id":"platform-id"`)) {
				t.Fatal(string(f.Body))
			}
		default:
			t.Fatal(f.Cmd)
		}
		s.deliver(reply)
	}
	p, err := a.Upload(context.Background(), route, gateway.MediaUpload{Kind: "file", Name: "data.bin", Data: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstructed, payload) || len(commands) != 4 {
		t.Fatal("upload incomplete", commands)
	}
	if _, err = a.Send(context.Background(), route, gateway.Message{Attachments: []gateway.Attachment{p}}); err != nil {
		t.Fatal(err)
	}
	foreign := route
	foreign.ChatID = "other"
	forged := p
	forged.Scope = &foreign
	if _, err = a.Send(context.Background(), foreign, gateway.Message{Attachments: []gateway.Attachment{forged}}); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal("forged route accepted", err)
	}
	a.current = newSession(context.Background(), &fakeSocket{})
	defer a.current.close()
	if _, err = a.Send(context.Background(), route, gateway.Message{Attachments: []gateway.Attachment{p}}); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal("old connection accepted", err)
	}
	if len(commands) != 5 {
		t.Fatal("invalid send reached network", commands)
	}
}

func TestUploadFailureDoesNotRetryOrFinalize(t *testing.T) {
	a, s, socket := fixture(t)
	route := gateway.Route{Account: a.Account(), ChatID: "u", Kind: "direct"}
	calls := 0
	socket.onWrite = func(raw []byte) {
		calls++
		var f frame
		_ = json.Unmarshal(raw, &f)
		code := 0
		body := json.RawMessage(`{"upload_id":"x"}`)
		if f.Cmd == "aibot_upload_media_chunk" {
			code = 400
		}
		s.deliver(frame{Headers: f.Headers, Code: &code, Body: body})
	}
	if _, err := a.Upload(context.Background(), route, gateway.MediaUpload{Kind: "file", Name: "a", Data: []byte("payload")}); err == nil || calls != 2 {
		t.Fatal(err, calls)
	}
	a, s, socket = fixture(t)
	a.cfg.MediaTimeout = time.Millisecond
	calls = 0
	socket.onWrite = func([]byte) { calls++ }
	_, err := a.Upload(context.Background(), route, gateway.MediaUpload{Kind: "file", Name: "a", Data: []byte("payload")})
	var api *gateway.APIError
	if !errors.As(err, &api) || !api.Uncertain || calls != 1 || !socket.closed.Load() {
		t.Fatal(err, calls)
	}
}
