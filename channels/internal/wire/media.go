package wire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

// MediaHTTP performs bounded binary/multipart IO against a configured platform
// endpoint. It never follows redirects or exposes signed URLs/response bodies in
// errors. max limits response bytes; upload callers must validate their input.
func MediaHTTP(ctx context.Context, c *http.Client, platform, method, endpoint string, headers http.Header, body io.Reader, max int64) ([]byte, http.Header, error) {
	return mediaHTTP(ctx, c, platform, method, endpoint, headers, body, max, false)
}

// MediaHTTPWithErrorBody additionally returns a bounded non-2xx response body
// for platform-specific status decoding. These bytes may contain secrets: never
// put them in errors/logs or expose them as downloaded content. Other failures
// return no body. MediaHTTP discards all error bodies by default.
func MediaHTTPWithErrorBody(ctx context.Context, c *http.Client, platform, method, endpoint string, headers http.Header, body io.Reader, max int64) ([]byte, http.Header, error) {
	return mediaHTTP(ctx, c, platform, method, endpoint, headers, body, max, true)
}

func mediaHTTP(ctx context.Context, c *http.Client, platform, method, endpoint string, headers http.Header, body io.Reader, max int64, errorBody bool) ([]byte, http.Header, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := Endpoint(endpoint); err != nil {
		return nil, nil, err
	}
	if max <= 0 || max > gateway.MaxMediaBytes+(64<<10) {
		return nil, nil, gateway.ErrInvalidEvent
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, nil, errors.New("invalid media request")
	}
	req.Header = headers.Clone()
	res, err := Client(c).Do(req)
	if err != nil {
		apiErr := &gateway.APIError{Platform: platform, Code: "media_transport", Uncertain: method != http.MethodGet}
		if cause := CancellationCause(ctx, err); cause != nil {
			return nil, nil, errors.Join(cause, apiErr)
		}
		return nil, nil, apiErr
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		api := &gateway.APIError{Platform: platform, Status: res.StatusCode, Code: "media_http", Retryable: res.StatusCode == 429 || res.StatusCode >= 500, Uncertain: method != http.MethodGet && res.StatusCode >= 500}
		if !errorBody {
			return nil, nil, api
		}
		const maxErrorBody = 64 << 10
		data, readErr := io.ReadAll(io.LimitReader(res.Body, maxErrorBody+1))
		if readErr != nil || len(data) > maxErrorBody || ctx.Err() != nil {
			api.Code = "media_error_body_limit_or_read"
			api.Uncertain = method != http.MethodGet && method != http.MethodHead
			if cause := CancellationCause(ctx, readErr); cause != nil {
				return nil, nil, errors.Join(cause, api)
			}
			return nil, nil, api
		}
		return data, res.Header.Clone(), api
	}
	if res.ContentLength > max {
		return nil, nil, mediaSizeError(platform, method, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		apiErr := &gateway.APIError{Platform: platform, Status: res.StatusCode, Code: "media_read", Uncertain: method != http.MethodGet}
		if cause := CancellationCause(ctx, err); cause != nil {
			return nil, nil, errors.Join(cause, apiErr)
		}
		return nil, nil, apiErr
	}
	if int64(len(data)) > max {
		return nil, nil, mediaSizeError(platform, method, res.StatusCode)
	}
	return data, res.Header.Clone(), nil
}

func mediaSizeError(platform, method string, status int) error {
	if method == http.MethodGet || method == http.MethodHead {
		return gateway.ErrMediaTooLarge
	}
	return errors.Join(gateway.ErrMediaTooLarge, &gateway.APIError{Platform: platform, Status: status, Code: "media_response_limit", Uncertain: true})
}

// DownloadMedia fetches a platform-supplied CDN URL. Default and standard HTTP
// transports pin each connection to a validated public address, blocking private
// IPs, DNS rebinding and proxy-based bypass. An explicitly injected nonstandard
// RoundTripper owns its own network policy (e.g. an isolated host transport).
func DownloadMedia(ctx context.Context, c *http.Client, platform, endpoint string, headers http.Header, max int64) ([]byte, http.Header, error) {
	return MediaTransfer(ctx, c, platform, http.MethodGet, endpoint, headers, nil, max)
}

// MediaTransfer applies the same public-destination policy to signed upload URLs.
func MediaTransfer(ctx context.Context, c *http.Client, platform, method, endpoint string, headers http.Header, body io.Reader, max int64) ([]byte, http.Header, error) {
	client := Client(c)
	var tr *http.Transport
	if client.Transport == nil {
		tr = &http.Transport{ForceAttemptHTTP2: true}
		if standard, ok := http.DefaultTransport.(*http.Transport); ok {
			tr = standard.Clone()
		}
	} else if standard, ok := client.Transport.(*http.Transport); ok {
		tr = standard.Clone()
	}
	if tr != nil {
		tr.Proxy = nil
		tr.DialContext = publicMediaDial
		tr.DialTLSContext = nil
		tr.DialTLS = nil
		client.Transport = tr
		defer tr.CloseIdleConnections()
	}
	return MediaHTTP(ctx, client, platform, method, endpoint, headers, body, max)
}

// ReportUnsupported makes intentional consumption visible even without a custom
// error callback. Only the structured format diagnostic is logged, never payloads.
func ReportUnsupported(err error, onError func(error)) {
	var unsupported *gateway.UnsupportedMessageError
	if !errors.As(err, &unsupported) {
		return
	}
	if onError != nil {
		onError(unsupported)
		return
	}
	slog.Warn("channel message format unsupported", "error", unsupported.Error())
}

func publicMediaDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.Contains(host, "%") {
		return nil, errors.New("invalid media destination")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("media destination lookup failed")
	}
	for _, address := range addresses {
		ip := address.IP
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return nil, errors.New("non-public media destination")
		}
		if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
			return nil, errors.New("non-public media destination")
		}
	}
	// No second DNS lookup after validation. A failure is returned to the caller;
	// the downloader does not retry another address or replay a request.
	dialer := net.Dialer{Timeout: 20 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
}
