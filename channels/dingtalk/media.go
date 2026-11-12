package dingtalk

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

// Download resolves a private downloadCode, then retrieves the actual bytes.
// No token is forwarded to the returned URL, and neither URL nor code is exposed.
func (a *Adapter) Download(ctx context.Context, r gateway.Route, p gateway.Attachment) (gateway.MediaContent, error) {
	if r.Account != a.Account() {
		return gateway.MediaContent{}, gateway.ErrInvalidEvent
	}
	if err := p.ValidateFor(r); err != nil {
		return gateway.MediaContent{}, err
	}
	code, ok := a.resources.Get(wire.ContextKey(r, p.Ref))
	if !ok {
		return gateway.MediaContent{}, gateway.ErrMediaExpired
	}
	token, err := a.accessToken(ctx)
	if err != nil {
		return gateway.MediaContent{}, err
	}
	robot := a.cfg.RobotCode
	if robot == "" {
		robot = a.cfg.ClientID
	}
	var out struct {
		URL string `json:"downloadUrl"`
	}
	headers := make(http.Header)
	headers.Set("x-acs-dingtalk-access-token", token)
	err = wire.JSON(ctx, a.cfg.HTTPClient, "dingtalk", http.MethodPost, a.cfg.APIBase+"/v1.0/robot/messageFiles/download", headers, map[string]string{"robotCode": robot, "downloadCode": code}, &out)
	if err != nil {
		var api *gateway.APIError
		if errors.As(err, &api) && api.Status == http.StatusUnauthorized {
			a.token.Invalidate(token)
		}
		return gateway.MediaContent{}, err
	}
	if out.URL == "" {
		return gateway.MediaContent{}, &gateway.APIError{Platform: "dingtalk", Code: "missing_download_url"}
	}
	data, headers, err := wire.DownloadMedia(ctx, a.cfg.HTTPClient, "dingtalk", out.URL, nil, gateway.MaxMediaBytes)
	if err != nil {
		return gateway.MediaContent{}, err
	}
	name := p.Name
	if _, params, err := mime.ParseMediaType(headers.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	return gateway.MediaContent{Data: data, Name: name, MIME: headers.Get("Content-Type")}, nil
}
