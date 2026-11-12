package weixin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Icatme/imbot-go"
	"github.com/Icatme/imbot-go/internal/auth"
	"github.com/Icatme/imbot-go/internal/crypto"
)

func TestNativeURLImageDownload(t *testing.T) {
	payload := []byte("\x89PNG\r\n\x1a\nimage bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	bot := imbot.New(imbot.Options{LogLevel: "silent"})
	a, err := New(Config{AccountID: "native", Bot: bot, TakeHandlerOwnership: true})
	if err != nil {
		t.Fatal(err)
	}
	m := incoming()
	m.Images = []imbot.ImageContent{{URL: server.URL + "/image?token=private"}}
	m.Raw.ItemList = []imbot.MessageItem{{Type: imbot.ItemImage, ImageItem: &imbot.ImageItem{URL: m.Images[0].URL}}}
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
	bot := imbot.New(imbot.Options{BaseURL: server.URL, AccountID: "bot-1", CredPath: credentialPath, ContextTokenPath: filepath.Join(dir, "contexts.json"), CursorPath: filepath.Join(dir, "cursor.json"), ReplayPath: filepath.Join(dir, "replay.json"), LogLevel: "silent"})
	if _, err := bot.Login(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	firstMedia := &imbot.CDNMedia{FullURL: server.URL + "/first", AESKey: crypto.EncodeAESKeyBase64(key)}
	secondMedia := &imbot.CDNMedia{FullURL: server.URL + "/second", AESKey: crypto.EncodeAESKeyBase64(key)}
	m := incoming()
	m.Raw.MessageID = ""
	m.Raw.MessageType = imbot.MessageTypeUser
	m.Raw.ItemList = []imbot.MessageItem{
		{Type: imbot.ItemFile, MsgID: "A", FileItem: &imbot.FileItem{FileName: "first.txt", Media: firstMedia}},
		{Type: imbot.ItemFile, MsgID: "B", FileItem: &imbot.FileItem{FileName: "second.txt", Media: secondMedia}},
	}
	m.Files = []imbot.FileContent{{FileName: "first.txt", Media: firstMedia}, {FileName: "second.txt", Media: secondMedia}}
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
	quoted := &imbot.IncomingMessage{UserID: "u", QuotedMessage: &imbot.QuotedMessage{MessageID: "A", Media: &imbot.QuotedMedia{Type: imbot.ContentFile, Cached: true}}}
	retained, err := bot.DownloadQuoted(context.Background(), quoted)
	if err != nil || string(retained.Data) != "first document" {
		t.Fatal(retained, err)
	}
}
