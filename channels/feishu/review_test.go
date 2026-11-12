package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/gateway"
)

func TestUploadedResourceCannotMoveRoutesByChangingScope(t *testing.T) {
	a := testAdapter(t)
	sends := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"tenant_access_token":"token","expire":7200}`
		if strings.HasSuffix(r.URL.Path, "/files") {
			body = `{"code":0,"data":{"file_key":"native-file"}}`
		}
		if strings.HasSuffix(r.URL.Path, "/messages") {
			var sent struct {
				Content string `json:"content"`
			}
			if json.NewDecoder(r.Body).Decode(&sent) != nil || !strings.Contains(sent.Content, `"file_key":"native-file"`) {
				t.Error("opaque upload ref was not resolved", sent.Content)
			}
			sends++
			body = `{"code":0,"data":{"message_id":"sent"}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	r := gateway.Route{Account: a.Account(), TenantID: "tenant", ChatID: "original", Kind: "direct"}
	p, err := a.Upload(context.Background(), r, gateway.MediaUpload{Kind: "file", Name: "report.pdf", Data: []byte("document")})
	if err != nil {
		t.Fatal(err)
	}
	for _, dimension := range []string{"chat", "tenant", "thread", "kind"} {
		foreign := r
		switch dimension {
		case "chat":
			foreign.ChatID = "other"
		case "tenant":
			foreign.TenantID = "other"
		case "thread":
			foreign.ThreadID = "other"
		case "kind":
			foreign.Kind = "group"
		}
		forged := p
		forged.Scope = &foreign
		if _, err := a.Send(context.Background(), foreign, gateway.Message{Attachments: []gateway.Attachment{forged}}); !errors.Is(err, gateway.ErrMediaExpired) || sends != 0 {
			t.Fatalf("upload retargeted by %s: sends=%d error=%v", dimension, sends, err)
		}
	}
	wrongKind := p
	wrongKind.Kind = "image"
	if _, err := a.Send(context.Background(), r, gateway.Message{Attachments: []gateway.Attachment{wrongKind}}); !errors.Is(err, gateway.ErrInvalidEvent) {
		t.Fatal(err)
	}
	if _, err := a.Send(context.Background(), r, gateway.Message{Attachments: []gateway.Attachment{p}}); err != nil || sends != 1 || !strings.HasPrefix(p.Ref, uploadRefPrefix) {
		t.Fatal(err, sends, p.Ref)
	}
	// Native keys explicitly supplied by hosts retain the existing Send contract.
	if _, err := a.Send(context.Background(), r, gateway.Message{Attachments: []gateway.Attachment{{Kind: "file", Ref: "native-file"}}}); err != nil || sends != 2 {
		t.Fatal(err, sends)
	}
	a.uploads.Put(wire.ContextKey(r, p.Ref), uploadedResource{Key: "native-file", Kind: "file"}, time.Now().Add(-time.Second))
	p.ExpiresAt = time.Now().Add(time.Hour)
	if _, err := a.Send(context.Background(), r, gateway.Message{Attachments: []gateway.Attachment{p}}); !errors.Is(err, gateway.ErrMediaExpired) || sends != 2 {
		t.Fatal(err, sends)
	}
}

