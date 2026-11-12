package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Icatme/imbot-go/internal/protocol"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type loginRequests struct {
	qrBody    []byte
	qrCalls   int
	pollCalls int
	polls     []loginPollRequest
}

type loginPollRequest struct {
	qrCode     string
	verifyCode string
}

func newLoginTestClient(t *testing.T, statusBodies ...string) (*protocol.Client, *loginRequests) {
	t.Helper()
	if len(statusBodies) == 0 {
		t.Fatal("missing QR status responses")
	}
	requests := &loginRequests{}
	client := protocol.NewClient()
	client.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var responseBody string
		switch req.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			requests.qrCalls++
			if req.Body != nil {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				requests.qrBody = body
			}
			responseBody = fmt.Sprintf(`{"qrcode":"qr-token-%d","qrcode_img_content":"https://example.com/qr"}`, requests.qrCalls)
		case "/ilink/bot/get_qrcode_status":
			requests.pollCalls++
			requests.polls = append(requests.polls, loginPollRequest{
				qrCode: req.URL.Query().Get("qrcode"), verifyCode: req.URL.Query().Get("verify_code"),
			})
			if len(statusBodies) == 1 {
				responseBody = statusBodies[0]
			} else if requests.pollCalls <= len(statusBodies) {
				responseBody = statusBodies[requests.pollCalls-1]
			} else {
				return nil, errors.New("unexpected extra QR status poll")
			}
		default:
			return nil, errors.New("unexpected auth request: " + req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    req,
		}, nil
	})}
	return client, requests
}

func assertNoLocalTokens(t *testing.T, body []byte) {
	t.Helper()
	if len(body) == 0 {
		return
	}
	var request struct {
		LocalTokens []string `json:"local_token_list"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode QR request: %v", err)
	}
	if len(request.LocalTokens) != 0 {
		t.Fatalf("forced login sent local tokens: %v", request.LocalTokens)
	}
}

func TestFinalizeLogin(t *testing.T) {
	dir := t.TempDir()
	status := &protocol.QRStatusResponse{
		BotToken: "bt",
		BotID:    "bid",
		UserID:   "uid",
		BaseURL:  "https://example.com",
	}
	path := filepath.Join(dir, "creds.json")
	creds, err := finalizeLogin(status, "https://default.com", path)
	if err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if creds.Token != "bt" || creds.AccountID != "bid" || creds.UserID != "uid" {
		t.Fatalf("credentials mismatch: %+v", creds)
	}
	if creds.BaseURL != "https://example.com" {
		t.Fatalf("expected base URL from status, got %s", creds.BaseURL)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("credentials file not saved")
	}
}

func TestFinalizeLoginFailsWhenCredentialsCannotBeSaved(t *testing.T) {
	status := &protocol.QRStatusResponse{
		BotToken: "bt",
		BotID:    "bid",
		UserID:   "uid",
	}
	creds, err := finalizeLogin(status, "https://default.com", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "save credentials") {
		t.Fatalf("expected persistence error, got credentials=%+v err=%v", creds, err)
	}
	if creds != nil {
		t.Fatalf("returned credentials after persistence failure: %+v", creds)
	}
}

func TestLoadCredentialsMissing(t *testing.T) {
	dir := t.TempDir()
	creds, err := LoadCredentials(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if creds != nil {
		t.Fatal("expected nil for missing file")
	}
}

func TestSaveCredentialsReplacesJSONWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("create credentials directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0644); err != nil {
		t.Fatalf("write old credentials: %v", err)
	}
	creds := &Credentials{Token: "secret", BaseURL: "https://example.com", AccountID: "account", UserID: "user"}

	if err := SaveCredentials(creds, path); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("credentials do not end in newline: %q", data)
	}
	var got Credentials
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	if got.Token != creds.Token || got.AccountID != creds.AccountID || got.UserID != creds.UserID {
		t.Fatalf("saved credentials = %+v, want %+v", got, *creds)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got&0077 != 0 {
		t.Fatalf("credentials permissions = %04o, want no group or other permissions", got)
	}
}

func TestClearCredentialsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if err := ClearCredentials(path); err != nil {
		t.Fatalf("missing credentials should already be clear: %v", err)
	}
}

func TestLoginNonForceReturnsStoredCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	want := &Credentials{Token: "stored-token", BaseURL: "https://stored.example", AccountID: "bot-1", UserID: "user-1"}
	if err := SaveCredentials(want, path); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	client := protocol.NewClient()
	requestCount := 0
	client.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestCount++
		return nil, errors.New("unexpected network request")
	})}

	got, err := Login(context.Background(), client, LoginOptions{CredPath: path})
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if requestCount != 0 {
		t.Fatalf("non-force login made %d network requests", requestCount)
	}
	if got.Token != want.Token || got.AccountID != want.AccountID || got.UserID != want.UserID {
		t.Fatalf("loaded credentials mismatch: %+v", got)
	}
}

func TestLoginForceDoesNotReuseStoredCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(&Credentials{Token: "invalid-token", AccountID: "bot-old", UserID: "user-old"}, path); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	client, requests := newLoginTestClient(t, `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://api.example"}`)

	got, err := Login(context.Background(), client, LoginOptions{
		CredPath:     path,
		Force:        true,
		InvalidToken: "invalid-token",
		OnQRURL:      func(string) {},
	})
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if got.Token != "fresh-token" {
		t.Fatalf("expected fresh credentials, got %+v", got)
	}
	if requests.qrCalls != 1 || requests.pollCalls != 1 {
		t.Fatalf("unexpected request counts: QR=%d poll=%d", requests.qrCalls, requests.pollCalls)
	}
	assertNoLocalTokens(t, requests.qrBody)
}

func TestLoginForceBindedRedirectDoesNotReuseStoredCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredentials(&Credentials{Token: "invalid-token", AccountID: "bot-old", UserID: "user-old"}, path); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	client, requests := newLoginTestClient(t, `{"status":"binded_redirect"}`)

	got, err := Login(context.Background(), client, LoginOptions{
		CredPath:     path,
		Force:        true,
		InvalidToken: "invalid-token",
		OnQRURL:      func(string) {},
	})
	if err == nil || !strings.Contains(err.Error(), "forced login cannot reuse existing credentials") {
		t.Fatalf("expected forced binded_redirect rejection, got credentials=%+v err=%v", got, err)
	}
	if got != nil {
		t.Fatalf("forced login returned stored credentials: %+v", got)
	}
	assertNoLocalTokens(t, requests.qrBody)
}

func TestLoginRejectsInvalidatedConfirmedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	client, _ := newLoginTestClient(t, `{"status":"confirmed","bot_token":"invalid-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1"}`)

	got, err := Login(context.Background(), client, LoginOptions{
		CredPath:     path,
		Force:        true,
		InvalidToken: "invalid-token",
		OnQRURL:      func(string) {},
	})
	if !errors.Is(err, ErrInvalidatedCredential) {
		t.Fatalf("expected ErrInvalidatedCredential, got credentials=%+v err=%v", got, err)
	}
	if got != nil {
		t.Fatalf("invalidated credentials returned: %+v", got)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("invalidated credentials were persisted: %v", statErr)
	}
}

func TestLoginVerifyCodeStates(t *testing.T) {
	const confirmed = `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1"}`
	tests := []struct {
		name        string
		statuses    []string
		codes       []string
		wantRetries []bool
		wantPolls   []loginPollRequest
		wantQRCalls int
	}{
		{
			name:     "rejected code requests another input",
			statuses: []string{`{"status":"need_verifycode"}`, `{"status":"need_verifycode"}`, confirmed},
			codes:    []string{" 123456\n", "654321"}, wantRetries: []bool{false, true}, wantQRCalls: 1,
			wantPolls: []loginPollRequest{{"qr-token-1", ""}, {"qr-token-1", "123456"}, {"qr-token-1", "654321"}},
		},
		{
			name:     "blocked code resets on a fresh QR",
			statuses: []string{`{"status":"need_verifycode"}`, `{"status":"verify_code_blocked"}`, `{"status":"need_verifycode"}`, confirmed},
			codes:    []string{"123456", "654321"}, wantRetries: []bool{false, false}, wantQRCalls: 2,
			wantPolls: []loginPollRequest{{"qr-token-1", ""}, {"qr-token-1", "123456"}, {"qr-token-2", ""}, {"qr-token-2", "654321"}},
		},
		{
			name:     "accepted code clears before a new challenge",
			statuses: []string{`{"status":"need_verifycode"}`, `{"status":"scaned"}`, `{"status":"need_verifycode"}`, confirmed},
			codes:    []string{"123456", "654321"}, wantRetries: []bool{false, false}, wantQRCalls: 1,
			wantPolls: []loginPollRequest{{"qr-token-1", ""}, {"qr-token-1", "123456"}, {"qr-token-1", ""}, {"qr-token-1", "654321"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := newLoginTestClient(t, tc.statuses...)
			path := filepath.Join(t.TempDir(), "credentials.json")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var retries []bool
			got, err := Login(ctx, client, LoginOptions{
				CredPath: path, OnQRURL: func(string) {}, OnScanned: func() {},
				OnVerifyCode: func(inputCtx context.Context, isRetry bool) (string, error) {
					if inputCtx != ctx {
						return "", errors.New("input callback did not receive the login context")
					}
					if len(retries) >= len(tc.codes) {
						return "", errors.New("unexpected extra verify-code callback")
					}
					code := tc.codes[len(retries)]
					retries = append(retries, isRetry)
					return code, nil
				},
			})
			if err != nil || got == nil || got.Token != "fresh-token" {
				t.Fatalf("login did not complete: credentials=%+v err=%v", got, err)
			}
			if !slices.Equal(retries, tc.wantRetries) || !slices.Equal(requests.polls, tc.wantPolls) || requests.qrCalls != tc.wantQRCalls {
				t.Fatalf("flow mismatch: retries=%v polls=%v QR calls=%d", retries, requests.polls, requests.qrCalls)
			}
			stored, err := LoadCredentials(path)
			if err != nil || stored == nil || stored.Token != got.Token {
				t.Fatalf("confirmed credentials not persisted: credentials=%+v err=%v", stored, err)
			}
			assertNoLocalTokens(t, requests.qrBody)
		})
	}
}

func TestLoginVerifyCodeInputFailure(t *testing.T) {
	inputErr := errors.New("input closed")
	tests := []struct {
		name     string
		callback func(context.Context, bool) (string, error)
		wantText string
		wantErr  error
	}{
		{name: "missing callback", wantText: "no OnVerifyCode handler"},
		{name: "callback failure", callback: func(context.Context, bool) (string, error) { return "", inputErr }, wantText: "verify code input failed", wantErr: inputErr},
		{name: "empty input", callback: func(context.Context, bool) (string, error) { return " \t\n", nil }, wantText: "verify code cannot be empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := newLoginTestClient(t, `{"status":"need_verifycode"}`)
			path := filepath.Join(t.TempDir(), "credentials.json")
			got, err := Login(context.Background(), client, LoginOptions{CredPath: path, OnQRURL: func(string) {}, OnVerifyCode: tc.callback})
			if got != nil || err == nil || !strings.Contains(err.Error(), tc.wantText) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("unexpected input failure: credentials=%+v err=%v", got, err)
			}
			if requests.qrCalls != 1 || requests.pollCalls != 1 {
				t.Fatalf("input failure continued authentication: %+v", requests)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("input failure persisted credentials: %v", err)
			}
		})
	}
}

