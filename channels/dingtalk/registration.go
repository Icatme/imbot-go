package dingtalk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Icatme/imbot-go/channels/internal/enrollment"
	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/channels/onboarding"
)

// RegistrationOptions configures the explicit user-authorized robot creation
// flow. BaseURL is the registration origin, not Config.APIBase. An empty Source
// uses DING_DWS_CLAW, the current official connector's registration template.
type RegistrationOptions struct {
	HTTPClient *http.Client
	BaseURL    string
	Source     string
}

// Credentials are the result of creating/authorizing a bot. They are sensitive:
// the application owns secure persistence and must not write them to logs.
type Credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// Config prepares the existing Stream adapter. DingTalk's bot send APIs use the
// app's ClientID as RobotCode; application policy still controls who may send.
func (c Credentials) Config(accountID string) Config {
	return Config{AccountID: accountID, ClientID: c.ClientID, ClientSecret: c.ClientSecret, RobotCode: c.ClientID}
}

// Registration retains its polling credential privately. Display Challenge only
// to the initiating user. Wait does not start a replacement authorization flow.
type Registration struct {
	session *enrollment.Session[Credentials]
}

func (r *Registration) Challenge() onboarding.Challenge { return r.session.Challenge() }

// Wait waits for approval, respecting the server interval and expiry. Explicit
// rejection, expiry and malformed responses end this registration. Cancellation
// or an IO failure returns promptly; the caller may explicitly resume with Wait.
func (r *Registration) Wait(ctx context.Context) (Credentials, error) { return r.session.Wait(ctx) }

// BeginRegistration requests a short-lived scan-to-create authorization. It
// neither starts a bot nor changes files or existing adapter credentials.
func BeginRegistration(ctx context.Context, opts RegistrationOptions) (*Registration, error) {
	base := wire.Base(opts.BaseURL, "https://oapi.dingtalk.com")
	if err := wire.Endpoint(base); err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.ForceQuery {
		return nil, onboarding.ErrInvalidResponse
	}
	source := opts.Source
	if source == "" {
		source = "DING_DWS_CLAW"
	}
	if strings.TrimSpace(source) == "" {
		return nil, onboarding.ErrInvalidResponse
	}
	var init struct {
		Code  *int   `json:"errcode"`
		Nonce string `json:"nonce"`
	}
	if err = enrollment.PostJSON(ctx, opts.HTTPClient, "dingtalk", base+"/app/registration/init", map[string]string{"source": source}, &init); err != nil {
		return nil, err
	}
	if err = registrationCode(init.Code); err != nil {
		return nil, err
	}
	// Validate opaque protocol values without changing their bytes.
	nonce := init.Nonce
	if strings.TrimSpace(nonce) == "" {
		return nil, onboarding.ErrInvalidResponse
	}
	var begin struct {
		Code         *int            `json:"errcode"`
		DeviceCode   string          `json:"device_code"`
		UserCode     string          `json:"user_code"`
		URL          string          `json:"verification_uri_complete"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		PollInterval json.RawMessage `json:"interval"`
	}
	// Include request time in the lifetime, rather than extending the server's
	// authorization validity by time spent waiting for its response.
	started := time.Now()
	if err = enrollment.PostJSON(ctx, opts.HTTPClient, "dingtalk", base+"/app/registration/begin", map[string]string{"nonce": nonce}, &begin); err != nil {
		return nil, err
	}
	if err = registrationCode(begin.Code); err != nil {
		return nil, err
	}
	deviceCode, verificationURL := begin.DeviceCode, strings.TrimSpace(begin.URL)
	if strings.TrimSpace(deviceCode) == "" || wire.Endpoint(verificationURL) != nil {
		return nil, onboarding.ErrInvalidResponse
	}
	verify, err := url.Parse(verificationURL)
	if err != nil || (verify.Scheme != "https" && verify.Scheme != "http") {
		return nil, onboarding.ErrInvalidResponse
	}
	expires, err := registrationSeconds(begin.ExpiresIn, 7200)
	if err != nil {
		return nil, err
	}
	interval, err := registrationSeconds(begin.PollInterval, 3)
	if err != nil || interval > expires {
		return nil, onboarding.ErrInvalidResponse
	}
	challenge := onboarding.Challenge{URL: verificationURL, UserCode: strings.TrimSpace(begin.UserCode), ExpiresAt: started.Add(expires), PollInterval: interval}
	if !time.Now().Before(challenge.ExpiresAt) {
		return nil, onboarding.ErrExpired
	}
	poll := func(ctx context.Context) (Credentials, bool, error) {
		var out struct {
			Code         *int   `json:"errcode"`
			Status       string `json:"status"`
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if err := enrollment.PostJSON(ctx, opts.HTTPClient, "dingtalk", base+"/app/registration/poll", map[string]string{"device_code": deviceCode}, &out); err != nil {
			return Credentials{}, false, err
		}
		if err := registrationCode(out.Code); err != nil {
			return Credentials{}, false, err
		}
		switch strings.ToUpper(strings.TrimSpace(out.Status)) {
		case "WAITING", "SCANNED":
			// Scanning precedes authorization completion; credentials arrive only
			// with SUCCESS. Keep the original polling interval and deadline.
			return Credentials{}, false, nil
		case "SUCCESS":
			credentials := Credentials{ClientID: out.ClientID, ClientSecret: out.ClientSecret}
			if strings.TrimSpace(credentials.ClientID) == "" || strings.TrimSpace(credentials.ClientSecret) == "" {
				return Credentials{}, false, onboarding.ErrInvalidResponse
			}
			return credentials, true, nil
		case "FAIL":
			return Credentials{}, false, onboarding.ErrDenied
		case "EXPIRED":
			return Credentials{}, false, onboarding.ErrExpired
		default:
			return Credentials{}, false, onboarding.ErrInvalidResponse
		}
	}
	return &Registration{session: enrollment.NewSession(challenge, poll)}, nil
}

func registrationCode(code *int) error {
	if code == nil {
		return onboarding.ErrInvalidResponse
	}
	return wire.Business("dingtalk", *code)
}

// Defaults match the registration protocol clients, and apply only to omitted
// fields. Explicit null/zero/fractional/overflow values cannot extend a session.
func registrationSeconds(raw json.RawMessage, fallback int64) (time.Duration, error) {
	seconds := fallback
	if len(raw) != 0 {
		if string(raw) == "null" || json.Unmarshal(raw, &seconds) != nil {
			return 0, onboarding.ErrInvalidResponse
		}
	}
	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	if seconds <= 0 || seconds > maxSeconds {
		return 0, onboarding.ErrInvalidResponse
	}
	return time.Duration(seconds) * time.Second, nil
}
