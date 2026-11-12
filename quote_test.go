package imbot

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icatme/imbot-go/internal/auth"
	"github.com/Icatme/imbot-go/internal/store"
)

func newQuoteTestBot(t *testing.T, dir, account string) *Bot {
	t.Helper()
	bot := New(Options{
		CredPath:         filepath.Join(dir, "credentials.json"),
		ContextTokenPath: filepath.Join(dir, "context.json"),
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay.json"),
		LogLevel:         "silent",
	})
	bot.creds = &auth.Credentials{AccountID: account, Token: "token", BaseURL: "https://unused.invalid"}
	return bot
}

func deliverQuoteTestWire(t *testing.T, bot *Bot, raw string) *IncomingMessage {
	t.Helper()
	var received *IncomingMessage
	err := bot.processUpdateBatch(context.Background(), MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		received = msg
		return AckMessage()
	}), updateBatch{messages: []json.RawMessage{json.RawMessage(raw)}, cursor: "after"})
	if err != nil {
		t.Fatal(err)
	}
	if received == nil {
		t.Fatal("message not delivered")
	}
	return received
}

func TestQuoteIncomingRestartAndPartialResolution(t *testing.T) {
	dir := t.TempDir()
	first := newQuoteTestBot(t, dir, "account")
	const source = `{"message_id":18446744073709551615,"from_user_id":"peer","to_user_id":"account","message_type":1,"item_list":[{"type":1,"msg_id":9007199254740993,"text_item":{"text":" 你好，世界！你好，朋友！ "}}]}`
	original := deliverQuoteTestWire(t, first, source)
	if original.Raw.MessageID != "18446744073709551615" || original.Raw.ItemList[0].MsgID != "9007199254740993" {
		t.Fatalf("IDs=%+v", original.Raw)
	}
	if !first.replayStore.Seen("peer:4:peer:message:18446744073709551615") {
		t.Fatal("lossless replay key missing")
	}

	restarted := newQuoteTestBot(t, dir, "account")
	const quoted = `{"message_id":2,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"text_item":{"text":"这段什么意思？"},"ref_msg":{"svr_id":18446744073709551615,"partial_text":{"start":"你好","end":"！","startindex":1,"endindex":1,"quotemd5":"%s"}}}]}`
	want := "你好，朋友！"
	var afterReceiveQuote string
	restarted.Hooks().AfterReceive.Register(func(msg *IncomingMessage) error { afterReceiveQuote = msg.QuotedMessage.Text; return nil })
	reply := deliverQuoteTestWire(t, restarted, fmt.Sprintf(quoted, quoteTestMD5(want)))
	if reply.QuotedMessage.Text != want || reply.QuotedMessage.Resolution != QuoteCached || afterReceiveQuote != want {
		t.Fatalf("quote=%+v hook=%q", reply.QuotedMessage, afterReceiveQuote)
	}
	if reply.Raw.ItemList[0].RefMsg.MessageItem != nil {
		t.Fatal("raw wire was modified during resolution")
	}
	for _, tc := range []struct{ account, peer string }{{"different-account", "peer"}, {"account", "different-peer"}} {
		other := newQuoteTestBot(t, dir, tc.account)
		raw := strings.Replace(fmt.Sprintf(quoted, quoteTestMD5(want)), `"from_user_id":"peer"`, `"from_user_id":"`+tc.peer+`"`, 1)
		msg := deliverQuoteTestWire(t, other, raw)
		if msg.QuotedMessage.Text != "" || msg.QuotedMessage.Resolution != QuoteMissing {
			t.Fatalf("cross-scope quote=%+v", msg.QuotedMessage)
		}
	}
}

