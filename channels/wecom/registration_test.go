package wecom

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Icatme/wechatbot-go/channels/onboarding"
	"github.com/Icatme/wechatbot-go/gateway"
)

type registrationRoundTripper func(*http.Request) (*http.Response, error)

func (f registrationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func registrationResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const registrationGenerated = `{"data":{"scode":"private-scode+/&=?","auth_url":"https://work.weixin.qq.com/ai/qc/auth?key=public-scan"}}`
const registrationSuccess = `{"data":{"status":"success","bot_info":{"botid":"robot","secret":"bot-secret"}}}`

type registrationFixture struct {
	t          *testing.T
	generated  string
	polls      []string
	pollStatus int
	pollError  error
	pollCount  int
}

func (f *registrationFixture) client() *http.Client {
	return &http.Client{Transport: registrationRoundTripper(func(r *http.Request) (*http.Response, error) {
		f.t.Helper()
		if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" || (r.Body != nil && r.Body != http.NoBody) {
			f.t.Fatalf("wrong GET contract: method=%s headers=%v body=%v", r.Method, r.Header, r.Body)
		}
		switch r.URL.Path {
		case "/ai/qc/generate":
			want := url.Values{"source": {"wecom_cli_external"}, "plat": {strconv.Itoa(registrationPlatform())}}
			if !reflect.DeepEqual(r.URL.Query(), want) {
				f.t.Fatalf("wrong generate query: %v", r.URL.Query())
			}
			body := f.generated
			if body == "" {
				body = registrationGenerated
			}
			return registrationResponse(200, body), nil
		case "/ai/qc/query_result":
			if !reflect.DeepEqual(r.URL.Query(), url.Values{"scode": {"private-scode+/&=?"}}) {
				f.t.Fatal("poll did not encode scode as a single query parameter")
			}
			f.pollCount++
			if f.pollError != nil {
				return nil, f.pollError
			}
			if f.pollCount > len(f.polls) {
				f.t.Fatalf("unexpected poll %d", f.pollCount)
			}
			status := f.pollStatus
			if status == 0 {
				status = 200
			}
			return registrationResponse(status, f.polls[f.pollCount-1]), nil
		default:
			f.t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, errors.New("unexpected path")
		}
	})}
}

func TestRegistrationBeginWireContract(t *testing.T) {
	for _, source := range []string{"", "custom +&source"} {
		t.Run(source, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				wantSource := source
				if wantSource == "" {
					wantSource = "wecom_cli_external"
				}
				wantQuery := url.Values{"source": {wantSource}, "plat": {strconv.Itoa(registrationPlatform())}}
				if r.Method != http.MethodGet || r.URL.Path != "/ai/qc/generate" || !reflect.DeepEqual(r.URL.Query(), wantQuery) {
					t.Errorf("wrong generate request: %s %s", r.Method, r.URL)
				}
				fmt.Fprint(w, registrationGenerated)
			}))
			defer srv.Close()
			before := time.Now()
			reg, err := BeginRegistration(context.Background(), RegistrationOptions{BaseURL: srv.URL, Source: source})
			if err != nil {
				t.Fatal(err)
			}
			challenge := reg.Challenge()
			if calls != 1 || challenge.URL != "https://work.weixin.qq.com/ai/qc/auth?key=public-scan" || challenge.UserCode != "" || challenge.PollInterval != 3*time.Second || challenge.ExpiresAt.Before(before.Add(5*time.Minute)) || challenge.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
				t.Fatalf("unexpected challenge: %+v; calls=%d", challenge, calls)
			}
			encoded, err := json.Marshal(reg)
			if err != nil || string(encoded) != "{}" {
				t.Fatalf("registration exposes polling state: %s %v", encoded, err)
			}
		})
	}
}

func TestRegistrationPendingAndAdapterConfig(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The official clients only recognize success. Preserve intermediate
		// nonempty statuses instead of guessing a closed enum.
		f := &registrationFixture{t: t, polls: []string{`{"data":{"status":"init"}}`, `{"data":{"status":"future-intermediate-state"}}`, registrationSuccess}}
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		credentials, err := reg.Wait(context.Background())
		if err != nil || credentials != (Credentials{BotID: "robot", Secret: "bot-secret"}) || time.Since(start) != 9*time.Second || f.pollCount != 3 {
			t.Fatalf("registration failed: polls=%d elapsed=%s err=%v", f.pollCount, time.Since(start), err)
		}
		again, err := reg.Wait(context.Background())
		if err != nil || again != credentials || f.pollCount != 3 {
			t.Fatal("success was not retained")
		}
		cfg := credentials.Config("local-account")
		adapter, err := New(cfg)
		if err != nil || cfg.TenantID != "" || adapter.Account().ID != "local-account" {
			t.Fatalf("credentials did not configure adapter: %v", err)
		}
	})
}

