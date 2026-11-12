package imbot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Icatme/imbot-go/internal/crypto"
)

func TestDownloadImageRejectsUnfollowedRedirectWithRealHTTPClient(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/image", http.StatusFound)
	}))
	defer server.Close()
	b := New(Options{LogLevel: "silent"})
	b.client.HTTP = server.Client()
	b.client.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	data, err := b.DownloadImage(context.Background(), ImageContent{URL: server.URL + "/redirect"})
	if err == nil || len(data) != 0 || calls.Load() != 1 {
		t.Fatalf("unfollowed redirect accepted: bytes=%q err=%v calls=%d", data, err, calls.Load())
	}
}

func TestDownloadImageRejectsEncryptedRedirectResponse(t *testing.T) {
	key := []byte("0123456789abcdef")
	ciphertext, err := crypto.EncryptAESECB([]byte("image bytes"), key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/image")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write(ciphertext)
	}))
	defer server.Close()
	b := New(Options{LogLevel: "silent"})
	b.client.HTTP = server.Client()
	b.client.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	data, err := b.DownloadImage(context.Background(), ImageContent{Media: &CDNMedia{FullURL: server.URL, AESKey: crypto.EncodeAESKeyBase64(key)}})
	if err == nil || len(data) != 0 {
		t.Fatalf("encrypted redirect accepted: bytes=%q err=%v", data, err)
	}
}

type cancelAtImageEOF struct{ cancel context.CancelFunc }

func (r cancelAtImageEOF) Read(p []byte) (int, error) {
	r.cancel()
	return copy(p, "image"), io.EOF
}
func (cancelAtImageEOF) Close() error { return nil }

func TestDownloadImageCancellationAtResponseCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := New(Options{LogLevel: "silent"})
	b.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: cancelAtImageEOF{cancel}}, nil
	})}
	data, err := b.DownloadImage(ctx, ImageContent{URL: "https://native.invalid/image"})
	if !errors.Is(err, context.Canceled) || len(data) != 0 {
		t.Fatalf("completed canceled read accepted: data=%q err=%v", data, err)
	}
}

func TestDownloadImageUsesNativeClientWithoutEncryptedFallback(t *testing.T) {
	b := New(Options{LogLevel: "silent"})
	var urls []string
	b.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		urls = append(urls, r.URL.Path)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("image bytes"))}, nil
	})}
	data, err := b.DownloadImage(context.Background(), ImageContent{URL: "https://native.invalid/plain"})
	if err != nil || string(data) != "image bytes" || len(urls) != 1 || urls[0] != "/plain" {
		t.Fatal(string(data), err, urls)
	}
	_, err = b.DownloadImage(context.Background(), ImageContent{Media: &CDNMedia{FullURL: "https://native.invalid/encrypted"}, URL: "https://native.invalid/fallback"})
	if err == nil || len(urls) != 2 || urls[1] != "/encrypted" {
		t.Fatalf("invalid encrypted image must not use the plain URL: %v, %v", err, urls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = b.DownloadImage(ctx, ImageContent{URL: "https://native.invalid/canceled"}); !errors.Is(err, context.Canceled) || len(urls) != 2 {
		t.Fatal(err, urls)
	}
}