func TestCardActionPreservesOfficialFormFields(t *testing.T) {
	a := testAdapter(t)
	a.rememberCardRoute(cardRoute(a), "msg")
	// CallBackAction from larksuite/oapi-sdk-go/event/dispatcher/callback/model.go.
	action := `{"tag":"button","name":"submit","value":{"operation":"save"},"form_value":{"name":"Ada"},"input_value":"input","option":"one","options":["one","two"],"checked":true,"timezone":"Asia/Shanghai"}`
	var body map[string]json.RawMessage
	if err := json.Unmarshal(cardPayload(), &body); err != nil {
		t.Fatal(err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(body["event"], &event); err != nil {
		t.Fatal(err)
	}
	event["action"] = json.RawMessage(action)
	body["event"], _ = json.Marshal(event)
	raw, _ := json.Marshal(body)
	e, err := a.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got, want bytes.Buffer
	if json.Compact(&got, e.Interaction.Value) != nil || json.Compact(&want, []byte(action)) != nil || got.String() != want.String() {
		t.Fatal(string(e.Interaction.Value))
	}
	serialized, _ := json.Marshal(e)
	if bytes.Contains(serialized, []byte("secret-card-token")) {
		t.Fatal("card credential included")
	}
}

func TestUploadInvalidTokenIsNotReusedOnNextCall(t *testing.T) {
	a := testAdapter(t)
	tokens, uploads := 0, 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"data":{"file_key":"native-file"}}`
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			tokens++
			token := "stale"
			if tokens > 1 {
				token = "fresh"
			}
			body = `{"code":0,"tenant_access_token":"` + token + `","expire":7200}`
		} else {
			uploads++
			if r.Header.Get("Authorization") == "Bearer stale" {
				body = `{"code":99991668}`
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	r := gateway.Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}
	u := gateway.MediaUpload{Kind: "file", Name: "report.pdf", Data: []byte("document")}
	if _, err := a.Upload(context.Background(), r, u); err == nil || tokens != 1 || uploads != 1 {
		t.Fatal(err, tokens, uploads)
	}
	if _, err := a.Upload(context.Background(), r, u); err != nil || tokens != 2 || uploads != 2 {
		t.Fatal(err, tokens, uploads)
	}
}

func TestSendHTTP401RefreshesOnlyOnNextExplicitCall(t *testing.T) {
	a := testAdapter(t)
	tokens, sends := 0, 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{"code":0,"data":{"message_id":"sent"}}`
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			tokens++
			token := "stale"
			if tokens > 1 {
				token = "fresh"
			}
			body = `{"code":0,"tenant_access_token":"` + token + `","expire":7200}`
		} else {
			sends++
			if r.Header.Get("Authorization") != "Bearer fresh" {
				status = http.StatusUnauthorized
				body = `{"code":99991668,"msg":"private server detail"}`
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	r := gateway.Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}
	_, err := a.Send(context.Background(), r, gateway.Message{Text: "hello"})
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Status != 401 || strings.Contains(err.Error(), "private") || tokens != 1 || sends != 1 {
		t.Fatalf("401 was hidden or retried: %v tokens=%d sends=%d", err, tokens, sends)
	}
	if _, err := a.Send(context.Background(), r, gateway.Message{Text: "hello"}); err != nil || tokens != 2 || sends != 2 {
		t.Fatalf("explicit send reused rejected token: %v tokens=%d sends=%d", err, tokens, sends)
	}
}

func TestHTTP400TokenErrorKeepsBusinessCodeAndRefreshesExplicitly(t *testing.T) {
	// Official SDK issue #6 records HTTP 400 with code 99991668. Media resource
	// GET also parses non-200 responses as JSON in service/im/v1/resource.go.
	for _, operation := range []string{"send", "upload", "download"} {
		t.Run(operation, func(t *testing.T) {
			a := testAdapter(t)
			e, err := a.Decode(messagePayload("file", `{"file_key":"key"}`))
			if err != nil {
				t.Fatal(err)
			}
			tokens, calls := 0, 0
			a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
				status := http.StatusOK
				body := "file bytes"
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					tokens++
					token := "stale"
					if tokens > 1 {
						token = "fresh"
					}
					body = `{"code":0,"tenant_access_token":"` + token + `","expire":7200}`
				} else {
					calls++
					if r.Header.Get("Authorization") != "Bearer fresh" {
						status = http.StatusBadRequest
						body = `{"code":99991668,"msg":"private server detail"}`
					} else if operation == "send" {
						body = `{"code":0,"data":{"message_id":"sent"}}`
					} else if operation == "upload" {
						body = `{"code":0,"data":{"file_key":"uploaded"}}`
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			call := func() error {
				switch operation {
				case "upload":
					_, err := a.Upload(context.Background(), e.Route, gateway.MediaUpload{Kind: "file", Name: "report.txt", Data: []byte("data")})
					return err
				case "download":
					_, err := a.Download(context.Background(), e.Route, e.Attachments[0])
					return err
				default:
					_, err := a.Send(context.Background(), e.Route, e.Reply("reply"))
					return err
				}
			}
			err = call()
			var api *gateway.APIError
			if !errors.As(err, &api) || api.Status != 400 || api.Code != "99991668" || api.Uncertain || strings.Contains(err.Error(), "private") || tokens != 1 || calls != 1 {
				t.Errorf("business error lost or retried: %v tokens=%d calls=%d", err, tokens, calls)
			}
			if err := call(); err != nil || tokens != 2 || calls != 2 {
				t.Fatalf("explicit operation reused rejected token: %v tokens=%d calls=%d", err, tokens, calls)
			}
		})
	}
}

func TestPlatformErrorKeepsHTTPUncertaintyAndRedaction(t *testing.T) {
	a := testAdapter(t)
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, status := `{"code":99991400,"msg":"private server detail"}`, http.StatusServiceUnavailable
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			body, status = `{"code":0,"tenant_access_token":"token","expire":7200}`, http.StatusOK
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	_, err := a.Send(context.Background(), gateway.Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}, gateway.Message{Text: "hello"})
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Code != "99991400" || api.Status != http.StatusServiceUnavailable || !api.Retryable || !api.Uncertain || strings.Contains(err.Error(), "private") {
		t.Fatalf("business parsing erased transport failure metadata: %v", err)
	}
}

func TestSendCancellationPreservesCauseAndUncertainty(t *testing.T) {
	a := testAdapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"tenant_access_token":"token","expire":7200}`))}, nil
		}
		cancel()
		return nil, ctx.Err()
	})}
	_, err := a.Send(ctx, gateway.Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}, gateway.Message{Text: "hello"})
	var api *gateway.APIError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &api) || !api.Uncertain || calls != 2 {
		t.Fatalf("cancelled in-flight send lost its cause/outcome: %v calls=%d", err, calls)
	}
}

func TestSuccessfulJSONResourceRemainsFileContent(t *testing.T) {
	a := testAdapter(t)
	tokens := 0
	const file = `{"code":99991668,"msg":"a legitimate JSON document"}`
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := file
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			tokens++
			body = `{"code":0,"tenant_access_token":"token","expire":7200}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	e, err := a.Decode(messagePayload("file", `{"file_key":"key","file_name":"report.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := a.Download(context.Background(), e.Route, e.Attachments[0])
		if err != nil || string(got.Data) != file || tokens != 1 {
			t.Fatalf("legitimate JSON file treated as API error: %v tokens=%d", err, tokens)
		}
	}
}

func TestUnknownChatTypeIsReportedWithoutInventingGroupRoute(t *testing.T) {
	a := testAdapter(t)
	a.cfg.RespondWithoutMention = true
	var v envelope
	if err := json.Unmarshal(payload(), &v); err != nil {
		t.Fatal(err)
	}
	v.Event.Message.ChatType = "future-chat-type"
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	reports, calls := 0, 0
	a.cfg.OnError = func(err error) {
		reports++
		var unsupported *gateway.UnsupportedMessageError
		if !errors.As(err, &unsupported) || unsupported.MessageType != "chat/future-chat-type" {
			t.Fatalf("wrong unknown-chat diagnostic: %v", err)
		}
	}
	if _, err := a.admit(context.Background(), raw, func(context.Context, gateway.Event) error { calls++; return nil }); err != nil || calls != 0 || reports != 1 {
		t.Fatalf("unknown chat was invented as group: %v sink=%d reports=%d", err, calls, reports)
	}
	r := gateway.Route{Account: a.Account(), TenantID: v.Header.Tenant, ChatID: v.Event.Message.ChatID, Kind: "group"}
	if _, exists := a.replyRoutes.Get(wire.ContextKey(r, v.Event.Message.ID)); exists {
		t.Fatal("unknown chat acquired a group reply capability")
	}
}

func TestPlatformImageContainersAcceptedWithoutConversion(t *testing.T) {
	a := testAdapter(t)
	uploads := 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"tenant_access_token":"token","expire":7200}`
		if strings.HasSuffix(r.URL.Path, "/images") {
			uploads++
			body = `{"code":0,"data":{"image_key":"tiff-key"}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	// Container signatures exercise local acceptance; the platform owns complete
	// image decoding, resolution limits and its documented TIFF/HEIC conversion.
	for _, tc := range []struct {
		name, mime string
		data       []byte
	}{
		{"little.tiff", "image/tiff", []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 0, 0}},
		{"big.tiff", "image/tiff", []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 0}},
		{"photo.heic", "image/heic", []byte("\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00heicmif1")},
	} {
		p, err := a.Upload(context.Background(), gateway.Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}, gateway.MediaUpload{Kind: "image", Name: tc.name, Data: tc.data})
		if err != nil || p.MIME != tc.mime {
			t.Fatal(tc.name, p, err)
		}
	}
	if uploads != 3 {
		t.Fatal(uploads)
	}
}

func TestSendRequiresExplicitSuccessCode(t *testing.T) {
	for _, response := range []string{`{"data":{"message_id":"msg"}}`, `{"code":null,"data":{"message_id":"msg"}}`} {
		t.Run(response, func(t *testing.T) {
			a := testAdapter(t)
			a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
				body := response
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					body = `{"code":0,"tenant_access_token":"token","expire":7200}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			route := cardRoute(a)
			route.ThreadID = ""
			out, err := a.Send(context.Background(), route, gateway.Message{Card: json.RawMessage(`{"schema":"2.0","body":{"elements":[]}}`)})
			var api *gateway.APIError
			if out.Accepted || !errors.As(err, &api) || !api.Uncertain {
				t.Errorf("missing success code accepted: receipt=%+v error=%v", out, err)
			}
			if _, ok := a.cardRoutes.Get(cardRouteKey(route.TenantID, route.ChatID, "msg")); ok {
				t.Fatal("invalid response created a card callback capability")
			}
		})
	}
}

