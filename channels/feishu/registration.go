package feishu

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

const registrationPath = "/oauth/v1/app/registration"

// RegistrationOptions starts a new, explicitly authorized bot setup. Domain is
// "feishu" (default) or "lark". AccountsBaseURL is an explicit endpoint override
// for the initial domain; a detected Lark tenant switches to the official Lark
// account service. No endpoint supplied in a response is used for polling.
type RegistrationOptions struct {
	HTTPClient      *http.Client
	Domain          string
	AccountsBaseURL string
}

// Credentials contains an app secret. Store it securely; do not log it. OpenID
// identifies the authorizing user, not the bot and not an automatic access grant.
type Credentials struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
	Domain    string `json:"domain"`
	OpenID    string `json:"open_id,omitempty"`
}

// Config connects credentials returned by Wait to the existing adapter. The
// application must still set its authorization policy and group mention identity.
func (c Credentials) Config(accountID string) Config {
	base := "https://open.feishu.cn"
	if c.Domain == "lark" {
		base = "https://open.larksuite.com"
	}
	return Config{AccountID: accountID, AppID: c.AppID, AppSecret: c.AppSecret, BaseURL: base, Transport: "websocket"}
}

// Registration holds a private, expiring device code. Only Challenge should be
// displayed to the initiating user. Construct registrations with BeginRegistration.
type Registration struct {
	session *enrollment.Session[Credentials]
}

func (r *Registration) Challenge() onboarding.Challenge { return r.session.Challenge() }

// Wait polls at the server's requested interval until authorization completes.
// Cancellation and network errors return to the caller without starting another
// registration. Expiry, denial and malformed responses are terminal.
func (r *Registration) Wait(ctx context.Context) (Credentials, error) { return r.session.Wait(ctx) }

// BeginRegistration requests a scan-to-create authorization challenge. The host
// renders Challenge().URL locally as a QR code; no QR service receives its URL.
func BeginRegistration(ctx context.Context, opts RegistrationOptions) (*Registration, error) {
	domain := opts.Domain
	if domain == "" {
		domain = "feishu"
	}
	if domain != "feishu" && domain != "lark" {
		return nil, onboarding.ErrInvalidResponse
	}
	base := wire.Base(opts.AccountsBaseURL, accountsBase(domain))
	baseURL, err := url.Parse(base)
	if !validRegistrationURL(base) || err != nil || baseURL.RawQuery != "" || baseURL.ForceQuery {
		return nil, onboarding.ErrInvalidResponse
	}
	var init struct {
		Methods []string `json:"supported_auth_methods"`
		Error   string   `json:"error"`
	}
	status, err := enrollment.PostForm(ctx, opts.HTTPClient, "feishu", base+registrationPath, url.Values{"action": {"init"}}, &init)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || init.Error != "" {
		return nil, onboarding.ErrInvalidResponse
	}
	supported := false
	for _, method := range init.Methods {
		supported = supported || method == "client_secret"
	}
	if !supported {
		return nil, onboarding.ErrInvalidResponse
	}
	var begin struct {
		DeviceCode string          `json:"device_code"`
		URL        string          `json:"verification_uri_complete"`
		UserCode   string          `json:"user_code"`
		Interval   json.RawMessage `json:"interval"`
		ExpiresIn  json.RawMessage `json:"expires_in"`
		Error      string          `json:"error"`
	}
	// Anchor expiry before the request: network latency must not extend a server
	// challenge beyond the lifetime that it advertises.
	started := time.Now()
	status, err = enrollment.PostForm(ctx, opts.HTTPClient, "feishu", base+registrationPath, url.Values{
		"action": {"begin"}, "archetype": {"PersonalAgent"}, "auth_method": {"client_secret"}, "request_user_info": {"open_id"},
	}, &begin)
	if err != nil {
		return nil, err
	}
	interval, validInterval := registrationSeconds(begin.Interval, 5)
	lifetime, validLifetime := registrationSeconds(begin.ExpiresIn, 600)
	if status != http.StatusOK || begin.Error != "" || strings.TrimSpace(begin.DeviceCode) == "" || !validRegistrationURL(begin.URL) || !validInterval || !validLifetime || interval >= lifetime {
		return nil, onboarding.ErrInvalidResponse
	}
	challenge := onboarding.Challenge{URL: begin.URL, UserCode: begin.UserCode, ExpiresAt: started.Add(lifetime), PollInterval: interval}
	if !time.Now().Before(challenge.ExpiresAt) {
		return nil, onboarding.ErrExpired
	}
	reg := &Registration{}
	reg.session = enrollment.NewSession(challenge, func(ctx context.Context) (Credentials, bool, error) {
		var response struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Error        string `json:"error"`
			UserInfo     struct {
				OpenID      string `json:"open_id"`
				TenantBrand string `json:"tenant_brand"`
			} `json:"user_info"`
		}
		status, err := enrollment.PostForm(ctx, opts.HTTPClient, "feishu", base+registrationPath, url.Values{
			"action": {"poll"}, "device_code": {begin.DeviceCode}, "tp": {"ob_app"},
		}, &response)
		if err != nil {
			return Credentials{}, false, err
		}
		if status != http.StatusOK && status != http.StatusBadRequest {
			return Credentials{}, false, onboarding.ErrInvalidResponse
		}
		brand := response.UserInfo.TenantBrand
		if brand != "" && brand != "feishu" && brand != "lark" || domain == "lark" && brand == "feishu" {
			return Credentials{}, false, onboarding.ErrInvalidResponse
		}
		if response.ClientID != "" || response.ClientSecret != "" {
			if status != http.StatusOK || response.Error != "" || strings.TrimSpace(response.ClientID) == "" || strings.TrimSpace(response.ClientSecret) == "" {
				return Credentials{}, false, onboarding.ErrInvalidResponse
			}
			if brand == "lark" {
				domain = "lark"
			}
			return Credentials{AppID: response.ClientID, AppSecret: response.ClientSecret, Domain: domain, OpenID: response.UserInfo.OpenID}, true, nil
		}
		switch response.Error {
		case "":
			// A first Feishu response may only identify the Lark tenant. That
			// is a domain handoff, not an arbitrary empty pending response.
			if status != http.StatusOK || brand != "lark" || domain == "lark" {
				return Credentials{}, false, onboarding.ErrInvalidResponse
			}
		case "authorization_pending":
		case "slow_down":
			// RFC 8628 / the official SDK require five additional seconds for
			// this and every subsequent poll, rather than an arbitrary retry.
			if err := reg.session.IncreasePollInterval(5 * time.Second); err != nil {
				return Credentials{}, false, err
			}
		case "access_denied":
			return Credentials{}, false, onboarding.ErrDenied
		case "expired_token":
			return Credentials{}, false, onboarding.ErrExpired
		default:
			return Credentials{}, false, onboarding.ErrInvalidResponse
		}
		if brand == "lark" && domain != "lark" {
			domain, base = "lark", accountsBase("lark")
		}
		return Credentials{}, false, nil
	})
	return reg, nil
}

func accountsBase(domain string) string {
	if domain == "lark" {
		return "https://accounts.larksuite.com"
	}
	return "https://accounts.feishu.cn"
}

func validRegistrationURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && wire.Endpoint(raw) == nil
}

func registrationSeconds(raw json.RawMessage, fallback int64) (time.Duration, bool) {
	seconds := fallback
	if len(raw) != 0 && (string(raw) == "null" || json.Unmarshal(raw, &seconds) != nil) {
		return 0, false
	}
	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	if seconds <= 0 || seconds > maxSeconds {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}