func TestQuoteItemOnlyIdentitySurvivesRestart(t *testing.T) {
	for _, tc := range []struct {
		name, topLevel, itemID string
		wantRawID              MessageID
	}{
		{"numeric item ID", "", `18446744073709551615`, ""},
		{"string item ID", "", `"18446744073709551615"`, ""},
		{"zero top-level ID", `"message_id":0,`, `18446744073709551615`, ""},
		{"blank top-level ID", `"message_id":"  ",`, `" 18446744073709551615 "`, "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bot := newQuoteTestBot(t, dir, "account")
			source := fmt.Sprintf(`{%s"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"msg_id":0},{"type":1,"msg_id":%s,"text_item":{"text":"original item-only text"}}]}`, tc.topLevel, tc.itemID)
			original := deliverQuoteTestWire(t, bot, source)
			if original.Raw.MessageID != tc.wantRawID {
				t.Fatalf("raw message ID changed: got %q, want %q", original.Raw.MessageID, tc.wantRawID)
			}
			restarted := newQuoteTestBot(t, dir, "account")
			msg := deliverQuoteTestWire(t, restarted, `{"message_id":2,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"text_item":{"text":"reply"},"ref_msg":{"svr_id":18446744073709551615}}]}`)
			if msg.QuotedMessage.Text != "original item-only text" || msg.QuotedMessage.Resolution != QuoteCached {
				t.Fatalf("quote = %+v", msg.QuotedMessage)
			}
		})
	}
}

func TestQuoteCachePrefersTopLevelIdentity(t *testing.T) {
	bot := newQuoteTestBot(t, t.TempDir(), "account")
	deliverQuoteTestWire(t, bot, `{"message_id":11,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"msg_id":22,"text_item":{"text":"top-level text"}}]}`)
	for _, tc := range []struct {
		id         int
		want       string
		resolution QuoteResolution
	}{
		{11, "top-level text", QuoteCached},
		{22, "", QuoteMissing},
	} {
		raw := fmt.Sprintf(`{"message_id":%d,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"ref_msg":{"svr_id":%d}}]}`, 100+tc.id, tc.id)
		msg := deliverQuoteTestWire(t, bot, raw)
		if msg.QuotedMessage.Text != tc.want || msg.QuotedMessage.Resolution != tc.resolution {
			t.Fatalf("reference %d: %+v", tc.id, msg.QuotedMessage)
		}
	}
}

func TestQuoteNestedReferenceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, ref, want string
		id              MessageID
		resolution      QuoteResolution
	}{
		{"numeric nested ID", `{"message_item":{"type":1,"msg_id":123}}`, "known original", "123", QuoteCached},
		{"string nested ID", `{"message_item":{"type":1,"msg_id":"123"}}`, "known original", "123", QuoteCached},
		{"zero server ID", `{"svr_id":0,"message_item":{"type":1,"msg_id":123}}`, "known original", "123", QuoteCached},
		{"blank server ID", `{"svr_id":"  ","message_item":{"type":1,"msg_id":" 123 "}}`, "known original", "123", QuoteCached},
		{"server ID takes priority", `{"svr_id":123,"message_item":{"type":1,"msg_id":456}}`, "known original", "123", QuoteCached},
		{"missing nested ID", `{"message_item":{"type":1,"msg_id":456}}`, "", "456", QuoteMissing},
		{"empty placeholder", `{"message_item":{"type":1}}`, "", "", QuoteMissing},
		{"empty media placeholder", `{"message_item":{"type":2,"msg_id":456,"image_item":{}}}`, "", "456", QuoteMissing},
		{"inline text takes priority", `{"svr_id":123,"message_item":{"type":1,"msg_id":456,"text_item":{"text":"inline text"}}}`, "inline text", "123", QuoteEmbedded},
		{"inline media takes priority", `{"svr_id":123,"message_item":{"type":2,"image_item":{"media":{"encrypt_query_param":"existing-media"}}}}`, "", "123", QuoteEmbedded},
		{"inline title takes priority", `{"svr_id":123,"title":"inline summary","message_item":{"type":1}}`, "", "123", QuoteEmbedded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot := newQuoteTestBot(t, t.TempDir(), "account")
			deliverQuoteTestWire(t, bot, `{"message_id":123,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"text_item":{"text":"known original"}}]}`)
			raw := fmt.Sprintf(`{"message_id":2,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"text_item":{"text":"reply"},"ref_msg":%s}]}`, tc.ref)
			msg := deliverQuoteTestWire(t, bot, raw)
			q := msg.QuotedMessage
			if q.Text != tc.want || q.MessageID != tc.id || q.Resolution != tc.resolution {
				t.Fatalf("quote = %+v", q)
			}
			if msg.Raw.ItemList[0].RefMsg.MessageItem.TextItem != nil && tc.want != "inline text" {
				t.Fatal("resolution modified the raw reference")
			}
		})
	}
}

