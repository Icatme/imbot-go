package qqbot

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

// operationJSON checks HTTP and QQ business status for every mutating operation.
// A valid empty acknowledgement is HTTP 204; other success bodies must be JSON.
func (a *Adapter) operationJSON(ctx context.Context, method, path string, input, output any) error {
	token, err := a.accessToken(ctx)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if len(encoded) > wire.MaxBody {
		return gateway.ErrMediaTooLarge
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.APIBase+path, bytes.NewReader(encoded))
	if err != nil {
		return gateway.ErrInvalidEvent
	}
	req.Header = http.Header{"Authorization": {"QQBot " + token}, "X-Union-Appid": {a.cfg.AppID}, "Content-Type": {"application/json"}, "Accept": {"application/json"}}
	res, err := wire.Client(a.cfg.HTTPClient).Do(req)
	if err != nil {
		api := &gateway.APIError{Platform: "qqbot", Code: "transport", Uncertain: true}
		if cause := wire.CancellationCause(ctx, err); cause != nil {
			return errors.Join(cause, api)
		}
		return api
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, wire.MaxBody+1))
	if err != nil || len(raw) > wire.MaxBody {
		api := a.invalidateTokenOnUnauthorized(token, &gateway.APIError{Platform: "qqbot", Status: res.StatusCode, Code: "response_limit_or_read", Uncertain: true})
		if cause := wire.CancellationCause(ctx, err); cause != nil {
			return errors.Join(cause, api)
		}
		return api
	}
	raw = bytes.TrimSpace(raw)
	if res.StatusCode == http.StatusNoContent && len(raw) == 0 && output == nil {
		return nil
	}
	var status struct {
		Code      int `json:"code"`
		ErrorCode int `json:"err_code"`
	}
	decodeErr := json.Unmarshal(raw, &status)
	if res.StatusCode < 200 || res.StatusCode >= 300 || status.Code != 0 || status.ErrorCode != 0 {
		code := status.Code
		if code == 0 {
			code = status.ErrorCode
		}
		codeText := "http"
		if code != 0 {
			codeText = strconv.Itoa(code)
		}
		return a.invalidateTokenOnUnauthorized(token, &gateway.APIError{Platform: "qqbot", Status: res.StatusCode, Code: codeText, Retryable: res.StatusCode == 429 || res.StatusCode >= 500 || code == 40093001, Uncertain: res.StatusCode >= 500})
	}
	if decodeErr != nil {
		return &gateway.APIError{Platform: "qqbot", Status: res.StatusCode, Code: "invalid_response", Uncertain: true}
	}
	if len(raw) == 0 || raw[0] != '{' {
		return &gateway.APIError{Platform: "qqbot", Status: res.StatusCode, Code: "invalid_response", Uncertain: true}
	}
	if output != nil && json.Unmarshal(raw, output) != nil {
		return &gateway.APIError{Platform: "qqbot", Status: res.StatusCode, Code: "invalid_response", Uncertain: true}
	}
	return nil
}