func TestRegistrationPreservesOpaqueValuesThroughAuthentication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const scode = " session +/&=\t"
		want := Credentials{BotID: " robot\t", Secret: " secret\t"}
		client := &http.Client{Transport: registrationRoundTripper(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/ai/qc/generate":
				return registrationResponse(200, `{"data":{"scode":" session +/&=\t","auth_url":"https://work.weixin.qq.com/auth"}}`), nil
			case "/ai/qc/query_result":
				if !reflect.DeepEqual(r.URL.Query(), url.Values{"scode": {scode}}) {
					t.Error("polling changed the opaque session code")
				}
				return registrationResponse(200, `{"data":{"status":"success","bot_info":{"botid":" robot\t","secret":" secret\t"}}}`), nil
			default:
				t.Fatal("unexpected registration endpoint")
				return nil, nil
			}
		})}
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := reg.Wait(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if credentials != want {
			t.Error("registration changed the opaque credentials")
		}

		type observedAuth struct {
			frame frame
			err   error
		}
		observed := make(chan observedAuth, 1)
		cfg := credentials.Config("local-account")
		cfg.HTTPClient = &http.Client{Transport: registrationRoundTripper(func(r *http.Request) (*http.Response, error) {
			local, remote := net.Pipe()
			go func() {
				defer remote.Close()
				rw := bufio.NewReadWriter(bufio.NewReader(remote), bufio.NewWriter(remote))
				auth, err := readClient(rw.Reader)
				if err == nil {
					// Stop after capturing the real authentication frame.
					rejected := 401
					err = writeServer(rw, frame{Headers: auth.Headers, Code: &rejected})
				}
				observed <- observedAuth{frame: auth, err: err}
			}()
			// RFC 6455 requires SHA-1 for the WebSocket handshake, not for credentials.
			sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			return &http.Response{StatusCode: http.StatusSwitchingProtocols, ProtoMajor: 1, Body: local, Header: http.Header{
				"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Accept": {base64.StdEncoding.EncodeToString(sum[:])},
			}}, nil
		})}
		adapter, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = adapter.connect(ctx, func(context.Context, gateway.Event) error { return nil })
		var api *gateway.APIError
		if !errors.As(err, &api) || api.Code != "authentication_rejected" {
			t.Fatalf("authentication frame was not exchanged: %v", err)
		}
		select {
		case got := <-observed:
			var body map[string]string
			if got.err != nil || got.frame.Cmd != "aibot_subscribe" || json.Unmarshal(got.frame.Body, &body) != nil {
				t.Fatalf("invalid authentication frame: %v", got.err)
			}
			if !reflect.DeepEqual(body, map[string]string{"bot_id": want.BotID, "secret": want.Secret}) {
				t.Error("authentication changed the original server credentials")
			}
		case <-ctx.Done():
			t.Fatal("authentication was not observed")
		}
	})
}

func TestRegistrationRejectsMalformedGeneration(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `{"data":null}`, `{"data":{"auth_url":"https://work.weixin.qq.com/auth"}}`,
		`{"data":{"scode":" " ,"auth_url":"https://work.weixin.qq.com/auth"}}`,
		`{"data":{"scode":123,"auth_url":"https://work.weixin.qq.com/auth"}}`,
		`{"data":{"scode":"private-scode","auth_url":"wss://work.weixin.qq.com/auth"}}`,
		`{"data":{"scode":"private-scode","auth_url":"http://remote.invalid/auth"}}`,
		`{"data":{"scode":"private-scode","auth_url":"https://user:secret@work.weixin.qq.com/auth"}}`,
		`{"errcode":null,"data":{"scode":"private-scode","auth_url":"https://work.weixin.qq.com/auth"}}`,
		`{"errcode":"0","data":{"scode":"private-scode","auth_url":"https://work.weixin.qq.com/auth"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			f := &registrationFixture{t: t, generated: body}
			_, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
			if !errors.Is(err, onboarding.ErrInvalidResponse) || strings.Contains(err.Error(), "private-scode") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("malformed generation accepted or leaked: %v", err)
			}
		})
	}
}

func TestRegistrationRejectsMalformedPoll(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `{"data":null}`, `{"data":{}}`, `{"data":{"status":null}}`,
		`{"data":{"status":" "}}`, `{"data":{"status":123}}`,
		`{"errcode":null,"data":{"status":"init"}}`, `{"errcode":"0","data":{"status":"init"}}`,
		`{"data":{"status":"success"}}`, `{"data":{"status":"success","bot_info":null}}`,
		`{"data":{"status":"success","bot_info":{"bot_id":"legacy","secret":"private-secret"}}}`,
		`{"data":{"status":"success","bot_info":{"botid":" \t","secret":"private-secret"}}}`,
		`{"data":{"status":"success","bot_info":{"botid":"robot"}}}`,
		`{"data":{"status":"success","bot_info":{"botid":"robot","secret":" "}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &registrationFixture{t: t, polls: []string{body}}
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					got, err := reg.Wait(context.Background())
					if got != (Credentials{}) || !errors.Is(err, onboarding.ErrInvalidResponse) || strings.Contains(err.Error(), "private-secret") {
						t.Fatalf("malformed response accepted or leaked: %v", err)
					}
				}
				if f.pollCount != 1 {
					t.Fatal("invalid response retried")
				}
			})
		})
	}
}

