package wechatbot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestCanceledPairingKeepsReauthenticationGatedUntilFreshLogin(t *testing.T) {
	bot, paths := newAuthenticatedSessionTestBot(t, "https://old.example")
	bot.opts.OnQRURL = func(string) {}
	pollCalls := 0
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			body = `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		case "/ilink/bot/get_qrcode_status":
			pollCalls++
			if req.URL.Query().Get("verify_code") == "" {
				body = `{"status":"need_verifycode"}`
			} else if req.URL.Query().Get("verify_code") == "654321" {
				body = `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://fresh.example"}`
			} else {
				return nil, errors.New("unexpected pairing code submission")
			}
		default:
			return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot.opts.OnVerifyCode = func(inputCtx context.Context, isRetry bool) (string, error) {
		if inputCtx != ctx || isRetry {
			return "", errors.New("incorrect initial pairing callback context or retry state")
		}
		cancel()
		return "123456", nil
	}
	creds, err := bot.Reauthenticate(ctx)
	if creds != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrReauthRequired) || pollCalls != 1 {
		t.Fatalf("canceled pairing result: credentials=%+v err=%v polls=%d", creds, err, pollCalls)
	}
	if _, err := bot.readyCreds(); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("canceled pairing reopened the old session: %v", err)
	}
	if _, err := os.Stat(paths.credentials); !os.IsNotExist(err) {
		t.Fatalf("canceled pairing retained credentials: %v", err)
	}
	if _, err := os.Stat(bot.reauthStore.Path()); err != nil {
		t.Fatalf("canceled pairing removed its durable marker: %v", err)
	}
	if _, err := bot.Login(context.Background(), false); !errors.Is(err, ErrReauthRequired) || pollCalls != 1 {
		t.Fatalf("ordinary login bypassed canceled reauthentication: err=%v polls=%d", err, pollCalls)
	}

	recoveryCtx := context.Background()
	bot.opts.OnVerifyCode = func(inputCtx context.Context, isRetry bool) (string, error) {
		if inputCtx != recoveryCtx || isRetry {
			return "", errors.New("fresh pairing inherited canceled context or retry state")
		}
		if _, err := bot.Reauthenticate(inputCtx); !errors.Is(err, ErrLoginInProgress) {
			return "", fmt.Errorf("reentrant pairing error: %w", err)
		}
		return "654321", nil
	}
	creds, err = bot.Reauthenticate(recoveryCtx)
	if err != nil || creds == nil || creds.Token != "fresh-token" || pollCalls != 3 {
		t.Fatalf("fresh pairing did not recover: credentials=%+v err=%v polls=%d", creds, err, pollCalls)
	}
	if _, err := bot.readyCreds(); err != nil || bot.ReauthRequired() {
		t.Fatalf("fresh pairing did not publish its session: %v", err)
	}
	if _, err := os.Stat(bot.reauthStore.Path()); !os.IsNotExist(err) {
		t.Fatalf("successful pairing retained its durable marker: %v", err)
	}
}
