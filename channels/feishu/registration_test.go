package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Icatme/wechatbot-go/channels/onboarding"
)

type registrationTransport func(*http.Request) (*http.Response, error)

func (f registrationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func registrationResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

const registrationBegin = `{"device_code":"private-device-code","verification_uri_complete":"https://accounts.feishu.cn/authorize?token=private-link","user_code":"USER-CODE","interval":1,"expires_in":60}`

func registrationClient(t *testing.T, begin string, poll func(*http.Request) (*http.Response, error)) *http.Client {
	t.Helper()
	return &http.Client{Transport: registrationTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != registrationPath || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("invalid registration request: method=%s path=%s content-type=%s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch r.Form.Get("action") {
		case "init":
			return registrationResponse(200, `{"supported_auth_methods":["client_secret"]}`), nil
		case "begin":
			if r.Form.Get("archetype") != "PersonalAgent" || r.Form.Get("auth_method") != "client_secret" || r.Form.Get("request_user_info") != "open_id" {
				t.Fatal("incorrect registration template")
			}
			return registrationResponse(200, begin), nil
		case "poll":
			if r.Form.Get("device_code") != "private-device-code" || r.Form.Get("tp") != "ob_app" {
				t.Fatal("incorrect polling credential or type")
			}
			return poll(r)
		default:
			t.Fatal("unexpected action")
			return nil, errors.New("unexpected action")
		}
	})}
}

func TestRegistrationHTTPContractAndConfig(t *testing.T) {
	var actions []string
	var actionsMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != registrationPath || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("incorrect endpoint or encoding")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		action := r.Form.Get("action")
		actionsMu.Lock()
		actions = append(actions, action)
		actionsMu.Unlock()
		switch action {
		case "init":
			fmt.Fprint(w, `{"supported_auth_methods":["client_secret"]}`)
		case "begin":
			if r.Form.Get("archetype") != "PersonalAgent" || r.Form.Get("request_user_info") != "open_id" || r.Form.Get("auth_method") != "client_secret" {
				t.Error("incorrect registration template")
			}
			fmt.Fprint(w, registrationBegin)
		case "poll":
			if r.Form.Get("device_code") != "private-device-code" {
				t.Error("wrong device code")
			}
			fmt.Fprint(w, `{"client_id":"cli_app","client_secret":"secret","user_info":{"open_id":"ou_owner","tenant_brand":"feishu"}}`)
		default:
			http.Error(w, "unexpected action", 400)
		}
	}))
	defer srv.Close()
	reg, err := BeginRegistration(context.Background(), RegistrationOptions{AccountsBaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	challenge := reg.Challenge()
	if challenge.URL != "https://accounts.feishu.cn/authorize?token=private-link" || challenge.UserCode != "USER-CODE" || challenge.PollInterval != time.Second {
		t.Fatal("incorrect challenge")
	}
	encoded, _ := json.Marshal(reg)
	if strings.Contains(string(encoded), "private-device-code") {
		t.Fatal("device credential serialized")
	}
	creds, err := reg.Wait(context.Background())
	if err != nil || creds.AppID != "cli_app" || creds.AppSecret != "secret" || creds.OpenID != "ou_owner" || creds.Domain != "feishu" {
		t.Fatalf("unexpected credential result: err=%v", err)
	}
	actionsMu.Lock()
	defer actionsMu.Unlock()
	if strings.Join(actions, ",") != "init,begin,poll" {
		t.Fatalf("unexpected requests: %v", actions)
	}
	cfg := creds.Config("local-account")
	if cfg.BaseURL != "https://open.feishu.cn" || cfg.Transport != "websocket" || cfg.AccountID != "local-account" || cfg.BotOpenID != "" {
		t.Fatal("credential configuration mixed user and bot identities")
	}
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationPendingSlowDownAndLark(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var hosts []string
		var times []time.Time
		client := registrationClient(t, registrationBegin, func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)
			times = append(times, time.Now())
			switch len(times) {
			case 1:
				return registrationResponse(400, `{"error":"authorization_pending"}`), nil
			case 2:
				return registrationResponse(400, `{"error":"slow_down","user_info":{"tenant_brand":"lark"}}`), nil
			default:
				return registrationResponse(200, `{"client_id":"cli_lark","client_secret":"secret","user_info":{"tenant_brand":"lark","open_id":"ou_owner"}}`), nil
			}
		})
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		creds, err := reg.Wait(context.Background())
		if err != nil || creds.Domain != "lark" || creds.Config("a").BaseURL != "https://open.larksuite.com" {
			t.Fatalf("Lark registration failed: %v", err)
		}
		if len(times) != 3 || times[0].Sub(start) != time.Second || times[1].Sub(times[0]) != time.Second || times[2].Sub(times[1]) != 6*time.Second {
			t.Fatalf("incorrect polling schedule: %v", times)
		}
		if strings.Join(hosts, ",") != "accounts.feishu.cn,accounts.feishu.cn,accounts.larksuite.com" {
			t.Fatalf("unexpected domain transition: %v", hosts)
		}
		if _, err := reg.Wait(context.Background()); err != nil || len(times) != 3 {
			t.Fatal("successful credentials not retained")
		}
	})
}

