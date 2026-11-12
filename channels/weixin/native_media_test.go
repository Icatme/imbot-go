package weixin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	wechatbot "github.com/Icatme/wechatbot-go"
	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/crypto"
)

func TestNativeURLImageDownload(t *testing.T) {
	payload := []byte("\x89PNG\r\n\x1a\nimage bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	bot := wechatbot.New(wechatbot.Options{LogLevel: "silent"})
	a, err := New(Config{AccountID: "native", Bot: bot, TakeHandlerOwnership: true})
	if err != nil {
		t.Fatal(err)
	}
	m := incoming()
	m.Images = []wechatbot.ImageContent{{URL: server.URL + "/image?token=private"}}
	m.Raw.ItemList = []wechatbot.MessageItem{{Type: wechatbot.ItemImage, ImageItem: &wechatbot.ImageItem{URL: m.Images[0].URL}}}
	e, err := a.normalize(m)
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.Download(context.Background(), e.Route, e.Attachments[0])
	if err != nil || string(data.Data) != string(payload) || data.MIME != "image/png" {
		t.Fatalf("valid URL-only image could not be downloaded: %+v, %v", data, err)
	}
}

func TestNativeMultiAttachmentDownloadDoesNotOverwriteQuoteCache(t *testing.T) {
	key := []byte("0123456789abcdef")
	first, err := crypto.EncryptAESECB([]byte("first document"), key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := crypto.EncryptAESECB([]byte("second document"), key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/first":
			_, _ = w.Write(first)
		case "/second":
			_, _ = w.Write(second)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "credentials.json")
	if err := auth.SaveCredentials(&auth.Credentials{Token: "test-token", BaseURL: server.URL, AccountID: "bot-1", UserID: "bot-user"}, credentialPath); err != nil {
		t.Fatal(err)
	}
	bot := wechatbot.New(wechatbot.Options{BaseURL: server.URL, AccountID: "bot-1", CredPath: credentialPath, ContextTokenPath: filepath.Join(dir, "contexts.json"), CursorPath: filepath.Join(dir, "cursor.json"), ReplayPath: filepath.Join(dir, "replay.json"), LogLevel: "silent"})
	if _, err := bot.Login(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	firstMedia := &wechatbot.CDNMedia{FullURL: server.URL + "/first", AESKey: crypto.EncodeAESKeyBase64(key)}
	secondMedia := &wechatbot.CDNMedia{FullURL: server.URL + "/second", AESKey: crypto.EncodeAESKeyBase64(key)}
	m := incoming()
	m.Raw.MessageID = ""
	m.Raw.MessageType = wechatbot.MessageTypeUser
	m.Raw.ItemList = []wechatbot.MessageItem{
		{Type: wechatbot.ItemFile, MsgID: "A", FileItem: &wechatbot.FileItem{FileName: "first.txt", Media: firstMedia}},
		{Type: wechatbot.ItemFile, MsgID: "B", FileItem: &wechatbot.FileItem{FileName: "second.txt", Media: secondMedia}},
	}
	m.Files = []wechatbot.FileContent{{FileName: "first.txt", Media: firstMedia}, {FileName: "second.txt", Media: secondMedia}}
	if data, err := bot.Download(context.Background(), m); err != nil || string(data.Data) != "first document" {
		t.Fatal(data, err)
	}
	a, err := New(Config{AccountID: "native", Bot: bot, TakeHandlerOwnership: true})
	if err != nil {
		t.Fatal(err)
	}
	e, err := a.normalize(m)
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.Download(context.Background(), e.Route, e.Attachments[1])
	if err != nil || string(data.Data) != "second document" || data.Name != "second.txt" {
		t.Fatal(data, err)
	}
	quoted := &wechatbot.IncomingMessage{UserID: "u", QuotedMessage: &wechatbot.QuotedMessage{MessageID: "A", Media: &wechatbot.QuotedMedia{Type: wechatbot.ContentFile, Cached: true}}}
	retained, err := bot.DownloadQuoted(context.Background(), quoted)
	if err != nil || string(retained.Data) != "first document" {
		t.Fatal(retained, err)
	}
}
