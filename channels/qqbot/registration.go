package qqbot

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Icatme/imbot-go/channels/internal/enrollment"
	"github.com/Icatme/imbot-go/channels/internal/wire"
	"github.com/Icatme/imbot-go/channels/onboarding"
	"github.com/Icatme/imbot-go/gateway"
)

// RegistrationOptions configures an explicit QQ bot binding. BaseURL overrides
// the registration API, not the official user-facing QR page or Config.APIBase.
// Source defaults to empty, which the platform displays as a third-party bot.
// Timeout is a local wait limit (default ten minutes), not a
// server-provided QR lifetime; the platform may expire the task sooner.
type RegistrationOptions struct {
	HTTPClient *http.Client
	BaseURL    string
	Source     string
	Timeout    time.Duration
}

// Credentials contains an app secret. The application owns secure persistence.
// UserOpenID is the scanning user's identity, not an automatic access grant.
type Credentials struct {
	AppID        string `json:"app_id"`
	ClientSecret string `json:"client_secret"`
	UserOpenID   string `json:"user_openid,omitempty"`
}

// Config supplies the existing Webhook adapter's credentials. Registration does
// not configure the platform's callback URL or enable proactive messages.
func (c Credentials) Config(accountID string) Config {
	return Config{AccountID: accountID, AppID: c.AppID, ClientSecret: c.ClientSecret}
}

// Registration holds its AES key privately. Challenge.URL contains the public
// task identifier; the key is sent only in the registration API request.
type Registration struct {
	session *enrollment.Session[Credentials]
}

func (r *Registration) Challenge() onboarding.Challenge { return r.session.Challenge() }

// Wait returns credentials after confirmed binding and authenticated decryption.
// Expiry ends the session without replacing the QR code. Cancellation or an IO
// error returns to the caller, who may explicitly resume the unexpired session.
func (r *Registration) Wait(ctx context.Context) (Credentials, error) { return r.session.Wait(ctx) }

// BeginRegistration creates a binding task with a fresh AES-256-GCM key. It does
// not change existing bot configuration or contact a QR rendering service.
func BeginRegistration(ctx context.Context, opts RegistrationOptions) (*Registration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	if timeout < 0 {
		return nil, onboarding.ErrInvalidResponse
	}
	started := time.Now()
	expiresAt := started.Add(timeout)
	if !expiresAt.After(started) || expiresAt.Sub(started) != timeout {
		return nil, onboarding.ErrInvalidResponse
	}
	base := wire.Base(opts.BaseURL, "https://q.qq.com")
	u, err := url.Parse(base)
	if err != nil || wire.Endpoint(base) != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.ForceQuery {
		return nil, onboarding.ErrInvalidResponse
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, onboarding.ErrInvalidResponse
	}
	var created struct {
		Code *int `json:"retcode"`
		Data *struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := enrollment.PostJSON(ctx, opts.HTTPClient, "qqbot", base+"/lite/create_bind_task", map[string]string{"key": base64.StdEncoding.EncodeToString(key[:])}, &created); err != nil {
		return nil, err
	}
	if err := registrationCode(created.Code); err != nil {
		return nil, err
	}
	if created.Data == nil || strings.TrimSpace(created.Data.TaskID) == "" {
		return nil, onboarding.ErrInvalidResponse
	}
	challenge := onboarding.Challenge{
		URL: "https://q.qq.com/qqbot/openclaw/connect.html?" + url.Values{
			"task_id": {created.Data.TaskID}, "source": {opts.Source}, "_wv": {"2"},
		}.Encode(),
		ExpiresAt: expiresAt, PollInterval: 2 * time.Second,
	}
	if !time.Now().Before(challenge.ExpiresAt) {
		return nil, onboarding.ErrExpired
	}
	reg := &Registration{}
	reg.session = enrollment.NewSession(challenge, func(ctx context.Context) (Credentials, bool, error) {
		var result struct {
			Code *int `json:"retcode"`
			Data *struct {
				Status          *int            `json:"status"`
				AppID           json.RawMessage `json:"bot_appid"`
				EncryptedSecret string          `json:"bot_encrypt_secret"`
				UserOpenID      string          `json:"user_openid"`
			} `json:"data"`
		}
		if err := enrollment.PostJSON(ctx, opts.HTTPClient, "qqbot", base+"/lite/poll_bind_result", map[string]string{"task_id": created.Data.TaskID}, &result); err != nil {
			return Credentials{}, false, err
		}
		if err := registrationCode(result.Code); err != nil {
			return Credentials{}, false, err
		}
		if result.Data == nil || result.Data.Status == nil {
			return Credentials{}, false, onboarding.ErrInvalidResponse
		}
		switch *result.Data.Status {
		case 0, 1: // NONE / PENDING
			return Credentials{}, false, nil
		case 3: // EXPIRED
			return Credentials{}, false, onboarding.ErrExpired
		case 2: // COMPLETED
			appID, ok := registrationAppID(result.Data.AppID)
			if !ok {
				return Credentials{}, false, onboarding.ErrInvalidResponse
			}
			secret, err := decryptRegistrationSecret(key[:], result.Data.EncryptedSecret)
			if err != nil {
				return Credentials{}, false, err
			}
			return Credentials{AppID: appID, ClientSecret: secret, UserOpenID: result.Data.UserOpenID}, true, nil
		default:
			return Credentials{}, false, onboarding.ErrInvalidResponse
		}
	})
	return reg, nil
}

func registrationCode(code *int) error {
	if code == nil {
		return onboarding.ErrInvalidResponse
	}
	if *code != 0 {
		return &gateway.APIError{Platform: "qqbot", Status: http.StatusOK, Code: strconv.Itoa(*code)}
	}
	return nil
}

// The official SDK accepts a string or numeric app ID. Parse numeric JSON
// directly to decimal text, avoiding float64 rounding of large identifiers.
func registrationAppID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	if raw[0] == '"' {
		var id string
		if json.Unmarshal(raw, &id) != nil || strings.TrimSpace(id) == "" {
			return "", false
		}
		return id, true
	}
	id := string(raw)
	for _, c := range id {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return id, id != "0"
}

func decryptRegistrationSecret(key []byte, encoded string) (string, error) {
	if len(key) != 32 || strings.ContainsAny(encoded, "\r\n") {
		return "", onboarding.ErrInvalidResponse
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return "", onboarding.ErrInvalidResponse
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", onboarding.ErrInvalidResponse
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(payload) <= gcm.NonceSize()+gcm.Overhead() {
		return "", onboarding.ErrInvalidResponse
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], nil)
	if err != nil || !utf8.Valid(plain) || strings.TrimSpace(string(plain)) == "" {
		return "", onboarding.ErrInvalidResponse
	}
	return string(plain), nil
}
