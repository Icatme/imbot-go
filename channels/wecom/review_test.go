package wecom

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Icatme/imbot-go/gateway"
)

type cancelWriteSocket struct{ fakeSocket }

func (f *cancelWriteSocket) Write(ctx context.Context, _ byte, _ []byte) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestRequestCancellationPreservesCauseAndUncertainDelivery(t *testing.T) {
	for _, stage := range []string{"write", "ack", "deadline"} {
		t.Run(stage, func(t *testing.T) {
			_, s, f := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			if stage == "write" {
				writer := &cancelWriteSocket{}
				s.socket = writer
				s.timeout = time.Millisecond
				want = context.DeadlineExceeded
			} else if stage == "ack" {
				f.onWrite = func([]byte) { cancel() }
			} else {
				s.timeout = time.Millisecond
				want = context.DeadlineExceeded
			}
			out, err := s.request(ctx, "aibot_send_msg", "request", map[string]string{"chatid": "u"})
			var api *gateway.APIError
			if !errors.Is(err, want) || !errors.As(err, &api) || !api.Uncertain || out.Accepted || s.ctx.Err() == nil {
				t.Fatalf("cause=%v receipt=%+v error=%v closed=%v", want, out, err, s.ctx.Err() != nil)
			}
		})
	}
}

func TestDownloadPreservesDeclaredMediaType(t *testing.T) {
	a, s, _ := fixture(t)
	a.cfg.HTTPClient = &http.Client{Transport: mediaTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Content-Disposition": {`attachment; filename="report.json"`}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})}
	f := pushed()
	f.Body = []byte(`{"msgid":"m","chattype":"single","from":{"userid":"u"},"msgtype":"file","file":{"url":"https://cdn.test/report"}}`)
	e, err := a.decode(f, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || got.MIME != "application/json" || got.Name != "report.json" || string(got.Data) != `{"ok":true}` {
		t.Fatalf("download lost declared MIME: %+v %v", got, err)
	}
}