func TestQuoteOutgoingReceiptAndActualWireTextSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	bot := newQuoteTestBot(t, dir, "account")
	var wireText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Msg WireMessage `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		wireText = body.Msg.ItemList[0].TextItem.Text
		_, _ = w.Write([]byte(`{"ret":0,"message_id":18446744073709551615}`))
	}))
	defer server.Close()
	bot.creds.BaseURL = server.URL
	bot.client.HTTP = server.Client()
	if err := bot.contextTokens.Set("peer", "context"); err != nil {
		t.Fatal(err)
	}
	bot.Hooks().BeforeSend.Register(func(req *SendRequest) error { req.Message.Item.TextItem.Text = " 1 < 2 "; return nil })
	var hookReceipt MessageID
	bot.Hooks().AfterSend.Register(func(outcome SendOutcome) error { hookReceipt = outcome.Result.MessageID; return nil })
	result, err := bot.SendMessage(context.Background(), "peer", OutboundMessage{Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "original"}}})
	if err != nil || result.MessageID != "18446744073709551615" || hookReceipt != result.MessageID {
		t.Fatalf("result=%+v hook=%q err=%v", result, hookReceipt, err)
	}
	if wireText != " 1 ＜ 2 " {
		t.Fatalf("wire=%q", wireText)
	}
	restarted := newQuoteTestBot(t, dir, "account")
	msg := deliverQuoteTestWire(t, restarted, `{"message_id":3,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"text_item":{"text":"为何？"},"ref_msg":{"svr_id":"18446744073709551615"}}]}`)
	if msg.QuotedMessage.Text != wireText || msg.QuotedMessage.Resolution != QuoteCached {
		t.Fatalf("quote=%+v wire=%q", msg.QuotedMessage, wireText)
	}
}

func TestQuoteCacheFailureDoesNotTurnAcceptedSendIntoFailure(t *testing.T) {
	bot := newQuoteTestBot(t, t.TempDir(), "account")
	if err := os.WriteFile(bot.quoteStore.Path(), []byte(`broken cache`), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ret":0,"message_id":123}`)) }))
	defer server.Close()
	bot.creds.BaseURL = server.URL
	bot.client.HTTP = server.Client()
	if err := bot.contextTokens.Set("peer", "context"); err != nil {
		t.Fatal(err)
	}
	var reported []error
	bot.opts.OnError = func(err error) { reported = append(reported, err) }
	result, err := bot.SendMessage(context.Background(), "peer", OutboundMessage{Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "delivered"}}})
	if err != nil || result.MessageID != "123" || len(reported) != 1 {
		t.Fatalf("result=%+v err=%v cache errors=%v", result, err, reported)
	}
	var cacheErr *QuoteCacheError
	if !errors.As(reported[0], &cacheErr) {
		t.Fatalf("cache error lost type: %v", reported[0])
	}
	msg := deliverQuoteTestWire(t, bot, `{"message_id":4,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"ref_msg":{"svr_id":123}}]}`)
	if msg.QuotedMessage.Text != "" || msg.QuotedMessage.Resolution != QuoteUnavailable || len(reported) != 2 {
		t.Fatalf("quote=%+v errors=%v", msg.QuotedMessage, reported)
	}
	data, _ := os.ReadFile(bot.quoteStore.Path())
	if string(data) != "broken cache" {
		t.Fatal("corrupt cache was overwritten")
	}
}

func TestQuoteFailedSendNeverCachesReceipt(t *testing.T) {
	bot := newQuoteTestBot(t, t.TempDir(), "account")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ret":-2,"message_id":123}`)) }))
	defer server.Close()
	bot.creds.BaseURL = server.URL
	bot.client.HTTP = server.Client()
	if err := bot.contextTokens.Set("peer", "context"); err != nil {
		t.Fatal(err)
	}
	result, err := bot.SendMessage(context.Background(), "peer", OutboundMessage{Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "not delivered"}}})
	if err == nil || result.MessageID != "" || result.ClientID == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(bot.quoteStore.Path()); !os.IsNotExist(err) {
		t.Fatalf("failed send wrote quote cache: %v", err)
	}
}

