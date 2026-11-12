package qqbot

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
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Icatme/imbot-go/channels/onboarding"
	"github.com/Icatme/imbot-go/gateway"
)

type registrationTransport func(*http.Request) (*http.Response, error)

func (f registrationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func registrationResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func encryptRegistrationSecret(t *testing.T, key, plain []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := bytes.Repeat([]byte{9}, gcm.NonceSize())
	return base64.StdEncoding.EncodeToString(gcm.Seal(iv, iv, plain, nil))
}

func registrationClient(t *testing.T, poll func(*http.Request, []byte) (*http.Response, error)) *http.Client {
	t.Helper()
	var key []byte
	return &http.Client{Transport: registrationTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("unexpected registration method or content type")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/lite/create_bind_task":
			var err error
			key, err = base64.StdEncoding.DecodeString(body["key"])
			if err != nil || len(key) != 32 || len(body) != 1 {
				t.Fatal("expected one random AES-256 key")
			}
			return registrationResponse(200, `{"retcode":0,"data":{"task_id":"task&?+id"}}`), nil
		case "/lite/poll_bind_result":
			if body["task_id"] != "task&?+id" || len(body) != 1 {
				t.Fatal("polling leaked key or changed task id")
			}
			return poll(r, key)
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
			return nil, nil
		}
	})}
}

func completedRegistration(t *testing.T, key []byte, id any) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"retcode": 0, "data": map[string]any{
		"status": 2, "bot_appid": id, "bot_encrypt_secret": encryptRegistrationSecret(t, key, []byte("app-secret")), "user_openid": "scanning-user",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registrationResponse(200, string(body))
}

func TestRegistrationHTTPContract(t *testing.T) {
	var calls atomic.Int32
	var key []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect HTTP registration contract")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/lite/create_bind_task":
			var err error
			key, err = base64.StdEncoding.DecodeString(body["key"])
			if err != nil || len(key) != 32 || len(body) != 1 {
				t.Error("incorrect registration key")
			}
			io.WriteString(w, `{"retcode":0,"data":{"task_id":"task&?+id"}}`)
		case "/lite/poll_bind_result":
			if len(body) != 1 || body["task_id"] != "task&?+id" {
				t.Error("incorrect polling body")
			}
			response := completedRegistration(t, key, "12345")
			defer response.Body.Close()
			io.Copy(w, response.Body)
		default:
			http.Error(w, "unexpected request", 404)
		}
	}))
	defer srv.Close()
	reg, err := BeginRegistration(context.Background(), RegistrationOptions{BaseURL: srv.URL, Source: "my platform&?"})
	if err != nil {
		t.Fatal(err)
	}
	challenge := reg.Challenge()
	u, err := url.Parse(challenge.URL)
	if err != nil || u.Scheme != "https" || u.Host != "q.qq.com" || u.Path != "/qqbot/openclaw/connect.html" || u.Query().Get("task_id") != "task&?+id" || u.Query().Get("source") != "my platform&?" || u.Query().Get("_wv") != "2" || len(u.Query()) != 3 {
		t.Fatal("incorrect official authorization URL")
	}
	if challenge.PollInterval != 2*time.Second {
		t.Fatal("incorrect polling interval")
	}
	creds, err := reg.Wait(context.Background())
	if err != nil || creds != (Credentials{AppID: "12345", ClientSecret: "app-secret", UserOpenID: "scanning-user"}) || calls.Load() != 2 {
		t.Fatalf("registration did not decrypt credentials: %v, calls=%d", err, calls.Load())
	}
	cfg := creds.Config("local-account")
	if cfg.AccountID != "local-account" || cfg.AppID != creds.AppID || cfg.ClientSecret != creds.ClientSecret || cfg.EnableProactive || cfg.EnableMarkdown {
		t.Fatal("Config changed message permissions")
	}
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationPendingAndLosslessNumericID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var times []time.Time
		client := registrationClient(t, func(r *http.Request, key []byte) (*http.Response, error) {
			times = append(times, time.Now())
			switch len(times) {
			case 1:
				return registrationResponse(200, `{"retcode":0,"data":{"status":0}}`), nil
			case 2:
				return registrationResponse(200, `{"retcode":0,"data":{"status":1}}`), nil
			default:
				return completedRegistration(t, key, json.Number("9007199254740993")), nil
			}
		})
		start := time.Now()
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(reg.Challenge().URL)
		if u.Query().Get("source") != "" || u.Query().Has("key") || reg.Challenge().ExpiresAt.Sub(start) != 10*time.Minute {
			t.Fatal("unexpected default source/key/timeout")
		}
		creds, err := reg.Wait(context.Background())
		if err != nil || creds.AppID != "9007199254740993" || len(times) != 3 {
			t.Fatalf("lossless app ID not preserved: %v", err)
		}
		for i, observed := range times {
			if observed.Sub(start) != time.Duration(i+1)*2*time.Second {
				t.Fatalf("incorrect polling schedule: %v", times)
			}
		}
		if _, err := reg.Wait(context.Background()); err != nil || len(times) != 3 {
			t.Fatal("completed task was polled again")
		}
	})
}

