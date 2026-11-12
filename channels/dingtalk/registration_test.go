package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Icatme/imbot-go/channels/onboarding"
	"github.com/Icatme/imbot-go/gateway"
)

type registrationRoundTripper func(*http.Request) (*http.Response, error)

func (f registrationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func registrationResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type registrationFixture struct {
	t          *testing.T
	begin      string
	polls      []string
	pollStatus int
	pollCount  int
}

func (f *registrationFixture) client() *http.Client {
	return &http.Client{Transport: registrationRoundTripper(func(r *http.Request) (*http.Response, error) {
		f.t.Helper()
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			f.t.Fatalf("wrong request: %s %v", r.Method, r.Header)
		}
		switch r.URL.Path {
		case "/app/registration/init":
			if !reflect.DeepEqual(body, map[string]string{"source": "DING_DWS_CLAW"}) {
				f.t.Fatalf("wrong init payload: %v", body)
			}
			return registrationResponse(200, `{"errcode":0,"nonce":"private-nonce"}`), nil
		case "/app/registration/begin":
			if !reflect.DeepEqual(body, map[string]string{"nonce": "private-nonce"}) {
				f.t.Fatalf("wrong begin payload: %v", body)
			}
			begin := f.begin
			if begin == "" {
				begin = `{"errcode":0,"device_code":"private-device-code","user_code":"shown-to-user","verification_uri_complete":"https://login.dingtalk.com/authorize?code=scan","expires_in":30,"interval":1}`
			}
			return registrationResponse(200, begin), nil
		case "/app/registration/poll":
			if !reflect.DeepEqual(body, map[string]string{"device_code": "private-device-code"}) {
				f.t.Fatalf("wrong poll payload: %v", body)
			}
			if f.pollCount >= len(f.polls) {
				f.t.Fatalf("unexpected additional poll %d", f.pollCount+1)
			}
			out := f.polls[f.pollCount]
			f.pollCount++
			status := f.pollStatus
			if status == 0 {
				status = 200
			}
			return registrationResponse(status, out), nil
		default:
			f.t.Fatalf("unexpected path: %s", r.URL.Path)
			return nil, errors.New("unexpected path")
		}
	})}
}

func TestRegistrationBeginWireContract(t *testing.T) {
	for _, source := range []string{"", "registered-template"} {
		t.Run(source, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("wrong request: %s %v", r.Method, r.Header)
				}
				switch r.URL.Path {
				case "/app/registration/init":
					want := source
					if want == "" {
						want = "DING_DWS_CLAW"
					}
					if !reflect.DeepEqual(body, map[string]string{"source": want}) {
						t.Errorf("init body = %v", body)
					}
					fmt.Fprint(w, `{"errcode":0,"nonce":"private-nonce"}`)
				case "/app/registration/begin":
					if !reflect.DeepEqual(body, map[string]string{"nonce": "private-nonce"}) {
						t.Errorf("begin body = %v", body)
					}
					fmt.Fprint(w, `{"errcode":0,"device_code":"private-device-code","verification_uri_complete":"https://login.dingtalk.com/authorize?code=scan"}`)
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			before := time.Now()
			reg, err := BeginRegistration(context.Background(), RegistrationOptions{BaseURL: srv.URL, Source: source})
			if err != nil {
				t.Fatal(err)
			}
			c := reg.Challenge()
			if calls != 2 || c.URL != "https://login.dingtalk.com/authorize?code=scan" || c.PollInterval != 3*time.Second || c.ExpiresAt.Before(before.Add(2*time.Hour)) || c.ExpiresAt.After(time.Now().Add(2*time.Hour)) {
				t.Fatalf("wrong challenge: %+v, calls=%d", c, calls)
			}
			encoded, err := json.Marshal(reg)
			if err != nil || string(encoded) != "{}" {
				t.Fatalf("registration exposes polling state: %s %v", encoded, err)
			}
		})
	}
}

func TestRegistrationWaitAndAdapterConfig(t *testing.T) {
	for _, pending := range []string{"WAITING", "SCANNED", " scanned "} {
		t.Run(pending, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &registrationFixture{t: t, polls: []string{fmt.Sprintf(`{"errcode":0,"status":%q}`, pending), `{"errcode":0,"status":"SUCCESS","client_id":"app","client_secret":"secret"}`}}
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				credentials, err := reg.Wait(context.Background())
				if err != nil || credentials.ClientID != "app" || credentials.ClientSecret != "secret" || time.Since(start) != 2*time.Second || f.pollCount != 2 {
					t.Fatalf("registration failed: calls=%d elapsed=%s err=%v", f.pollCount, time.Since(start), err)
				}
				again, err := reg.Wait(context.Background())
				if err != nil || again != credentials || f.pollCount != 2 {
					t.Fatal("successful result was not retained")
				}
				cfg := credentials.Config("local-account")
				adapter, err := New(cfg)
				if err != nil || cfg.RobotCode != "app" || adapter.Account().ID != "local-account" {
					t.Fatalf("credentials do not configure adapter: %v", err)
				}
			})
		})
	}
}

