package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/enrollment"
	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/channels/onboarding"
)

// RegistrationOptions selects the smart-robot scan-to-create service, separate
// from self-built enterprise applications (wecomapp). Source defaults to the
// current official wecom-cli template, wecom_cli_external.
type RegistrationOptions struct {
	HTTPClient *http.Client
	BaseURL    string
	Source     string
}

// Credentials authenticate a smart robot's WebSocket connection. The caller
// owns secure storage; never put Secret in logs or a public configuration page.
type Credentials struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// Config connects the result to the existing smart-robot adapter. The service
// does not return a tenant identity; set TenantID through application policy.
func (c Credentials) Config(accountID string) Config {
	return Config{AccountID: accountID, BotID: c.BotID, Secret: c.Secret}
}

// Registration holds the private scode through the polling closure. It does not
// render QR codes, write credentials, or replace an existing robot connection.
type Registration struct {
	session *enrollment.Session[Credentials]
}

func (r *Registration) Challenge() onboarding.Challenge               { return r.session.Challenge() }
func (r *Registration) Wait(ctx context.Context) (Credentials, error) { return r.session.Wait(ctx) }

// BeginRegistration requests a QR authorization URL from WeCom. The current
// official client polls every three seconds for at most five minutes; the API
// does not return an expiry. Challenge.ExpiresAt is this local waiting limit,
// measured from the generate request, not a claim about server validity.
func BeginRegistration(ctx context.Context, opts RegistrationOptions) (*Registration, error) {
	base := wire.Base(opts.BaseURL, "https://work.weixin.qq.com")
	if err := wire.Endpoint(base); err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.ForceQuery {
		return nil, onboarding.ErrInvalidResponse
	}
	source := opts.Source
	if source == "" {
		source = "wecom_cli_external"
	}
	if strings.TrimSpace(source) == "" {
		return nil, onboarding.ErrInvalidResponse
	}
	query := url.Values{"source": {source}, "plat": {strconv.Itoa(registrationPlatform())}}
	var generated struct {
		Code json.RawMessage `json:"errcode"`
		Data *struct {
			SessionCode string `json:"scode"`
			AuthURL     string `json:"auth_url"`
		} `json:"data"`
	}
	started := time.Now()
	if err := enrollment.GetJSON(ctx, opts.HTTPClient, "wecom", base+"/ai/qc/generate?"+query.Encode(), &generated); err != nil {
		return nil, err
	}
	if err := registrationCode(generated.Code); err != nil {
		return nil, err
	}
	if generated.Data == nil {
		return nil, onboarding.ErrInvalidResponse
	}
	scode, authURL := generated.Data.SessionCode, strings.TrimSpace(generated.Data.AuthURL)
	if strings.TrimSpace(scode) == "" || wire.Endpoint(authURL) != nil {
		return nil, onboarding.ErrInvalidResponse
	}
	auth, err := url.Parse(authURL)
	if err != nil || (auth.Scheme != "https" && auth.Scheme != "http") {
		return nil, onboarding.ErrInvalidResponse
	}
	challenge := onboarding.Challenge{URL: authURL, ExpiresAt: started.Add(5 * time.Minute), PollInterval: 3 * time.Second}
	if !time.Now().Before(challenge.ExpiresAt) {
		return nil, onboarding.ErrExpired
	}
	endpoint := base + "/ai/qc/query_result?" + url.Values{"scode": {scode}}.Encode()
	poll := func(ctx context.Context) (Credentials, bool, error) {
		var result struct {
			Code json.RawMessage `json:"errcode"`
			Data *struct {
				Status  string `json:"status"`
				BotInfo *struct {
					BotID  string `json:"botid"`
					Secret string `json:"secret"`
				} `json:"bot_info"`
			} `json:"data"`
		}
		if err := enrollment.GetJSON(ctx, opts.HTTPClient, "wecom", endpoint, &result); err != nil {
			return Credentials{}, false, err
		}
		if err := registrationCode(result.Code); err != nil {
			return Credentials{}, false, err
		}
		if result.Data == nil || strings.TrimSpace(result.Data.Status) == "" {
			return Credentials{}, false, onboarding.ErrInvalidResponse
		}
		switch result.Data.Status {
		case "success":
			if result.Data.BotInfo == nil {
				return Credentials{}, false, onboarding.ErrInvalidResponse
			}
			credentials := Credentials{BotID: result.Data.BotInfo.BotID, Secret: result.Data.BotInfo.Secret}
			if strings.TrimSpace(credentials.BotID) == "" || strings.TrimSpace(credentials.Secret) == "" {
				return Credentials{}, false, onboarding.ErrInvalidResponse
			}
			return credentials, true, nil
		default:
			// The official wecom-cli and QR page define only "success" as
			// complete; every other nonempty status waits. Do not invent
			// denied/expired enums. The local five-minute deadline bounds it.
			return Credentials{}, false, nil
		}
	}
	return &Registration{session: enrollment.NewSession(challenge, poll)}, nil
}

func registrationCode(raw json.RawMessage) error {
	// Successful responses from the service omit errcode. If an error envelope
	// includes it, never ignore a nonzero code or malformed field.
	if len(raw) == 0 {
		return nil
	}
	var code int
	if string(raw) == "null" || json.Unmarshal(raw, &code) != nil {
		return onboarding.ErrInvalidResponse
	}
	return wire.Business("wecom", code)
}

func registrationPlatform() int {
	switch runtime.GOOS {
	case "darwin":
		return 1
	case "windows":
		return 2
	case "linux":
		return 3
	default:
		return 0
	}
}
