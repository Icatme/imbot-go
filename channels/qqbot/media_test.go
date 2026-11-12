package qqbot

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

type mediaTransport func(*http.Request) (*http.Response, error)

func (f mediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func route(a *Adapter) gateway.Route {
	return gateway.Route{Account: a.Account(), TenantID: "app", ChatID: "user", Kind: "direct"}
}

func TestQuoteAndMediaRemainSeparate(t *testing.T) {
	a := fixture(t)
	e, err := a.decode(envelope{Type: "C2C_MESSAGE_CREATE", Data: json.RawMessage(`{"id":"m","author":{"user_openid":"user"},"content":"current","message_type":103,"attachments":[{"content_type":"image/png","url":"https://gchat.qpic.cn/current"}],"msg_elements":[{"content":"quoted","attachments":[{"content_type":"application/pdf","filename":"a.pdf","url":"https://multimedia.nt.qq.com.cn/quote?secret=value"}]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if e.Text != "current" || len(e.Attachments) != 1 || len(e.Parts) != 2 || e.Quote == nil || e.Quote.Text != "quoted" || len(e.Quote.Attachments) != 1 {
		t.Fatalf("wrong quote normalization: %+v", e)
	}
	if e.Attachments[0].Ref == e.Quote.Attachments[0].Ref {
		t.Fatal("current and quoted media share locator")
	}
	b, err := json.Marshal(e)
	if err != nil || bytes.Contains(b, []byte("secret=value")) || bytes.Contains(b, []byte("https://")) {
		t.Fatalf("secret leaked: %s, %v", b, err)
	}
}

func TestUnknownMessageReportedAndConsumed(t *testing.T) {
	for _, observe := range []bool{false, true} {
		a := fixture(t)
		calls := 0
		if observe {
			a.cfg.OnError = func(err error) {
				calls++
				var unsupported *gateway.UnsupportedMessageError
				if !errors.As(err, &unsupported) {
					t.Errorf("wrong error: %v", err)
				}
			}
		}
		w := httptest.NewRecorder()
		a.Handler(func(context.Context, gateway.Event) error { t.Fatal("unknown message dispatched"); return nil }).ServeHTTP(w, signRequest(a, []byte(`{"op":0,"t":"NEW_MESSAGE_TYPE","d":{}}`)))
		var ack map[string]int
		if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
			t.Fatal(err)
		}
		want := 0
		if observe {
			want = 0
			if calls != 1 {
				t.Fatal(calls)
			}
		}
		if ack["d"] != want {
			t.Fatal(observe, ack)
		}
	}
}

func TestDownloadUsesObservedRouteAndAuthenticatedCDN(t *testing.T) {
	a := fixture(t)
	e, err := a.decode(envelope{Type: "C2C_MESSAGE_CREATE", Data: json.RawMessage(`{"id":"m","author":{"user_openid":"user"},"attachments":[{"content_type":"application/pdf","filename":"a.pdf","size":7,"url":"https://multimedia.nt.qq.com.cn/download?secret=redacted"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		if r.URL.Host != "multimedia.nt.qq.com.cn" || r.Method != "GET" || r.Header.Get("Authorization") != "QQBot tok" {
			t.Errorf("wrong CDN request: %s %s %v", r.Method, r.URL.Host, r.Header)
		}
		return response("PDFDATA"), nil
	})}
	out, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || string(out.Data) != "PDFDATA" || out.Name != "a.pdf" || calls != 2 {
		t.Fatal(out, err, calls)
	}
	wrong := e.Route
	wrong.ChatID = "other"
	if _, err = a.Download(context.Background(), wrong, e.Attachments[0]); err == nil {
		t.Fatal("cross-route download")
	}
	if calls != 2 {
		t.Fatal("cross-route download made request")
	}
	unknown := e.Attachments[0]
	unknown.Ref = "lost"
	if _, err = a.Download(context.Background(), e.Route, unknown); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
}

func TestDownloadDoesNotLeakTokenToForeignHost(t *testing.T) {
	a := fixture(t)
	e, err := a.decode(envelope{Type: "C2C_MESSAGE_CREATE", Data: json.RawMessage(`{"id":"m","author":{"user_openid":"user"},"attachments":[{"url":"https://example.com/private","content_type":"image/png"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("requested foreign host or token")
		return nil, nil
	})}
	if _, err = a.Download(context.Background(), e.Route, e.Attachments[0]); err == nil {
		t.Fatal("foreign host accepted")
	}
}

func TestChunkedUploadThenRichMediaSend(t *testing.T) {
	a := fixture(t)
	data := []byte("abcdefgh")
	var calls []string
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPut {
			want := []byte("abcd")
			if r.URL.Path == "/2" {
				want = []byte("efgh")
			}
			if !bytes.Equal(body, want) || r.Header.Get("Authorization") != "" {
				t.Errorf("wrong COS bytes or leaked token: %s %v", body, r.Header)
			}
			return response(""), nil
		}
		if r.Header.Get("Authorization") != "QQBot tok" {
			t.Error("missing API auth")
		}
		var b map[string]any
		if err := json.Unmarshal(body, &b); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/v2/users/user/upload_prepare":
			m, s := md5.Sum(data), sha1.Sum(data)
			if b["md5"] != hex.EncodeToString(m[:]) || b["sha1"] != hex.EncodeToString(s[:]) || b["md5_10m"] != b["md5"] || b["file_size"] != float64(len(data)) {
				t.Errorf("wrong hashes or size: %v", b)
			}
			return response(`{"upload_id":"upload","block_size":4,"parts":[{"index":1,"presigned_url":"https://bucket.cos.ap-guangzhou.myqcloud.com/1"},{"index":2,"presigned_url":"https://bucket.cos.ap-guangzhou.myqcloud.com/2"}]}`), nil
		case "/v2/users/user/upload_part_finish":
			part := data[:4]
			if b["part_index"] == float64(2) {
				part = data[4:]
			}
			m := md5.Sum(part)
			if b["upload_id"] != "upload" || b["block_size"] != float64(4) || b["md5"] != hex.EncodeToString(m[:]) {
				t.Error(b)
			}
			return response(`{}`), nil
		case "/v2/users/user/files":
			if b["upload_id"] != "upload" {
				t.Error(b)
			}
			return response(`{"file_info":"INFO","ttl":300}`), nil
		case "/v2/users/user/messages":
			media, ok := b["media"].(map[string]any)
			if !ok || media["file_info"] != "INFO" || b["msg_type"] != float64(7) || b["msg_id"] != "incoming" {
				t.Error(b)
			}
			return response(`{"id":"sent"}`), nil
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
			return nil, nil
		}
	})}
	r := route(a)
	p, err := a.Upload(context.Background(), r, gateway.MediaUpload{Kind: "file", Name: "a.pdf", MIME: "application/pdf", ReplyTo: "incoming", Data: data})
	if err != nil || !strings.HasPrefix(p.Ref, uploadRefPrefix) || p.Scope == nil || *p.Scope != r || !p.ExpiresAt.After(time.Now()) {
		t.Fatal(p, err)
	}
	if raw, err := json.Marshal(p); err != nil || bytes.Contains(raw, []byte("INFO")) {
		t.Fatalf("uploaded file_info escaped private cache: %s, %v", raw, err)
	}
	if _, err = a.Send(context.Background(), r, gateway.Message{ReplyTo: "incoming", Attachments: []gateway.Attachment{p}}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 8 {
		t.Fatal(calls)
	}
}

func TestUploadRejectsInvalidPartPlanBeforeCOS(t *testing.T) {
	for _, parts := range []string{`[]`, `[{"index":1,"presigned_url":"https://cos.example/1"},{"index":1,"presigned_url":"https://cos.example/2"}]`, `[{"index":1,"presigned_url":"https://127.0.0.1/a"},{"index":2,"presigned_url":"https://cos.example/2"}]`} {
		a := fixture(t)
		a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/app/getAppAccessToken" {
				return response(`{"access_token":"tok","expires_in":7200}`), nil
			}
			if !strings.HasSuffix(r.URL.Path, "/upload_prepare") {
				t.Fatal("invalid plan caused further IO", r.URL)
			}
			return response(fmt.Sprintf(`{"upload_id":"u","block_size":2,"parts":%s}`, parts)), nil
		})}
		if _, err := a.Upload(context.Background(), route(a), gateway.MediaUpload{Kind: "file", ReplyTo: "m", Data: []byte("data")}); err == nil {
			t.Fatal("invalid plan accepted")
		}
	}
}

func TestPartFailureIsNotRetriedOrCompleted(t *testing.T) {
	a := fixture(t)
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		switch {
		case r.URL.Path == "/app/getAppAccessToken":
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		case strings.HasSuffix(r.URL.Path, "/upload_prepare"):
			return response(`{"upload_id":"u","block_size":4,"parts":[{"index":1,"presigned_url":"https://bucket.cos.ap-guangzhou.myqcloud.com/1"}]}`), nil
		case r.Method == http.MethodPut:
			return response(""), nil
		case strings.HasSuffix(r.URL.Path, "/upload_part_finish"):
			return response(`{"code":40093001,"message":"private details"}`), nil
		default:
			t.Fatal("upload continued after failure")
			return nil, nil
		}
	})}
	_, err := a.Upload(context.Background(), route(a), gateway.MediaUpload{Kind: "file", ReplyTo: "m", Data: []byte("data")})
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Code != "40093001" || calls != 4 || strings.Contains(err.Error(), "private") {
		t.Fatal(err, calls)
	}
}

func TestURLUploadIsExplicitAndDoesNotSend(t *testing.T) {
	a := fixture(t)
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		if r.URL.Path != "/v2/users/user/files" {
			t.Fatal("fetched or sent unexpected URL", r.URL)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["srv_send_msg"] != false || body["url"] != "https://example.com/file.pdf" || body["file_type"] != float64(4) {
			t.Error(body)
		}
		return response(`{"file_info":"INFO","ttl":300}`), nil
	})}
	for _, source := range []string{"http://example.com/f", "https://127.0.0.1/f", "https://user:pass@example.com/f"} {
		if _, err := a.UploadURL(context.Background(), route(a), URLUpload{Kind: "file", URL: source, ReplyTo: "m"}); err == nil {
			t.Fatal(source)
		}
	}
	if calls != 0 {
		t.Fatal("invalid input caused network IO")
	}
	if _, err := a.UploadURL(context.Background(), route(a), URLUpload{Kind: "file", URL: "https://example.com/file.pdf", ReplyTo: "m"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestUploadHashWindowAndHTTPQuotaError(t *testing.T) {
	a := fixture(t)
	data := bytes.Repeat([]byte{'a'}, 10002433)
	data[len(data)-1] = 'b'
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(`{"access_token":"tok","expires_in":7200}`), nil
		}
		if !strings.HasSuffix(r.URL.Path, "/upload_prepare") {
			t.Fatal("quota failure continued upload")
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Fatal(err)
		}
		first, full := md5.Sum(data[:10002432]), md5.Sum(data)
		if b["md5_10m"] != hex.EncodeToString(first[:]) || b["md5"] != hex.EncodeToString(full[:]) || b["md5"] == b["md5_10m"] {
			t.Error("incorrect QQ first-10002432-byte window")
		}
		res := response(`{"code":40093002,"message":"private token detail"}`)
		res.StatusCode = http.StatusBadRequest
		return res, nil
	})}
	_, err := a.Upload(context.Background(), route(a), gateway.MediaUpload{Kind: "file", ReplyTo: "m", Data: data})
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Code != "40093002" || api.Status != 400 || api.Retryable || strings.Contains(err.Error(), "private") || calls != 2 {
		t.Fatal(err, calls)
	}
}

func TestMediaCancellationScopeAndExpiryBeforeIO(t *testing.T) {
	a := fixture(t)
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid operation performed network IO")
		return nil, nil
	})}
	r := route(a)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Upload(ctx, r, gateway.MediaUpload{Kind: "file", ReplyTo: "m", Data: []byte("data")}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	other := r
	other.ChatID = "different"
	p := gateway.Attachment{Kind: "file", Ref: "FILEINFO", Scope: &other}
	if _, err := a.Send(context.Background(), r, gateway.Message{ReplyTo: "m", Attachments: []gateway.Attachment{p}}); err == nil {
		t.Fatal("cross-route send accepted")
	}
	p.Scope = &r
	p.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := a.Send(context.Background(), r, gateway.Message{ReplyTo: "m", Attachments: []gateway.Attachment{p}}); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
	p.ExpiresAt = time.Time{}
	p.MessageID = "inbound"
	if _, err := a.Send(context.Background(), r, gateway.Message{ReplyTo: "m", Attachments: []gateway.Attachment{p}}); !errors.Is(err, gateway.ErrUnsupported) {
		t.Fatal("inbound locator used as file_info", err)
	}
}