func TestRegistrationPreservesOpaqueValues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const nonce = "\tprivate-nonce \n"
		const deviceCode = " private-device-code\t\n"
		want := Credentials{ClientID: "\tclient-id ", ClientSecret: " private-secret\n"}
		var receivedNonce, receivedDeviceCode string
		client := &http.Client{Transport: registrationRoundTripper(func(r *http.Request) (*http.Response, error) {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			var response any
			switch r.URL.Path {
			case "/app/registration/init":
				response = map[string]any{"errcode": 0, "nonce": nonce}
			case "/app/registration/begin":
				receivedNonce = body["nonce"]
				response = map[string]any{"errcode": 0, "device_code": deviceCode, "verification_uri_complete": "https://login.dingtalk.com/auth", "interval": 1, "expires_in": 30}
			case "/app/registration/poll":
				receivedDeviceCode = body["device_code"]
				response = map[string]any{"errcode": 0, "status": "SUCCESS", "client_id": want.ClientID, "client_secret": want.ClientSecret}
			default:
				return nil, errors.New("unexpected registration request")
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				return nil, err
			}
			return registrationResponse(200, string(encoded)), nil
		})}
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		got, err := reg.Wait(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if receivedNonce != nonce {
			t.Error("begin request changed opaque nonce bytes")
		}
		if receivedDeviceCode != deviceCode {
			t.Error("poll request changed opaque device code bytes")
		}
		if got != want {
			t.Error("registration changed opaque credential bytes")
		}
		cfg := got.Config("local-account")
		if cfg.ClientID != want.ClientID || cfg.ClientSecret != want.ClientSecret || cfg.RobotCode != want.ClientID {
			t.Error("adapter config changed opaque credential bytes")
		}
	})
}

func TestRegistrationTerminalResponses(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		want       error
	}{
		{"denied", `{"errcode":0,"status":"FAIL","fail_reason":"secret-private-device-code"}`, onboarding.ErrDenied},
		{"expired", `{"errcode":0,"status":"EXPIRED"}`, onboarding.ErrExpired},
		{"unknown", `{"errcode":0,"status":"secret-unknown"}`, onboarding.ErrInvalidResponse},
		{"missing-status", `{"errcode":0}`, onboarding.ErrInvalidResponse},
		{"missing-code", `{"status":"SUCCESS","client_id":"app","client_secret":"secret"}`, onboarding.ErrInvalidResponse},
		{"null-code", `{"errcode":null,"status":"WAITING"}`, onboarding.ErrInvalidResponse},
		{"missing-credentials", `{"errcode":0,"status":"SUCCESS","client_id":"app"}`, onboarding.ErrInvalidResponse},
		{"blank-client-id", `{"errcode":0,"status":"SUCCESS","client_id":" \t","client_secret":"secret"}`, onboarding.ErrInvalidResponse},
		{"blank-client-secret", `{"errcode":0,"status":"SUCCESS","client_id":"app","client_secret":" \n"}`, onboarding.ErrInvalidResponse},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &registrationFixture{t: t, polls: []string{`{"errcode":0,"status":"SCANNED"}`, tt.body}}
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					got, err := reg.Wait(context.Background())
					if !errors.Is(err, tt.want) || got != (Credentials{}) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-device-code") {
						t.Fatalf("wrong terminal outcome: %v", err)
					}
				}
				if f.pollCount != 2 {
					t.Fatal("terminal outcome caused another poll")
				}
			})
		})
	}
}