func TestRegistrationLarkCredentialsOnSwitchResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := registrationClient(t, registrationBegin, func(r *http.Request) (*http.Response, error) {
			calls++
			return registrationResponse(200, `{"client_id":"cli_lark","client_secret":"secret","user_info":{"tenant_brand":"lark"}}`), nil
		})
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		creds, err := reg.Wait(context.Background())
		if err != nil || creds.Domain != "lark" || calls != 1 {
			t.Fatalf("same-response credentials were discarded: %v, calls=%d", err, calls)
		}
	})
}

func TestRegistrationBrandOnlyLarkHandoff(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		t.Run(fmt.Sprintf("repeated=%t", repeated), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var hosts []string
				client := registrationClient(t, registrationBegin, func(r *http.Request) (*http.Response, error) {
					hosts = append(hosts, r.URL.Host)
					if len(hosts) == 1 || repeated {
						return registrationResponse(200, `{"user_info":{"tenant_brand":"lark"}}`), nil
					}
					return registrationResponse(200, `{"client_id":"cli_lark","client_secret":"secret"}`), nil
				})
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				creds, err := reg.Wait(context.Background())
				if repeated {
					if !errors.Is(err, onboarding.ErrInvalidResponse) {
						t.Fatalf("repeated brand-only response accepted: %v", err)
					}
				} else if err != nil || creds.Domain != "lark" {
					t.Fatalf("brand-only handoff failed: %v", err)
				}
				if strings.Join(hosts, ",") != "accounts.feishu.cn,accounts.larksuite.com" {
					t.Fatalf("incorrect handoff routing: %v", hosts)
				}
			})
		})
	}
}

func TestRegistrationTerminalResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       error
	}{
		{"denied", `{"error":"access_denied","error_description":"private-server-detail"}`, 400, onboarding.ErrDenied},
		{"expired", `{"error":"expired_token"}`, 400, onboarding.ErrExpired},
		{"denied_lark_handoff", `{"error":"access_denied","user_info":{"tenant_brand":"lark"}}`, 400, onboarding.ErrDenied},
		{"expired_lark_handoff", `{"error":"expired_token","user_info":{"tenant_brand":"lark"}}`, 400, onboarding.ErrExpired},
		{"400_brand_only", `{"user_info":{"tenant_brand":"lark"}}`, 400, onboarding.ErrInvalidResponse},
		{"empty", `{}`, 200, onboarding.ErrInvalidResponse},
		{"unknown", `{"error":"private-server-detail"}`, 400, onboarding.ErrInvalidResponse},
		{"partial", `{"client_id":"cli_app"}`, 200, onboarding.ErrInvalidResponse},
		{"whitespace", `{"client_id":" ","client_secret":"secret"}`, 200, onboarding.ErrInvalidResponse},
		{"400_success", `{"client_id":"cli_app","client_secret":"secret"}`, 400, onboarding.ErrInvalidResponse},
		{"error_with_credentials", `{"client_id":"cli_app","client_secret":"secret","error":"access_denied"}`, 200, onboarding.ErrInvalidResponse},
		{"unknown_brand", `{"client_id":"cli_app","client_secret":"secret","user_info":{"tenant_brand":"https://evil.test"}}`, 200, onboarding.ErrInvalidResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := registrationClient(t, registrationBegin, func(*http.Request) (*http.Response, error) {
					calls++
					return registrationResponse(tc.status, tc.body), nil
				})
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					creds, err := reg.Wait(context.Background())
					if !errors.Is(err, tc.want) || creds != (Credentials{}) || strings.Contains(err.Error(), "private-server-detail") {
						t.Fatalf("terminal response not rejected safely: %v", err)
					}
				}
				if calls != 1 {
					t.Fatal("terminal response retried")
				}
			})
		})
	}
}

func TestRegistrationInvalidBegin(t *testing.T) {
	for _, field := range []string{"device_code", "verification_uri_complete", "interval", "expires_in"} {
		for _, value := range []any{nil, "", 0, -1, 999999999999999999} {
			t.Run(fmt.Sprintf("%s_%v", field, value), func(t *testing.T) {
				var response map[string]any
				if err := json.Unmarshal([]byte(registrationBegin), &response); err != nil {
					t.Fatal(err)
				}
				response[field] = value
				body, _ := json.Marshal(response)
				client := registrationClient(t, string(body), func(*http.Request) (*http.Response, error) {
					t.Fatal("polled invalid registration")
					return nil, nil
				})
				if _, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client}); !errors.Is(err, onboarding.ErrInvalidResponse) {
					t.Fatalf("accepted invalid field: %v", err)
				}
			})
		}
	}
}

func TestRegistrationHonorsServerTiming(t *testing.T) {
	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	for _, tc := range []struct {
		name              string
		interval, expires int64
	}{
		{"long_interval", 301, 600},
		{"long_lifetime", 5, 7201},
		{"largest_lifetime", 5, maxSeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := fmt.Sprintf(`{"device_code":"private-device-code","verification_uri_complete":"https://accounts.feishu.cn/authorize","interval":%d,"expires_in":%d}`, tc.interval, tc.expires)
				calls := 0
				client := registrationClient(t, body, func(*http.Request) (*http.Response, error) {
					calls++
					return registrationResponse(200, `{"client_id":"cli_app","client_secret":"secret"}`), nil
				})
				start := time.Now()
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
				if err != nil {
					t.Fatalf("rejected valid server timing: %v", err)
				}
				interval := time.Duration(tc.interval) * time.Second
				expires := start.Add(time.Duration(tc.expires) * time.Second)
				if reg.Challenge().PollInterval != interval || !reg.Challenge().ExpiresAt.Equal(expires) {
					t.Fatalf("server timing was changed: %+v", reg.Challenge())
				}
				creds, err := reg.Wait(context.Background())
				if err != nil || creds.AppID != "cli_app" || calls != 1 || time.Since(start) != interval {
					t.Fatalf("server poll interval was ignored: calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
				}
			})
		})
	}
}