func TestLoginCancellationWhileWaitingForVerifyCode(t *testing.T) {
	client, requests := newLoginTestClient(t, `{"status":"need_verifycode"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	path := filepath.Join(t.TempDir(), "credentials.json")
	go func() {
		_, err := Login(ctx, client, LoginOptions{
			CredPath: path, OnQRURL: func(string) {},
			OnVerifyCode: func(inputCtx context.Context, isRetry bool) (string, error) {
				close(entered)
				<-inputCtx.Done()
				return "", inputCtx.Err()
			},
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("verify-code callback did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("login did not preserve cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("login did not stop after verify-code input was canceled")
	}
	if requests.pollCalls != 1 {
		t.Fatalf("canceled input continued polling: %d calls", requests.pollCalls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("canceled input persisted credentials: %v", err)
	}
}

func TestLoginDoesNotSubmitCodeReturnedAfterCancellation(t *testing.T) {
	client, requests := newLoginTestClient(t, `{"status":"need_verifycode"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := Login(ctx, client, LoginOptions{
		CredPath: filepath.Join(t.TempDir(), "credentials.json"), OnQRURL: func(string) {},
		OnVerifyCode: func(context.Context, bool) (string, error) {
			cancel()
			return "123456", nil
		},
	})
	if got != nil || !errors.Is(err, context.Canceled) || requests.pollCalls != 1 {
		t.Fatalf("submitted a code after cancellation: credentials=%+v err=%v polls=%d", got, err, requests.pollCalls)
	}
}

func TestLoginCancellationDuringPollInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	synctest.Test(t, func(t *testing.T) {
		client, requests := newLoginTestClient(t, `{"status":"wait"}`)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := Login(ctx, client, LoginOptions{CredPath: path, OnQRURL: func(string) {}})
			done <- err
		}()

		// Let Login finish decoding/checking the response and block on its poll
		// interval before canceling. An HTTP-completion signal is too early.
		synctest.Wait()
		if requests.qrCalls != 1 || requests.pollCalls != 1 {
			t.Fatalf("login did not reach its first poll interval: %+v", requests)
		}
		select {
		case err := <-done:
			t.Fatalf("login exited before cancellation: %v", err)
		default:
		}

		canceledAt := time.Now()
		cancel()
		// Receiving also drains the login goroutine if a regression requires
		// the fake clock to advance through an uncancellable sleep.
		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("login did not preserve cancellation: %v", err)
		}
		if elapsed := time.Since(canceledAt); elapsed != 0 {
			t.Fatalf("login waited %v after cancellation", elapsed)
		}
		if requests.qrCalls != 1 || requests.pollCalls != 1 {
			t.Fatalf("canceled login continued requests: %+v", requests)
		}
	})
}

