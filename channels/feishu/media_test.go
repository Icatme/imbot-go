package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

func TestMediaUploadAndDownloadWireContract(t *testing.T) {
	want := []byte("file contents")
	var uploads, downloads int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			io.WriteString(w, `{"code":0,"tenant_access_token":"secret","expire":7200}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing authentication")
		}
		switch r.URL.Path {
		case "/open-apis/im/v1/files":
			uploads++
			if r.Method != "POST" {
				t.Error(r.Method)
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			if r.FormValue("file_type") != "pdf" || r.FormValue("file_name") != "report.pdf" {
				t.Error(r.Form)
			}
			f, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			got, _ := io.ReadAll(f)
			if !bytes.Equal(got, want) {
				t.Error("file bytes changed")
			}
			io.WriteString(w, `{"code":0,"data":{"file_key":"uploaded-key"}}`)
		case "/open-apis/im/v1/messages/msg/resources/incoming-key":
			downloads++
			if r.URL.Query().Get("type") != "file" {
				t.Error(r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `attachment; filename="report.pdf"`)
			w.Write(want)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	a := testAdapter(t)
	a.cfg.BaseURL = s.URL
	e, err := a.Decode(messagePayload("file", `{"file_key":"incoming-key","file_name":"incoming.pdf"}`))
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := a.Upload(context.Background(), e.Route, gateway.MediaUpload{Kind: "file", Name: "report.pdf", Data: want})
	if err != nil || !strings.HasPrefix(uploaded.Ref, uploadRefPrefix) || uploaded.Scope == nil || *uploaded.Scope != e.Route {
		t.Fatal(uploaded, err)
	}
	got, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || !bytes.Equal(got.Data, want) || got.Name != "report.pdf" || got.MIME != "application/pdf" {
		t.Fatal(got, err)
	}
	other := e.Route
	other.ChatID = "other"
	if _, err = a.Download(context.Background(), other, e.Attachments[0]); !errors.Is(err, gateway.ErrInvalidEvent) {
		t.Fatal(err)
	}
	missing := e.Attachments[0]
	missing.MessageID = "not-observed"
	if _, err = a.Download(context.Background(), e.Route, missing); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
	expired := e.Attachments[0]
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if _, err = a.Download(context.Background(), e.Route, expired); !errors.Is(err, gateway.ErrMediaExpired) {
		t.Fatal(err)
	}
	if uploads != 1 || downloads != 1 {
		t.Fatal(uploads, downloads)
	}
}

func TestMediaUploadRejectsInvalidInputsBeforeNetwork(t *testing.T) {
	a := testAdapter(t)
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid input caused network IO")
		return nil, nil
	})}
	r := gateway.Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}
	for _, u := range []gateway.MediaUpload{{Kind: "voice", Name: "sound.mp3", Data: []byte("mp3 bytes")}, {Kind: "video", Name: "movie.mp4", Data: []byte("not mp4")}, {Kind: "image", Name: "image.png", Data: []byte("not an image")}, {Kind: "file", Name: "", Data: []byte("data")}} {
		if _, err := a.Upload(context.Background(), r, u); err == nil {
			t.Fatal(u)
		}
	}
	if _, err := a.Upload(context.Background(), r, gateway.MediaUpload{Kind: "image", Name: "x.png", Data: make([]byte, imageUploadLimit+1)}); !errors.Is(err, gateway.ErrMediaTooLarge) {
		t.Fatal(err)
	}
}

type mediaRoundTrip func(*http.Request) (*http.Response, error)

func (f mediaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMediaUploadBusinessErrorAndMissingKey(t *testing.T) {
	for _, reply := range []string{`{"code":123}`, `{"code":0,"data":{}}`, `{"data":{"file_key":"x"}}`} {
		t.Run(reply, func(t *testing.T) {
			a := testAdapter(t)
			calls := 0
			a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
				body := `{"code":0,"tenant_access_token":"token","expire":7200}`
				if strings.HasSuffix(r.URL.Path, "/files") {
					calls++
					body = reply
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			_, err := a.Upload(context.Background(), gateway.Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}, gateway.MediaUpload{Kind: "file", Name: "a.bin", Data: []byte("data")})
			if err == nil || calls != 1 {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestNativeMediaMultipartTypes(t *testing.T) {
	opus := append([]byte("OggS"), make([]byte, 24)...)
	opus = append(opus, []byte("OpusHead")...)
	for _, tc := range []struct {
		kind, typ, field string
		data             []byte
	}{
		{"image", "message", "image", []byte("\x89PNG\r\n\x1a\nimage bytes")},
		{"audio", "opus", "file", opus},
		{"video", "mp4", "file", []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			a := testAdapter(t)
			calls := 0
			a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
				body := `{"code":0,"tenant_access_token":"token","expire":7200}`
				if strings.HasSuffix(r.URL.Path, "/images") || strings.HasSuffix(r.URL.Path, "/files") {
					calls++
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					field := "file_type"
					if tc.kind == "image" {
						field = "image_type"
					}
					if r.FormValue(field) != tc.typ {
						t.Fatal(r.Form)
					}
					if tc.kind != "image" && r.FormValue("duration") != "1000" {
						t.Fatal(r.Form)
					}
					f, _, err := r.FormFile(tc.field)
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
					data, _ := io.ReadAll(f)
					if !bytes.Equal(data, tc.data) {
						t.Fatal("upload bytes differ")
					}
					body = `{"code":0,"data":{"image_key":"image-key","file_key":"file-key"}}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			p, err := a.Upload(context.Background(), gateway.Route{Account: a.Account(), ChatID: "chat", Kind: "direct"}, gateway.MediaUpload{Kind: tc.kind, Name: "media", Data: tc.data, DurationMS: 1000})
			if err != nil || calls != 1 || p.Kind != tc.kind {
				t.Fatal(p, calls, err)
			}
		})
	}
}

func TestOutboundPostAndNativeMedia(t *testing.T) {
	for _, kind := range []string{"image", "file", "voice", "audio", "video"} {
		typ, raw, err := outboundContent(gateway.Message{Attachments: []gateway.Attachment{{Kind: kind, Ref: "key"}}})
		if err != nil {
			t.Fatal(err)
		}
		want := kind
		if kind == "voice" {
			want = "audio"
		}
		if kind == "video" {
			want = "media"
		}
		if typ != want || !strings.Contains(string(raw), "key") {
			t.Fatal(typ, string(raw))
		}
	}
	typ, raw, err := outboundContent(gateway.Message{Text: "**Caption**", Format: "markdown", Attachments: []gateway.Attachment{{Kind: "image", Ref: "img"}, {Kind: "video", Ref: "video"}}})
	if err != nil || typ != "post" {
		t.Fatal(typ, err)
	}
	var body struct {
		ZH struct {
			Content [][]map[string]string `json:"content"`
		} `json:"zh_cn"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.ZH.Content) != 3 || body.ZH.Content[0][0]["tag"] != "md" || body.ZH.Content[1][0]["tag"] != "img" || body.ZH.Content[2][0]["tag"] != "media" {
		t.Fatal(string(raw))
	}
	if _, _, err = outboundContent(gateway.Message{Text: "caption", Attachments: []gateway.Attachment{{Kind: "file", Ref: "key"}}}); !errors.Is(err, gateway.ErrUnsupported) {
		t.Fatal("file caption should not invent a post node", err)
	}
}
