package feishu

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Icatme/imbot-go/gateway"
	"github.com/Icatme/imbot-go/receive"
)

func TestNativeMediaKindsCannotBypassReceivePolicy(t *testing.T) {
	for _, tc := range []struct {
		native string
		kind   receive.Kind
	}{{"media", receive.Video}, {"sticker", receive.Image}, {"audio", receive.Voice}} {
		t.Run(tc.native, func(t *testing.T) {
			a := testAdapter(t)
			e, err := a.Decode(messagePayload(tc.native, `{"file_key":"native-key"}`))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(e.ContentKinds, []receive.Kind{tc.kind}) || len(e.Attachments) != 1 || e.Attachments[0].Kind != tc.native {
				t.Errorf("semantic/native kinds: kinds=%v attachments=%+v", e.ContentKinds, e.Attachments)
			}
			calls := 0
			g, err := gateway.New(gateway.Options{
				Authorize:     gateway.AllowAll,
				ReceivePolicy: receive.Policy{Disabled: []receive.Kind{tc.kind}},
				Handler:       func(context.Context, gateway.Event) error { calls++; return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Register(a); err != nil {
				t.Fatal(err)
			}
			err = g.Accept(t.Context(), e)
			var rejected *receive.Rejection
			if !errors.As(err, &rejected) || rejected.Kind != tc.kind || !gateway.Consumed(err) || calls != 0 {
				t.Fatalf("native alias bypassed policy: error=%v handler=%d", err, calls)
			}
		})
	}
}
