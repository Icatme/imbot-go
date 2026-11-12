package dingtalk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Icatme/wechatbot-go/gateway"
)

func TestDownloadInvalidTokenIsNotReusedOnNextCall(t *testing.T) {
	a := testAdapter(t)
	tokens, resolutions := 0, 0
	a.cfg.HTTPClient = &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, status := "downloaded", 200
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			tokens++
			token := "stale"
			if tokens > 1 {
				token = "fresh"
			}
			body = `{"accessToken":"` + token + `","expireIn":7200}`
		case "/v1.0/robot/messageFiles/download":
			resolutions++
			body = `{"downloadUrl":"https://cdn.example.test/file"}`
			if r.Header.Get("x-acs-dingtalk-access-token") == "stale" {
				status = 401
				body = `{"code":"InvalidAuthentication"}`
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	e, err := a.Decode(richPayload("richText"), "delivery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Download(context.Background(), e.Route, e.Attachments[0]); err == nil || tokens != 1 || resolutions != 1 {
		t.Fatal(err, tokens, resolutions)
	}
	got, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || string(got.Data) != "downloaded" || tokens != 2 || resolutions != 2 {
		t.Fatal(got, err, tokens, resolutions)
	}
}

func TestUnknownConversationTypeIsObservedBeforeConsumption(t *testing.T) {
	a := testAdapter(t)
	reports := 0
	a.cfg.OnError = func(err error) {
		var unsupported *gateway.UnsupportedMessageError
		if !errors.As(err, &unsupported) || unsupported.MessageType != "conversation/future" {
			t.Errorf("unexpected report: %v", err)
		}
		reports++
	}
	payload := strings.Replace(string(richPayload("richText")), `"conversationType":"1"`, `"conversationType":"future"`, 1)
	ack, _ := a.handle(context.Background(), streamFrame{Type: "CALLBACK", Headers: map[string]string{"topic": topic, "messageId": "delivery"}, Data: payload}, func(context.Context, gateway.Event) error { t.Fatal("unknown route dispatched"); return nil })
	if ack.Code != 200 || reports != 1 {
		t.Fatal(ack.Code, reports)
	}
}

func TestUnknownMessageTypeDoesNotAssumeKnownContentShape(t *testing.T) {
	for _, content := range []string{`"opaque future payload"`, `[{"future":true}]`, `{"recognition":{"future":true}}`} {
		t.Run(content, func(t *testing.T) {
			a := testAdapter(t)
			reports, calls := 0, 0
			a.cfg.OnError = func(err error) {
				reports++
				var unsupported *gateway.UnsupportedMessageError
				if !errors.As(err, &unsupported) || unsupported.MessageType != "future-format" {
					t.Errorf("unexpected report: %v", err)
				}
			}
			payload := `{"msgId":"m","msgtype":"future-format","senderStaffId":"u","senderCorpId":"tenant","conversationType":"1","content":` + content + `}`
			ack, _ := a.handle(context.Background(), streamFrame{Type: "CALLBACK", Headers: map[string]string{"topic": topic, "messageId": "delivery"}, Data: payload}, func(context.Context, gateway.Event) error { calls++; return nil })
			if ack.Code != 200 || reports != 1 || calls != 0 {
				t.Fatalf("unknown content caused redelivery: ACK=%d reports=%d sink=%d", ack.Code, reports, calls)
			}
		})
	}
}

func TestKnownMalformedMediaStillRejectsAdmission(t *testing.T) {
	a := testAdapter(t)
	payload := `{"msgId":"m","msgtype":"file","senderStaffId":"u","senderCorpId":"tenant","conversationType":"1","content":"not a file object"}`
	ack, _ := a.handle(context.Background(), streamFrame{Type: "CALLBACK", Headers: map[string]string{"topic": topic, "messageId": "delivery"}, Data: payload}, func(context.Context, gateway.Event) error { t.Fatal("malformed file reached sink"); return nil })
	if ack.Code != 500 {
		t.Fatal("known malformed media was consumed", ack.Code)
	}
}
