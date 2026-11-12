package wecomapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

type appMediaTransport func(*http.Request) (*http.Response, error)

func (f appMediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVideoLocatorDownloadDoesNotForwardApplicationCredentials(t *testing.T) {
	a := fixture(t)
	a.cfg.APIBase = "https://api.test"
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: appMediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := ""
		headers := http.Header{"Content-Type": {"application/json"}}
		switch r.URL.Host {
		case "api.test":
			if r.URL.Path == "/cgi-bin/gettoken" {
				body = `{"errcode":0,"access_token":"private-token","expires_in":7200}`
			} else {
				if r.URL.Query().Get("access_token") != "private-token" {
					t.Error("missing API token")
				}
				body = `{"video_url":"https://cdn.test/video"}`
			}
		case "cdn.test":
			if r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
				t.Error("credentials forwarded to CDN")
			}
			body = "video bytes"
			headers.Set("Content-Type", "video/mp4")
		default:
			t.Fatal("unexpected host", r.URL.Host)
		}
		return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>video</MsgType><MediaId>video-id</MediaId>", 1)
	e, err := a.Decode([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || string(got.Data) != "video bytes" || calls != 3 {
		t.Fatal(got, err, calls)
	}
}

func TestMediaUploadDownloadAndSend(t *testing.T) {
	a := fixture(t)
	payload := []byte("actual document bytes")
	uploads, downloads, sends := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = io.WriteString(w, `{"errcode":0,"access_token":"private-token","expires_in":7200}`)
		case "/cgi-bin/media/upload":
			uploads++
			if r.Method != "POST" || r.URL.Query().Get("access_token") != "private-token" || r.URL.Query().Get("type") != "file" {
				t.Error("wrong upload request")
			}
			mr, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				return
			}
			part, err := mr.NextPart()
			if err != nil {
				t.Error(err)
				return
			}
			data, err := io.ReadAll(part)
			if err != nil || part.FormName() != "media" || part.FileName() != "report.txt" || !bytes.Equal(data, payload) {
				t.Error("wrong multipart content", err)
			}
			_, _ = io.WriteString(w, `{"type":"file","media_id":"native-media","created_at":1000}`)
		case "/cgi-bin/media/get":
			downloads++
			if r.URL.Query().Get("media_id") != "native-media" {
				t.Error("wrong media id")
			}
			w.Header().Set("Content-Disposition", `attachment; filename="report.txt"`)
			_, _ = w.Write(payload)
		case "/cgi-bin/message/send":
			sends++
			var message struct {
				File struct {
					ID string `json:"media_id"`
				} `json:"file"`
			}
			if json.NewDecoder(r.Body).Decode(&message) != nil || message.File.ID != "native-media" {
				t.Error("did not resolve uploaded reference")
			}
			_, _ = io.WriteString(w, `{"errcode":0,"msgid":"sent"}`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a.cfg.APIBase = srv.URL
	route := gateway.Route{Account: a.Account(), TenantID: "corp", ChatID: "u", Kind: "direct"}
	p, err := a.Upload(context.Background(), route, gateway.MediaUpload{Kind: "file", Name: "report.txt", Data: payload})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Download(context.Background(), route, p)
	if err != nil || !bytes.Equal(got.Data, payload) || got.Name != "report.txt" {
		t.Fatal(got, err)
	}
	if _, err = a.Send(context.Background(), route, gateway.Message{Attachments: []gateway.Attachment{p}}); err != nil {
		t.Fatal(err)
	}
	foreign := route
	foreign.ChatID = "other"
	forged := p
	forged.Scope = &foreign
	if _, err = a.Send(context.Background(), foreign, gateway.Message{Attachments: []gateway.Attachment{forged}}); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal("cross-route send", err)
	}
	if _, err = a.Download(context.Background(), foreign, forged); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal("cross-route download", err)
	}
	p.ExpiresAt = time.Now().Add(-time.Second)
	if _, err = a.Download(context.Background(), route, p); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
	if uploads != 1 || downloads != 1 || sends != 1 {
		t.Fatal(uploads, downloads, sends)
	}
}

func TestObservedMediaDownloadAndBusinessError(t *testing.T) {
	a := fixture(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/cgi-bin/gettoken" {
			_, _ = io.WriteString(w, `{"errcode":0,"access_token":"secret","expires_in":7200}`)
			return
		}
		calls++
		_, _ = io.WriteString(w, `{"errcode":40014,"errmsg":"secret response must not escape"}`)
	}))
	defer srv.Close()
	a.cfg.APIBase = srv.URL
	xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>file</MsgType><MediaId>received</MediaId>", 1)
	e, err := a.Decode([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Download(context.Background(), e.Route, e.Attachments[0])
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Code != "40014" || strings.Contains(err.Error(), "secret") || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestMalformedMediaControlResponseIsNotFileContent(t *testing.T) {
	for _, body := range []string{`{"errcode":"40014","errmsg":"secret"}`, `{"errcode":40014`, `{}`, `null`} {
		t.Run(body, func(t *testing.T) {
			a := fixture(t)
			a.cfg.APIBase = "https://api.test"
			a.cfg.HTTPClient = &http.Client{Transport: appMediaTransport(func(r *http.Request) (*http.Response, error) {
				response := body
				if r.URL.Path == "/cgi-bin/gettoken" {
					response = `{"errcode":0,"access_token":"private","expires_in":7200}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>file</MsgType><MediaId>file-id</MediaId>", 1)
			e, err := a.Decode([]byte(xml))
			if err != nil {
				t.Fatal(err)
			}
			got, err := a.Download(context.Background(), e.Route, e.Attachments[0])
			var api *gateway.APIError
			if !errors.As(err, &api) || len(got.Data) != 0 || strings.Contains(err.Error(), "secret") {
				t.Fatal(got, err)
			}
		})
	}
}

func TestUnsupportedCallbackIsReportedAndAcknowledged(t *testing.T) {
	a := fixture(t)
	var reported error
	a.cfg.OnError = func(err error) { reported = err }
	xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>future</MsgType>", 1)
	w := httptest.NewRecorder()
	a.Handler(func(context.Context, gateway.Event) error { t.Fatal("unsupported content dispatched"); return nil }).ServeHTTP(w, request(a, "POST", encrypt(a, xml, "corp"), strconv.FormatInt(time.Now().Unix(), 10)))
	var unsupported *gateway.UnsupportedMessageError
	if w.Code != 200 || w.Body.String() != "success" || !errors.As(reported, &unsupported) || unsupported.MessageType != "future" {
		t.Fatal(w.Code, reported)
	}
}

func TestUploadBusinessFailureDoesNotRetry(t *testing.T) {
	a := fixture(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cgi-bin/gettoken" {
			_, _ = io.WriteString(w, `{"errcode":0,"access_token":"token","expires_in":7200}`)
			return
		}
		calls++
		_, _ = io.WriteString(w, `{"errcode":45009,"errmsg":"rate limited"}`)
	}))
	defer srv.Close()
	a.cfg.APIBase = srv.URL
	route := gateway.Route{Account: a.Account(), TenantID: "corp", ChatID: "u", Kind: "direct"}
	_, err := a.Upload(context.Background(), route, gateway.MediaUpload{Kind: "file", Name: "a", Data: []byte("b")})
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Code != "45009" || calls != 1 {
		t.Fatal(err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = a.Upload(ctx, route, gateway.MediaUpload{Kind: "file", Name: "a", Data: []byte("b")}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("cancelled upload reached server")
	}
}