func TestLoginBlockedVerifyCodeHonorsQRRefreshLimit(t *testing.T) {
	client, requests := newLoginTestClient(t, `{"status":"verify_code_blocked"}`)
	got, err := Login(context.Background(), client, LoginOptions{CredPath: filepath.Join(t.TempDir(), "credentials.json"), OnQRURL: func(string) {}})
	if got != nil || err == nil || !strings.Contains(err.Error(), "after 3 attempts") || requests.qrCalls != 3 || requests.pollCalls != 3 {
		t.Fatalf("unexpected blocked-code limit: credentials=%+v err=%v requests=%+v", got, err, requests)
	}
}

func TestLoginCorruptCredentialsStopsBeforeQR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	const original = "invalid credentials JSON\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	client, requests := newLoginTestClient(t, `{"status":"confirmed"}`)
	got, err := Login(context.Background(), client, LoginOptions{CredPath: path, OnQRURL: func(string) {}})
	if got != nil || err == nil || !strings.Contains(err.Error(), "load credentials") || requests.qrCalls != 0 {
		t.Fatalf("corrupt credentials started QR login: credentials=%+v err=%v QR calls=%d", got, err, requests.qrCalls)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != original {
		t.Fatalf("corrupt credentials were replaced: data=%q err=%v", data, err)
	}
}

func TestLoginBindedRedirectWithoutCredentials(t *testing.T) {
	client, requests := newLoginTestClient(t, `{"status":"binded_redirect"}`)
	got, err := Login(context.Background(), client, LoginOptions{CredPath: filepath.Join(t.TempDir(), "credentials.json"), OnQRURL: func(string) {}})
	if got != nil || err == nil || !strings.Contains(err.Error(), "no local credentials") {
		t.Fatalf("unexpected bound-account result: credentials=%+v err=%v", got, err)
	}
	assertNoLocalTokens(t, requests.qrBody)
}
