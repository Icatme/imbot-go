package qqbot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/gateway"
)

func TestLateUnauthorizedDoesNotDiscardFreshToken(t *testing.T) {
	a := fixture(t)
	var fetches atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/app/getAppAccessToken" {
			return response(fmt.Sprintf(`{"access_token":"token-%d","expires_in":7200}`, fetches.Add(1))), nil
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		if body.Content == "slow" {
			close(started)
			<-release
		}
		if body.Content == "slow" || body.Content == "expire" {
			out := response(`{"code":401}`)
			out.StatusCode = http.StatusUnauthorized
			return out, nil
		}
		return response(`{"id":"sent"}`), nil
	})}
	send := func(text string) error {
		_, err := a.Send(context.Background(), route(a), gateway.Message{Text: text, ReplyTo: "m"})
		return err
	}
	done := make(chan error, 1)
	go func() { done <- send("slow") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request did not start")
	}
	if err := send("expire"); err == nil {
		t.Fatal("expected first 401")
	}
	if err := send("refresh"); err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 2 {
		t.Fatalf("token not refreshed: %d", fetches.Load())
	}
	unblock()
	if err := <-done; err == nil {
		t.Fatal("expected late 401")
	}
	if err := send("final"); err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 2 {
		t.Fatalf("late old-token rejection discarded fresh token: %d fetches", fetches.Load())
	}
}