func TestInvalidTenantTokenIsDiscardedOnNextExplicitCall(t *testing.T) {
	// Feishu's generic error table defines 99991665 as an invalid tenant token.
	for _, operation := range []string{"send", "upload", "download"} {
		t.Run(operation, func(t *testing.T) {
			a := testAdapter(t)
			e, err := a.Decode(messagePayload("file", `{"file_key":"key"}`))
			if err != nil {
				t.Fatal(err)
			}
			tokens, calls := 0, 0
			a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
				body, status := "file bytes", http.StatusOK
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					tokens++
					token := "old"
					if tokens > 1 {
						token = "new"
					}
					body = `{"code":0,"tenant_access_token":"` + token + `","expire":7200}`
				} else {
					calls++
					if r.Header.Get("Authorization") == "Bearer old" {
						body, status = `{"code":99991665,"msg":"private token detail"}`, http.StatusBadRequest
					} else if operation == "send" {
						body = `{"code":0,"data":{"message_id":"sent"}}`
					} else if operation == "upload" {
						body = `{"code":0,"data":{"file_key":"uploaded"}}`
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			call := func() error {
				switch operation {
				case "upload":
					_, err := a.Upload(context.Background(), e.Route, gateway.MediaUpload{Kind: "file", Name: "a.txt", Data: []byte("bytes")})
					return err
				case "download":
					_, err := a.Download(context.Background(), e.Route, e.Attachments[0])
					return err
				default:
					_, err := a.Send(context.Background(), e.Route, gateway.Message{Text: "hello"})
					return err
				}
			}
			err = call()
			var api *gateway.APIError
			if !errors.As(err, &api) || api.Code != "99991665" || tokens != 1 || calls != 1 || strings.Contains(err.Error(), "private") {
				t.Fatalf("first rejection was hidden or retried: %v tokens=%d calls=%d", err, tokens, calls)
			}
			if err := call(); err != nil || tokens != 2 || calls != 2 {
				t.Fatalf("explicit call reused invalid tenant token: %v tokens=%d calls=%d", err, tokens, calls)
			}
		})
	}
}