func TestRegistrationRejectsInvalidTiming(t *testing.T) {
	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	for _, tc := range []struct {
		name              string
		interval, expires int64
	}{
		{"lifetime_overflow", 5, maxSeconds + 1},
		{"interval_overflow", maxSeconds + 1, maxSeconds},
		{"interval_equals_lifetime", 60, 60},
		{"interval_exceeds_lifetime", 61, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"device_code":"private-device-code","verification_uri_complete":"https://accounts.feishu.cn/authorize","interval":%d,"expires_in":%d}`, tc.interval, tc.expires)
			client := registrationClient(t, body, nil)
			if _, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client}); !errors.Is(err, onboarding.ErrInvalidResponse) {
				t.Fatalf("accepted invalid server timing: %v", err)
			}
		})
	}
}

func TestRegistrationMissingTimingDefaultsAndOfficialExpiry(t *testing.T) {
	client := registrationClient(t, `{"device_code":"private-device-code","verification_uri_complete":"https://accounts.feishu.cn/authorize"}`, nil)
	start := time.Now()
	reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
	if err != nil || reg.Challenge().PollInterval != 5*time.Second || reg.Challenge().ExpiresAt.Before(start.Add(600*time.Second)) {
		t.Fatalf("documented defaults missing: %v", err)
	}
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		body := strings.Replace(registrationBegin, `"expires_in":60`, `"expires_in":3`, 1)
		client := registrationClient(t, body, func(*http.Request) (*http.Response, error) {
			calls++
			return registrationResponse(400, `{"error":"authorization_pending"}`), nil
		})
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if _, err := reg.Wait(context.Background()); !errors.Is(err, onboarding.ErrExpired) || time.Since(start) != 3*time.Second || calls != 2 {
			t.Fatalf("official expires_in was ignored: calls=%d err=%v elapsed=%v", calls, err, time.Since(start))
		}
	})
}

func TestRegistrationCancellationAndExplicitResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		client := registrationClient(t, registrationBegin, func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return registrationResponse(200, `{"client_id":"cli_app","client_secret":"secret"}`), nil
		})
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { _, err := reg.Wait(ctx); result <- err }()
		time.Sleep(2 * time.Second)
		if _, err := reg.Wait(context.Background()); !errors.Is(err, onboarding.ErrConcurrentWait) {
			t.Fatalf("concurrent wait accepted: %v", err)
		}
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel not preserved: %v", err)
		}
		if _, err := reg.Wait(context.Background()); err != nil || calls.Load() != 2 {
			t.Fatalf("explicit resume failed: %v", err)
		}
	})
}

func TestRegistrationRejectedConfigurationAndChallengeURLs(t *testing.T) {
	for _, opts := range []RegistrationOptions{{Domain: "unknown"}, {AccountsBaseURL: "https://u:secret@accounts.feishu.cn"}, {AccountsBaseURL: "http://evil.test"}, {AccountsBaseURL: "wss://accounts.feishu.cn"}, {AccountsBaseURL: "https://accounts.feishu.cn?query=value"}, {AccountsBaseURL: "https://accounts.feishu.cn?"}} {
		opts.HTTPClient = &http.Client{Transport: registrationTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid registration options reached the network")
			return nil, nil
		})}
		if _, err := BeginRegistration(context.Background(), opts); !errors.Is(err, onboarding.ErrInvalidResponse) {
			t.Fatalf("accepted invalid options: %v", err)
		}
	}
	for _, raw := range []string{"http://evil.test", "https://u:secret@example.com", "wss://example.com", "javascript:alert(1)"} {
		body := strings.Replace(registrationBegin, "https://accounts.feishu.cn/authorize?token=private-link", raw, 1)
		client := registrationClient(t, body, nil)
		if _, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client}); !errors.Is(err, onboarding.ErrInvalidResponse) {
			t.Fatalf("accepted unsafe challenge: %v", err)
		}
	}
}

func TestRegistrationRejectsInitAndBeginHTTPError(t *testing.T) {
	for _, badAction := range []string{"init", "begin"} {
		client := &http.Client{Transport: registrationTransport(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("action") == badAction {
				return registrationResponse(400, `{"supported_auth_methods":["client_secret"],"device_code":"private-device-code","verification_uri_complete":"https://accounts.feishu.cn/authorize"}`), nil
			}
			return registrationResponse(200, `{"supported_auth_methods":["client_secret"]}`), nil
		})}
		if _, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client}); !errors.Is(err, onboarding.ErrInvalidResponse) {
			t.Fatalf("accepted HTTP error during %s: %v", badAction, err)
		}
	}
}