func TestRegistrationErrorsDoNotRetryOrLeak(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		body      string
		transport error
	}{
		{name: "business", body: `{"errcode":1234,"errmsg":"private-scode private-secret","data":{"status":"init"}}`},
		{name: "http", status: 503, body: `private-scode private-secret`},
		{name: "transport", transport: errors.New("request?scode=private-scode private-secret")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &registrationFixture{t: t, polls: []string{tt.body}, pollStatus: tt.status, pollError: tt.transport}
				reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				_, err = reg.Wait(context.Background())
				var api *gateway.APIError
				if !errors.As(err, &api) || strings.Contains(err.Error(), "private-") || f.pollCount != 1 || time.Since(start) != 3*time.Second {
					t.Fatalf("failure was retried or leaked: %v polls=%d", err, f.pollCount)
				}
			})
		})
	}
	f := &registrationFixture{t: t, generated: `{"errcode":4321,"errmsg":"private-scode private-secret"}`}
	_, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Code != "4321" || strings.Contains(err.Error(), "private-") {
		t.Fatalf("generate business error lost or leaked: %v", err)
	}
}

func TestRegistrationLocalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &registrationFixture{t: t}
		for range 99 {
			f.polls = append(f.polls, `{"data":{"status":"init"}}`)
		}
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for range 2 {
			_, err := reg.Wait(context.Background())
			if !errors.Is(err, onboarding.ErrExpired) || time.Since(start) != 5*time.Minute || f.pollCount != 99 {
				t.Fatalf("local deadline failed: elapsed=%s polls=%d err=%v", time.Since(start), f.pollCount, err)
			}
		}
	})
}

func TestRegistrationCancellationAndResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &registrationFixture{t: t, polls: []string{registrationSuccess}}
		reg, err := BeginRegistration(context.Background(), RegistrationOptions{HTTPClient: f.client()})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := reg.Wait(ctx); done <- err }()
		synctest.Wait()
		if _, err := reg.Wait(context.Background()); !errors.Is(err, onboarding.ErrConcurrentWait) {
			t.Fatalf("concurrent wait accepted: %v", err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || f.pollCount != 0 {
			t.Fatalf("canceled waiter made request: %v", err)
		}
		if _, err := reg.Wait(context.Background()); err != nil || f.pollCount != 1 {
			t.Fatalf("explicit resume failed: %v", err)
		}
	})
}

func TestRegistrationRejectsUnsafeOptionsAndCanceledBegin(t *testing.T) {
	client := &http.Client{Transport: registrationRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid/canceled registration performed IO")
		return nil, context.Canceled
	})}
	for _, opts := range []RegistrationOptions{
		{Source: " "}, {BaseURL: "wss://work.weixin.qq.com"}, {BaseURL: "http://remote.invalid"},
		{BaseURL: "https://work.weixin.qq.com?secret=private"}, {BaseURL: "https://work.weixin.qq.com?"},
	} {
		opts.HTTPClient = client
		if _, err := BeginRegistration(context.Background(), opts); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BeginRegistration(ctx, RegistrationOptions{HTTPClient: client}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRegistrationDoesNotFollowRedirect(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/leak?scode=private-scode", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := BeginRegistration(context.Background(), RegistrationOptions{BaseURL: srv.URL})
	var api *gateway.APIError
	if !errors.As(err, &api) || api.Status != 307 || hits != 1 || strings.Contains(err.Error(), "private-scode") {
		t.Fatalf("redirect followed or leaked: hits=%d err=%v", hits, err)
	}
}