func TestRegistrationRejectsMalformedChallenge(t *testing.T) {
	base := map[string]any{"errcode": 0, "device_code": "private-device-code", "verification_uri_complete": "https://login.dingtalk.com/authorize", "expires_in": 30, "interval": 1}
	for _, tt := range []struct {
		name, field string
		value       any
	}{
		{"zero-expiry", "expires_in", 0}, {"negative-expiry", "expires_in", -1}, {"null-expiry", "expires_in", nil},
		{"fractional-expiry", "expires_in", 1.5}, {"string-expiry", "expires_in", "30"}, {"overflow-expiry", "expires_in", int64(1 << 62)},
		{"zero-interval", "interval", 0}, {"null-interval", "interval", nil}, {"fractional-interval", "interval", 1.5}, {"long-interval", "interval", 31},
		{"empty-device-code", "device_code", " "}, {"null-code", "errcode", nil},
		{"insecure-url", "verification_uri_complete", "http://remote.invalid/auth"},
		{"websocket-url", "verification_uri_complete", "wss://login.dingtalk.com/auth"},
		{"credential-url", "verification_uri_complete", "https://user:secret@login.dingtalk.com/auth"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := make(map[string]any, len(base))
			for k, v := range base {
				body[k] = v
			}
			body[tt.field] = tt.value
			raw, _ := json.Marshal(body)
			f := &registrationFixture{t: t, begin: string(raw)}
			if _, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()}); !errors.Is(err, onboarding.ErrInvalidResponse) {
				t.Fatalf("malformed challenge accepted: %v", err)
			}
		})
	}
}

func TestRegistrationCancellationAndResume(t *testing.T) {
	for _, scanned := range []bool{false, true} {
		t.Run(fmt.Sprintf("scanned=%t", scanned), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &registrationFixture{t: t, polls: []string{`{"errcode":0,"status":"SUCCESS","client_id":"app","client_secret":"secret"}`}}
				wantPolls := 0
				if scanned {
					f.polls = append([]string{`{"errcode":0,"status":"SCANNED"}`}, f.polls...)
					wantPolls = 1
				}
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { _, err := reg.Wait(ctx); done <- err }()
				synctest.Wait()
				if scanned {
					time.Sleep(time.Second)
					synctest.Wait()
				}
				if _, err := reg.Wait(context.Background()); !errors.Is(err, onboarding.ErrConcurrentWait) {
					t.Fatalf("concurrent Wait admitted: %v", err)
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) || f.pollCount != wantPolls {
					t.Fatalf("canceled waiter made a request: %v", err)
				}
				if _, err := reg.Wait(context.Background()); err != nil || f.pollCount != wantPolls+1 {
					t.Fatalf("explicit resume failed: %v", err)
				}
			})
		})
	}
}

func TestRegistrationServerDeadline(t *testing.T) {
	for _, pending := range []string{"WAITING", "SCANNED"} {
		t.Run(pending, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &registrationFixture{t: t, begin: `{"errcode":0,"device_code":"private-device-code","verification_uri_complete":"https://login.dingtalk.com/auth","expires_in":2,"interval":1}`, polls: []string{fmt.Sprintf(`{"errcode":0,"status":%q}`, pending)}}
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				if _, err := reg.Wait(context.Background()); !errors.Is(err, onboarding.ErrExpired) || time.Since(start) != 2*time.Second || f.pollCount != 1 {
					t.Fatalf("server deadline not enforced: polls=%d err=%v", f.pollCount, err)
				}
			})
		})
	}
}

func TestRegistrationPollErrorDoesNotRetryOrLeak(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &registrationFixture{t: t, pollStatus: status, polls: []string{`{"errcode":1234,"errmsg":"secret-private-device-code"}`}}
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				_, err = reg.Wait(context.Background())
				var api *gateway.APIError
				if !errors.As(err, &api) || strings.Contains(err.Error(), "secret") || f.pollCount != 1 || time.Since(start) != time.Second {
					t.Fatalf("error was retried or leaked: %v", err)
				}
			})
		})
	}
}

func TestRegistrationDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/leak?secret=private-nonce", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := BeginRegistration(context.Background(), RegistrationOptions{BaseURL: srv.URL})
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Status != 307 || hits != 1 || strings.Contains(err.Error(), "private-nonce") {
		t.Fatalf("redirect followed or leaked: %v, hits=%d", err, hits)
	}
}

func TestRegistrationRejectsInvalidInitAndCanceledBegin(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"errcode":null,"nonce":"private-nonce"}`, `{"errcode":0,"nonce":" "}`, `{"errcode":0,"nonce":123}`} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: registrationRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return registrationResponse(200, body), nil
			})}
			if _, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client}); !errors.Is(err, onboarding.ErrInvalidResponse) || calls != 1 {
				t.Fatalf("invalid init proceeded: calls=%d err=%v", calls, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: registrationRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled registration performed IO")
		return nil, context.Canceled
	})}
	if _, err := BeginRegistration(ctx, RegistrationOptions{HTTPClient: client}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
