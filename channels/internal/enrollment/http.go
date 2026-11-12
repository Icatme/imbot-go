package enrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/channels/onboarding"
	"github.com/Icatme/wechatbot-go/gateway"
)

func PostJSON(ctx context.Context, client *http.Client, platform, endpoint string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return onboarding.ErrInvalidResponse
	}
	_, err = request(ctx, client, platform, http.MethodPost, endpoint, "application/json", body, output, false)
	return err
}

// GetJSON keeps query-based registration credentials out of returned errors and
// rejects redirects, just like the POST registration helpers.
func GetJSON(ctx context.Context, client *http.Client, platform, endpoint string, output any) error {
	_, err := request(ctx, client, platform, http.MethodGet, endpoint, "", nil, output, false)
	return err
}

// PostForm also decodes HTTP 400: OAuth device registration uses that status for
// pending/denied/expired responses. The platform must validate the parsed result.
func PostForm(ctx context.Context, client *http.Client, platform, endpoint string, input url.Values, output any) (int, error) {
	return request(ctx, client, platform, http.MethodPost, endpoint, "application/x-www-form-urlencoded", []byte(input.Encode()), output, true)
}

func request(ctx context.Context, client *http.Client, platform, method, endpoint, contentType string, body []byte, output any, allowBadRequest bool) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := wire.Endpoint(endpoint); err != nil {
		return 0, err
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || len(body) > wire.MaxBody {
		return 0, onboarding.ErrInvalidResponse
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, onboarding.ErrInvalidResponse
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	res, err := wire.Client(client).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, &gateway.APIError{Platform: platform, Code: "registration_transport", Uncertain: true}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, wire.MaxBody+1))
	if ctx.Err() != nil {
		return res.StatusCode, ctx.Err()
	}
	if err != nil || len(data) > wire.MaxBody {
		return res.StatusCode, &gateway.APIError{Platform: platform, Status: res.StatusCode, Code: "registration_response_limit_or_read", Uncertain: true}
	}
	if (res.StatusCode < 200 || res.StatusCode >= 300) && !(allowBadRequest && res.StatusCode == http.StatusBadRequest) {
		return res.StatusCode, &gateway.APIError{Platform: platform, Status: res.StatusCode, Code: "registration_http", Uncertain: res.StatusCode >= 500}
	}
	if json.Unmarshal(data, output) != nil {
		return res.StatusCode, onboarding.ErrInvalidResponse
	}
	return res.StatusCode, nil
}