func TestQuoteDisabledAndInvalidPartialRemainExplicit(t *testing.T) {
	bot := newQuoteTestBot(t, t.TempDir(), "account")
	path := bot.quoteStore.Path()
	bot.quoteStore = nil
	msg := deliverQuoteTestWire(t, bot, `{"message_id":5,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"text_item":{"text":"incoming"},"ref_msg":{"svr_id":123}}]}`)
	if msg.QuotedMessage.Resolution != QuoteDisabled || msg.QuotedMessage.Text != "" {
		t.Fatalf("quote=%+v", msg.QuotedMessage)
	}
	msg = deliverQuoteTestWire(t, bot, `{"message_id":6,"from_user_id":"peer","message_type":1,"item_list":[{"type":1,"ref_msg":{"message_item":{"type":1,"text_item":{"text":"original"}},"partial_text":{"start":"o","end":"l","startindex":0,"endindex":0,"quotemd5":"wrong"}}}]}`)
	if msg.QuotedMessage.Resolution != QuoteInvalidPartial || msg.QuotedMessage.Text != "" {
		t.Fatalf("invalid partial=%+v", msg.QuotedMessage)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled cache wrote file: %v", err)
	}
}

func TestQuoteCachePathCollisionProtectsOtherState(t *testing.T) {
	bot := newQuoteTestBot(t, t.TempDir(), "account")
	if err := bot.contextTokens.Set("peer", "context"); err != nil {
		t.Fatal(err)
	}
	bot.quoteStore = store.NewQuoteStore(bot.contextTokens.Path(), 0, 0)
	var reported error
	bot.opts.OnError = func(err error) { reported = err }
	bot.cacheQuote("account", "peer", "123", "body")
	if reported == nil || !strings.Contains(reported.Error(), "must be distinct") {
		t.Fatalf("collision error=%v", reported)
	}
	var tokens map[string]string
	data, _ := os.ReadFile(bot.contextTokens.Path())
	if err := json.Unmarshal(data, &tokens); err != nil || tokens["peer"] != "context" {
		t.Fatalf("state overwritten: %s %v", data, err)
	}
}

func TestResolvePartialQuote(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		partial          PartialText
		ok               bool
	}{
		{"global", "abcedfabcgh", "abc", PartialText{"a", "c", 1, 1, quoteTestMD5("abc")}, true},
		{"relative with hash", "x-end start first-end second-end", "start first-end second-end", PartialText{"start", "end", 0, 1, quoteTestMD5("start first-end second-end")}, true},
		{"global without hash", "x-end start first-end second-end", "start first-end", PartialText{"start", "end", 0, 1, ""}, true},
		{"unicode and whitespace", " 🐱你好！ ", " 🐱你好！ ", PartialText{" ", " ", 0, 1, strings.ToUpper(quoteTestMD5(" 🐱你好！ "))}, true},
		{"negative index", "hello", "", PartialText{"h", "o", -1, 0, ""}, false},
		{"large index", "hello", "", PartialText{"h", "o", int(^uint(0) >> 1), 0, ""}, false},
		{"wrong hash", "hello", "", PartialText{"h", "o", 0, 0, "bad"}, false},
		{"no guessed relative slice", "end start end", "", PartialText{"start", "end", 0, 0, ""}, false},
		{"missing marker", "hello", "", PartialText{"", "o", 0, 0, ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolvePartialQuote(tc.text, tc.partial)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q %v, want %q %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func quoteTestMD5(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

func TestPartialQuoteMissingIndexesAreNotZero(t *testing.T) {
	for _, raw := range []string{
		`{"start":"h","end":"o"}`,
		`{"start":"h","end":"o","startindex":null,"endindex":0}`,
		`{"start":"h","end":"o","startindex":0}`,
	} {
		var partial PartialText
		if err := json.Unmarshal([]byte(raw), &partial); err != nil {
			t.Fatal(err)
		}
		if text, ok := resolvePartialQuote("hello", partial); ok || text != "" {
			t.Fatalf("missing index produced %q", text)
		}
	}
}
