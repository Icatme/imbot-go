package imbot_test

import (
	"errors"
	"testing"

	"github.com/Icatme/imbot-go"
)

func TestPublicAPIErrorSupportsErrorsAs(t *testing.T) {
	want := &imbot.APIError{
		Endpoint:   "/ilink/bot/sendmessage",
		HTTPStatus: 401,
		RetCode:    -14,
		Message:    "expired",
	}
	var got *imbot.APIError
	if !errors.As(want, &got) || got.Code() != -14 || !got.IsSessionExpired() {
		t.Fatalf("public API error = %+v", got)
	}
}
