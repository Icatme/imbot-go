package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

// Preserve the transport error chain while exposing the platform's numeric
// code as the first APIError. Neither diagnostic includes the response body.
type apiBusinessError struct {
	api   *gateway.APIError
	cause error
}

func (e *apiBusinessError) Error() string   { return e.api.Error() }
func (e *apiBusinessError) Unwrap() []error { return []error{e.api, e.cause} }

// apiHTTP preserves Feishu business errors even when their HTTP status is 400.
// The shared helper retains the response bounds, redirect policy and redaction.
// Successful resource GET bodies are files, including legitimate JSON files.
func (a *Adapter) apiHTTP(ctx context.Context, token, method, endpoint string, headers http.Header, body io.Reader, max int64) ([]byte, http.Header, error) {
	data, responseHeaders, err := wire.MediaHTTPWithErrorBody(ctx, a.cfg.HTTPClient, "feishu", method, endpoint, headers, body, max)
	if err == nil {
		return data, responseHeaders, nil
	}
	var api *gateway.APIError
	if len(data) > 0 && errors.As(err, &api) && (api.Status < 200 || api.Status >= 300) {
		var response struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(data, &response) == nil && response.Code != 0 {
			platform := *api
			platform.Code = strconv.Itoa(response.Code)
			err = &apiBusinessError{api: &platform, cause: err}
		}
	}
	return nil, responseHeaders, a.mediaAPIError(token, err)
}

func (a *Adapter) apiJSON(ctx context.Context, token, endpoint string, input, output any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if len(encoded) > wire.MaxBody {
		return gateway.ErrInvalidEvent
	}
	headers := wire.Bearer(token)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	data, _, err := a.apiHTTP(ctx, token, http.MethodPost, endpoint, headers, bytes.NewReader(encoded), wire.MaxBody)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, output) != nil {
		return &gateway.APIError{Platform: "feishu", Status: http.StatusOK, Code: "invalid_json", Uncertain: true}
	}
	return nil
}
