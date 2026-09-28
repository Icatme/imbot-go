package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMessageIDLosslessJSON(t *testing.T) {
	for _, tc := range []struct {
		wire string
		want MessageID
	}{
		{`18446744073709551615`, "18446744073709551615"},
		{`9007199254740993`, "9007199254740993"},
		{`"18446744073709551615"`, "18446744073709551615"},
		{`"item-1"`, "item-1"},
		{`0`, ""}, {`"0"`, ""}, {`null`, ""}, {`""`, ""},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			id := MessageID("previous")
			if err := json.Unmarshal([]byte(tc.wire), &id); err != nil || id != tc.want {
				t.Fatalf("decode = %q, %v; want %q", id, err, tc.want)
			}
			raw, err := json.Marshal(id)
			if err != nil || string(raw) != `"`+string(tc.want)+`"` {
				t.Fatalf("marshal = %s, %v", raw, err)
			}
		})
	}
	for _, raw := range []string{`18446744073709551616`, `-1`, `1.5`, `1e3`, `true`, `{}`, `[]`} {
		id := MessageID("unchanged")
		if err := json.Unmarshal([]byte(raw), &id); err == nil || id != "unchanged" {
			t.Fatalf("invalid %s: id=%q err=%v", raw, id, err)
		}
	}
}

func TestSendMessageReturnsServerReceipt(t *testing.T) {
	for _, body := range []string{`{"ret":0,"message_id":18446744073709551615}`, `{"ret":0,"message_id":"18446744073709551615"}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			result, err := NewClient().SendMessage(context.Background(), server.URL, "token", map[string]any{})
			if err != nil || result.MessageID != "18446744073709551615" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestSendMessageDoesNotReturnReceiptOnRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ret":0,"errcode":-14,"message_id":123}`))
	}))
	defer server.Close()
	result, err := NewClient().SendMessage(context.Background(), server.URL, "token", map[string]any{})
	if err == nil || result != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
