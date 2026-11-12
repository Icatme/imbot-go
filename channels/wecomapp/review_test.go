package wecomapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

type cancelAfterResponseBody struct {
	*strings.Reader
	cancel context.CancelFunc
}

func (b *cancelAfterResponseBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if b.Reader.Len() == 0 {
		b.cancel()
		return n, io.EOF
	}
	return n, err
}

func TestCompletedUploadSurvivesConcurrentCancellation(t *testing.T) {
	a := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var uploads, sends int
	a.cfg.HTTPClient = &http.Client{Transport: appMediaTransport(func(r *http.Request) (*http.Response, error) {
		var body io.Reader
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			body = strings.NewReader(`{"errcode":0,"access_token":"token","expires_in":7200}`)
		case "/cgi-bin/media/upload":
			uploads++
			body = &cancelAfterResponseBody{strings.NewReader(`{"type":"file","media_id":"accepted-media"}`), cancel}
		case "/cgi-bin/message/send":
			sends++
			var message struct {
				File struct {
					ID string `json:"media_id"`
				} `json:"file"`
			}
			if err := json.NewDecoder(r.Body).Decode(&message); err != nil || message.File.ID != "accepted-media" {
				t.Fatalf("completed upload was not retained: %+v %v", message, err)
			}
			body = strings.NewReader(`{"errcode":0,"msgid":"sent"}`)
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(body)}, nil
	})}
	route := gateway.Route{Account: a.Account(), TenantID: "corp", ChatID: "u", Kind: "direct"}
	p, err := a.Upload(ctx, route, gateway.MediaUpload{Kind: "file", Name: "report.txt", Data: []byte("content")})
	if err != nil || p.Ref == "" || ctx.Err() != context.Canceled {
		t.Fatalf("completed upload lost: %+v error=%v context=%v", p, err, ctx.Err())
	}
	if _, err := a.Send(context.Background(), route, gateway.Message{Attachments: []gateway.Attachment{p}}); err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || sends != 1 {
		t.Fatalf("uploads=%d sends=%d", uploads, sends)
	}
}