func TestRegistrationEncryptedSecretInteroperability(t *testing.T) {
	// Produced independently with node:crypto aes-256-gcm (the official SDK's
	// crypto API): key=32 bytes of 7, IV=12 bytes of 9, text="qq-test-secret".
	const encoded = "CQkJCQkJCQkJCQkJVvSp4NuDtUzTB6hKg56K0wzIxtb/6OG9WRUzMO+A"
	key := bytes.Repeat([]byte{7}, 32)
	if got, err := decryptRegistrationSecret(key, encoded); err != nil || got != "qq-test-secret" {
		t.Fatalf("official crypto framing mismatch: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(encoded)
	for _, offset := range []int{0, 12, len(raw) - 1} {
		tampered := append([]byte(nil), raw...)
		tampered[offset] ^= 1
		if got, err := decryptRegistrationSecret(key, base64.StdEncoding.EncodeToString(tampered)); !errors.Is(err, onboarding.ErrInvalidResponse) || got != "" {
			t.Fatal("tampered ciphertext accepted")
		}
	}
	for _, invalid := range []string{"", "not-base64", base64.StdEncoding.EncodeToString(make([]byte, 28)), encryptRegistrationSecret(t, key, []byte{}), encryptRegistrationSecret(t, key, []byte("  ")), encryptRegistrationSecret(t, key, []byte{0xff, 0xfe})} {
		if got, err := decryptRegistrationSecret(key, invalid); !errors.Is(err, onboarding.ErrInvalidResponse) || got != "" {
			t.Fatal("invalid secret accepted")
		}
	}
}

func TestRegistrationRejectsMalformedAndExpiredResults(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"missing_retcode", `{"data":{"status":1}}`, onboarding.ErrInvalidResponse},
		{"null_retcode", `{"retcode":null,"data":{"status":1}}`, onboarding.ErrInvalidResponse},
		{"missing_data", `{"retcode":0}`, onboarding.ErrInvalidResponse},
		{"missing_status", `{"retcode":0,"data":{}}`, onboarding.ErrInvalidResponse},
		{"null_status", `{"retcode":0,"data":{"status":null}}`, onboarding.ErrInvalidResponse},
		{"unknown_status", `{"retcode":0,"data":{"status":4}}`, onboarding.ErrInvalidResponse},
		{"expired", `{"retcode":0,"data":{"status":3}}`, onboarding.ErrExpired},
		{"missing_id", `{"retcode":0,"data":{"status":2,"bot_encrypt_secret":"private-ciphertext"}}`, onboarding.ErrInvalidResponse},
		{"invalid_id", `{"retcode":0,"data":{"status":2,"bot_appid":{},"bot_encrypt_secret":"private-ciphertext"}}`, onboarding.ErrInvalidResponse},
		{"missing_secret", `{"retcode":0,"data":{"status":2,"bot_appid":"123"}}`, onboarding.ErrInvalidResponse},
		{"bad_secret", `{"retcode":0,"data":{"status":2,"bot_appid":"123","bot_encrypt_secret":"private-ciphertext"}}`, onboarding.ErrInvalidResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := registrationClient(t, func(*http.Request, []byte) (*http.Response, error) {
					calls++
					return registrationResponse(200, tc.body), nil
				})
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					creds, err := reg.Wait(context.Background())
					if !errors.Is(err, tc.want) || creds != (Credentials{}) || strings.Contains(err.Error(), "private-ciphertext") {
						t.Fatalf("incorrect failure: %v", err)
					}
				}
				if calls != 1 {
					t.Fatal("terminal failure retried")
				}
			})
		})
	}
}

func TestRegistrationRetcodeErrorRedactedWithoutRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := registrationClient(t, func(*http.Request, []byte) (*http.Response, error) {
			calls++
			return registrationResponse(200, `{"retcode":123,"msg":"private-key task&?+id private-ciphertext"}`), nil
		})
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		_, err = reg.Wait(context.Background())
		var api *gateway.APIError
		if !errors.As(err, &api) || api.Code != "123" || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "task") || calls != 1 {
			t.Fatalf("retcode lost or server message leaked: %v", err)
		}
	})
}

func TestRegistrationCancellationAndExplicitResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		client := registrationClient(t, func(r *http.Request, key []byte) (*http.Response, error) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return completedRegistration(t, key, "123"), nil
		})
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := reg.Wait(ctx); done <- err }()
		time.Sleep(3 * time.Second)
		if _, err := reg.Wait(context.Background()); !errors.Is(err, onboarding.ErrConcurrentWait) {
			t.Fatalf("concurrent wait accepted: %v", err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation: %v", err)
		}
		if _, err := reg.Wait(context.Background()); err != nil || calls.Load() != 2 {
			t.Fatalf("explicit resume failed: %v", err)
		}
	})
}

func TestRegistrationLocalTimeoutDoesNotRefreshTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := registrationClient(t, func(*http.Request, []byte) (*http.Response, error) {
			calls++
			return registrationResponse(200, `{"retcode":0,"data":{"status":1}}`), nil
		})
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if _, err := reg.Wait(context.Background()); !errors.Is(err, onboarding.ErrExpired) || time.Since(start) != 5*time.Second || calls != 2 {
			t.Fatalf("local timeout not enforced: %v, calls=%d", err, calls)
		}
	})
}

func TestRegistrationRejectsOptionsBeforeNetwork(t *testing.T) {
	for _, opts := range []RegistrationOptions{{Timeout: -1}, {Timeout: time.Duration(-1 << 63)}, {BaseURL: "http://evil.test"}, {BaseURL: "https://user:secret@q.qq.com"}, {BaseURL: "wss://q.qq.com"}, {BaseURL: "https://q.qq.com?query=value"}, {BaseURL: "https://q.qq.com?"}} {
		opts.HTTPClient = &http.Client{Transport: registrationTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid options reached network")
			return nil, nil
		})}
		if _, err := BeginRegistration(context.Background(), opts); !errors.Is(err, onboarding.ErrInvalidResponse) {
			t.Fatalf("invalid options accepted: %v", err)
		}
	}
}

func TestRegistrationLongerLocalTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := registrationClient(t, func(*http.Request, []byte) (*http.Response, error) {
			t.Fatal("BeginRegistration unexpectedly polled")
			return nil, nil
		})
		start := time.Now()
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client, Timeout: 2 * time.Hour})
		if err != nil || reg.Challenge().ExpiresAt.Sub(start) != 2*time.Hour {
			t.Fatalf("custom local timeout rejected: %v", err)
		}
	})
}

func TestRegistrationTransportErrorRequiresExplicitResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := registrationClient(t, func(r *http.Request, key []byte) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("private-key task&?+id private-ciphertext")
			}
			return completedRegistration(t, key, "123"), nil
		})
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		_, err = reg.Wait(context.Background())
		var api *gateway.APIError
		if !errors.As(err, &api) || api.Code != "registration_transport" || calls != 1 || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "task") {
			t.Fatalf("transport failure retried or leaked: %v", err)
		}
		if _, err := reg.Wait(context.Background()); err != nil || calls != 2 {
			t.Fatalf("explicit resume failed: %v", err)
		}
	})
}

func TestRegistrationCreateValidationAndFreshKeys(t *testing.T) {
	for _, body := range []string{`{}`, `{"retcode":null}`, `{"retcode":0}`, `{"retcode":0,"data":{}}`, `{"retcode":0,"data":{"task_id":" "}}`, `{"retcode":0,"data":{"task_id":123}}`} {
		client := &http.Client{Transport: registrationTransport(func(r *http.Request) (*http.Response, error) {
			return registrationResponse(200, body), nil
		})}
		if _, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client}); !errors.Is(err, onboarding.ErrInvalidResponse) {
			t.Fatalf("malformed create response accepted: %v", err)
		}
	}
	var keys []string
	client := &http.Client{Transport: registrationTransport(func(r *http.Request) (*http.Response, error) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, body["key"])
		return registrationResponse(200, `{"retcode":0,"data":{"task_id":"task"}}`), nil
	})}
	for i := 0; i < 2; i++ {
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(reg)
		if strings.Contains(string(encoded), keys[i]) || strings.Contains(reg.Challenge().URL, keys[i]) {
			t.Fatal("private key exposed outside registration request")
		}
	}
	if keys[0] == keys[1] || keys[0] == "" {
		t.Fatal("registration reused its key")
	}
}