func TestObservedMediaCannotBeReboundForSend(t *testing.T) {
	a := fixture(t)
	xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>file</MsgType><MediaId>native-received-id</MediaId>", 1)
	e, err := a.Decode([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	foreign := e.Route
	foreign.ChatID = "another-user"
	p := e.Attachments[0]
	p.Scope = &foreign
	calls := 0
	a.cfg.HTTPClient = &http.Client{Transport: appMediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"errcode":0,"msgid":"sent"}`
		if r.URL.Path == "/cgi-bin/gettoken" {
			body = `{"errcode":0,"access_token":"token","expires_in":7200}`
		} else {
			var sent struct {
				File struct {
					ID string `json:"media_id"`
				} `json:"file"`
			}
			if json.NewDecoder(r.Body).Decode(&sent) != nil || sent.File.ID != "native-received-id" {
				t.Fatal("observed locator was not resolved to its native media ID")
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if _, err := a.Send(context.Background(), foreign, gateway.Message{Attachments: []gateway.Attachment{p}}); !errors.Is(err, gateway.ErrMediaExpired) || calls != 0 {
		t.Fatalf("observed media was rebound and sent: error=%v HTTP requests=%d", err, calls)
	}
	if _, err := a.Send(context.Background(), e.Route, gateway.Message{Attachments: e.Attachments}); err != nil || calls != 2 {
		t.Fatalf("valid same-route media send failed: %v calls=%d", err, calls)
	}
}

func TestApplicationContentPartsIncludeTextAndRecognizedVoice(t *testing.T) {
	a := fixture(t)
	e, err := a.Decode([]byte(xmlMessage()))
	if err != nil || len(e.Parts) != 1 || e.Parts[0].Text != "你好" {
		t.Fatalf("text missing from ordered content: %+v %v", e.Parts, err)
	}
	xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>voice</MsgType><MediaId>native-voice</MediaId><Recognition>voice transcript</Recognition>", 1)
	e, err = a.Decode([]byte(xml))
	if err != nil || e.Text != "voice transcript" || len(e.Parts) != 2 || e.Parts[0].Text != "voice transcript" || e.Parts[1].Attachment == nil || e.Parts[1].Attachment.Ref != e.Attachments[0].Ref {
		t.Fatalf("voice transcript/media missing from ordered content: %+v %v", e.Parts, err)
	}
}

func TestApplication401RefreshesOnlyOnExplicitCall(t *testing.T) {
	for _, operation := range []string{"download", "upload", "send"} {
		t.Run(operation, func(t *testing.T) {
			a := fixture(t)
			xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>file</MsgType><MediaId>received</MediaId>", 1)
			e, err := a.Decode([]byte(xml))
			if err != nil {
				t.Fatal(err)
			}
			tokens, calls := 0, 0
			a.cfg.HTTPClient = &http.Client{Transport: appMediaTransport(func(r *http.Request) (*http.Response, error) {
				out := &http.Response{StatusCode: 200, Header: make(http.Header)}
				if r.URL.Path == "/cgi-bin/gettoken" {
					tokens++
					out.Body = io.NopCloser(strings.NewReader(fmt.Sprintf(`{"errcode":0,"access_token":"token%d","expires_in":7200}`, tokens)))
					return out, nil
				}
				calls++
				if got := r.URL.Query().Get("access_token"); got != fmt.Sprintf("token%d", calls) {
					t.Errorf("explicit operation reused rejected token: %q", got)
				}
				if calls == 1 {
					out.StatusCode = http.StatusUnauthorized
					out.Body = io.NopCloser(strings.NewReader(`{"errmsg":"private server detail"}`))
				} else {
					body := "file bytes"
					if operation == "upload" {
						body = `{"type":"file","media_id":"uploaded"}`
					} else if operation == "send" {
						body = `{"errcode":0,"msgid":"sent"}`
					}
					out.Body = io.NopCloser(strings.NewReader(body))
				}
				return out, nil
			})}
			call := func() error {
				switch operation {
				case "download":
					_, err := a.Download(context.Background(), e.Route, e.Attachments[0])
					return err
				case "upload":
					_, err := a.Upload(context.Background(), e.Route, gateway.MediaUpload{Kind: "file", Name: "report.txt", Data: []byte("data")})
					return err
				default:
					_, err := a.Send(context.Background(), e.Route, gateway.Message{Text: "reply"})
					return err
				}
			}
			err = call()
			var api *gateway.APIError
			if !errors.As(err, &api) || api.Status != 401 || strings.Contains(err.Error(), "private") || tokens != 1 || calls != 1 {
				t.Fatalf("first 401 was not returned without retry: %v tokens=%d calls=%d", err, tokens, calls)
			}
			if err := call(); err != nil || tokens != 2 || calls != 2 {
				t.Fatalf("explicit call did not refresh credentials: %v tokens=%d calls=%d", err, tokens, calls)
			}
		})
	}
}

func TestDownloadPreservesDeclaredMediaType(t *testing.T) {
	a := fixture(t)
	a.cfg.HTTPClient = &http.Client{Transport: appMediaTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"ok":true}`
		headers := http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Content-Disposition": {`attachment; filename="report.json"`}}
		if r.URL.Path == "/cgi-bin/gettoken" {
			body = `{"errcode":0,"access_token":"token","expires_in":7200}`
			headers.Del("Content-Disposition")
		}
		return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>file</MsgType><MediaId>native-file</MediaId>", 1)
	e, err := a.Decode([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || got.MIME != "application/json" || got.Name != "report.json" || string(got.Data) != `{"ok":true}` {
		t.Fatalf("download lost declared MIME: %+v %v", got, err)
	}
}

func TestLateAuthenticationFailureKeepsRefreshedToken(t *testing.T) {
	for _, operation := range []string{"download", "upload", "send"} {
		for _, failure := range []string{"http401", "business40014"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				a := fixture(t)
				xml := strings.Replace(xmlMessage(), "<MsgType>text</MsgType>", "<MsgType>file</MsgType><MediaId>received</MediaId>", 1)
				e, err := a.Decode([]byte(xml))
				if err != nil {
					t.Fatal(err)
				}
				started, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(unblock)
				var tokens, oldRequests atomic.Int32
				a.cfg.HTTPClient = &http.Client{Transport: appMediaTransport(func(r *http.Request) (*http.Response, error) {
					out := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}}
					var body string
					if r.URL.Path == "/cgi-bin/gettoken" {
						body = fmt.Sprintf(`{"errcode":0,"access_token":"token%d","expires_in":7200}`, tokens.Add(1))
					} else if r.URL.Query().Get("access_token") == "token1" {
						if oldRequests.Add(1) == 1 {
							close(started)
							select {
							case <-release:
							case <-r.Context().Done():
								return nil, r.Context().Err()
							}
						}
						body = `{"errcode":40014}`
						if failure == "http401" {
							out.StatusCode = http.StatusUnauthorized
						}
					} else {
						switch operation {
						case "download":
							body = "file bytes"
							out.Header.Set("Content-Type", "application/octet-stream")
						case "upload":
							body = `{"type":"file","media_id":"uploaded"}`
						case "send":
							body = `{"errcode":0,"msgid":"sent"}`
						}
					}
					out.Body = io.NopCloser(strings.NewReader(body))
					return out, nil
				})}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				call := func() error {
					switch operation {
					case "download":
						_, err := a.Download(ctx, e.Route, e.Attachments[0])
						return err
					case "upload":
						_, err := a.Upload(ctx, e.Route, gateway.MediaUpload{Kind: "file", Name: "report.txt", Data: []byte("data")})
						return err
					default:
						_, err := a.Send(ctx, e.Route, gateway.Message{Text: "reply"})
						return err
					}
				}
				late := make(chan error, 1)
				go func() { late <- call() }()
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("first request did not reach the API")
				}
				if err := call(); err == nil {
					t.Fatal("old token failure was accepted")
				}
				if err := call(); err != nil || tokens.Load() != 2 {
					t.Fatalf("explicit call did not refresh: error=%v token fetches=%d", err, tokens.Load())
				}
				unblock()
				if err := <-late; err == nil {
					t.Fatal("late old-token failure was accepted")
				}
				if err := call(); err != nil || tokens.Load() != 2 || oldRequests.Load() != 2 {
					t.Fatalf("late failure discarded new token: error=%v token fetches=%d old requests=%d", err, tokens.Load(), oldRequests.Load())
				}
			})
		}
	}
}
