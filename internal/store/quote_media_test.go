package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQuoteMediaRestartIsolationAndOwnedNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	s := NewQuoteStore(path, 0, 0)
	key := QuoteKey{"account", "peer", "123"}
	meta := QuoteMedia{Type: "file", FileName: "../../original.pdf", MIMEType: "application/pdf"}
	if err := s.PutMedia(key, meta, []byte("document")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(key, QuoteText{Text: "caption"}); err != nil {
		t.Fatal(err)
	}
	other := NewQuoteStore(path, 0, 0)
	data, got, err := other.ReadMedia(key)
	if err != nil || string(data) != "document" || got.FileName != meta.FileName || !validMediaFile(got.File) {
		t.Fatalf("read=%q %+v %v", data, got, err)
	}
	if text, ok, err := other.Get(key); err != nil || !ok || text.Text != "caption" {
		t.Fatalf("text=%q %v %v", text, ok, err)
	}
	for _, wrong := range []QuoteKey{{"other", "peer", "123"}, {"account", "other", "123"}} {
		if _, _, err := other.ReadMedia(wrong); !errors.Is(err, ErrQuoteMediaUnavailable) {
			t.Fatalf("cross-scope read: %v", err)
		}
	}
	entries, err := os.ReadDir(s.MediaDir())
	if err != nil || len(entries) != 1 || entries[0].Name() != got.File {
		t.Fatalf("files=%v %v", entries, err)
	}
	// A replay retains the original payload and timestamp, and cannot publish a caller-supplied path.
	replayTime := time.Now().Add(time.Hour)
	s.now = func() time.Time { return replayTime }
	for _, name := range []string{meta.FileName, "", "updated.pdf"} {
		if err := s.PutMedia(key, QuoteMedia{Type: "file", FileName: name, File: "../../secret"}, nil); err != nil {
			t.Fatal(err)
		}
		data, replayed, err := s.ReadMedia(key)
		if err != nil || string(data) != "document" || replayed != got {
			t.Fatalf("replay name=%q data=%q metadata=%+v err=%v", name, data, replayed, err)
		}
	}
}

func TestQuoteMediaRetentionBudgetsAndCleanup(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), 24*time.Hour, 10)
	s.ConfigureMedia(QuoteMediaPolicy{Retention: time.Hour, MaxBytes: 6, MaxFileBytes: 4})
	now := time.Unix(10000, 0)
	s.now = func() time.Time { return now }
	key := func(account, id string) QuoteKey { return QuoteKey{account, "peer", id} }
	put := func(account, id, data string) {
		t.Helper()
		if err := s.PutMedia(key(account, id), QuoteMedia{Type: "file", FileName: id}, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	put("a", "old", "1234")
	now = now.Add(time.Second)
	put("a", "new", "5678")
	put("b", "other-account", "abcd")
	put("a", "too-large", "12345")
	for _, id := range []string{"old", "too-large"} {
		if _, _, err := s.ReadMedia(key("a", id)); !errors.Is(err, ErrQuoteMediaUnavailable) {
			t.Fatalf("budget miss %s: %v", id, err)
		}
		if m, ok, err := s.GetMedia(key("a", id)); err != nil || !ok || m.File != "" {
			t.Fatalf("metadata=%+v %v %v", m, ok, err)
		}
	}
	if _, _, err := s.ReadMedia(key("b", "other-account")); err != nil {
		t.Fatal(err)
	}
	// Only owned orphan names are eligible for deletion; arbitrary files survive.
	unknown := filepath.Join(s.MediaDir(), "user-notes.txt")
	if err := os.WriteFile(unknown, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if m, ok, err := s.GetMedia(key("a", "new")); err != nil || !ok || m.File != "" {
		t.Fatalf("expired metadata=%+v %v %v", m, ok, err)
	}
	if _, _, err := s.ReadMedia(key("a", "new")); !errors.Is(err, ErrQuoteMediaUnavailable) {
		t.Fatal(err)
	}
	if err := s.Prune(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(s.MediaDir())
	if len(entries) != 1 || entries[0].Name() != "user-notes.txt" {
		t.Fatalf("remaining files=%v", entries)
	}
	if _, ok, err := s.GetMedia(key("a", "new")); err != nil || !ok {
		t.Fatalf("expiry deleted display metadata: %v %v", ok, err)
	}
}

func TestQuoteMediaFailedCommitPreservesOldPayload(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), 0, 0)
	key := QuoteKey{"account", "peer", "id"}
	meta := QuoteMedia{Type: "file", FileName: "doc"}
	if err := s.PutMedia(key, meta, []byte("old")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.Path())
	failure := errors.New("disk unavailable")
	s.write = func(string, any) error { return failure }
	if err := s.PutMedia(key, meta, []byte("new")); !errors.Is(err, failure) {
		t.Fatalf("write=%v", err)
	}
	data, _, err := s.ReadMedia(key)
	if err != nil || string(data) != "old" {
		t.Fatalf("old payload=%q %v", data, err)
	}
	after, _ := os.ReadFile(s.Path())
	if !bytes.Equal(before, after) {
		t.Fatal("failed commit changed metadata")
	}
	entries, _ := os.ReadDir(s.MediaDir())
	if len(entries) != 1 {
		t.Fatalf("failed commit stranded payloads: %v", entries)
	}
}

func TestQuoteMediaDisabledMissingAndUnsafeFiles(t *testing.T) {
	for _, mode := range []string{"disabled", "missing", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), 0, 0)
			key := QuoteKey{"a", "p", "m"}
			if err := s.PutMedia(key, QuoteMedia{Type: "file"}, []byte("retained")); err != nil {
				t.Fatal(err)
			}
			m, _, _ := s.GetMedia(key)
			switch mode {
			case "disabled":
				s.ConfigureMedia(QuoteMediaPolicy{Disabled: true})
			case "missing":
				if err := os.Remove(filepath.Join(s.MediaDir(), m.File)); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := filepath.Join(t.TempDir(), "secret")
				if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(s.MediaDir(), m.File)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(s.MediaDir(), m.File)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			data, _, err := s.ReadMedia(key)
			if err == nil || data != nil || strings.Contains(string(data), "secret") {
				t.Fatalf("unsafe read=%q %v", data, err)
			}
			if mode != "symlink" && !errors.Is(err, ErrQuoteMediaUnavailable) {
				t.Fatal(err)
			}
		})
	}
}

func TestQuoteMediaMessageEvictionAndOrphans(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), 0, 1)
	for _, id := range []string{"one", "two"} {
		if err := s.PutMedia(QuoteKey{"a", "p", id}, QuoteMedia{Type: "image"}, []byte(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.ReadMedia(QuoteKey{"a", "p", "one"}); !errors.Is(err, ErrQuoteMediaUnavailable) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(s.MediaDir())
	if len(entries) != 1 {
		t.Fatalf("evicted file survived: %v", entries)
	}
	orphan := filepath.Join(s.MediaDir(), "media-AbCdEfGhIjKlMn")
	if err := os.WriteFile(orphan, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := NewQuoteStore(s.Path(), 0, 1)
	if err := restarted.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan survived prune: %v", err)
	}
}

func TestQuoteMediaConcurrentWritesReadsAndPrune(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), 0, 0)
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Go(func() {
			key := QuoteKey{"account", "peer", fmt.Sprint(i + 1)}
			if err := s.PutMedia(key, QuoteMedia{Type: "file"}, []byte(key.Message)); err != nil {
				t.Error(err)
				return
			}
			if err := s.Prune(); err != nil {
				t.Error(err)
				return
			}
			data, _, err := s.ReadMedia(key)
			if err != nil || string(data) != key.Message {
				t.Errorf("concurrent read=%q %v", data, err)
			}
		})
	}
	workers.Wait()
}
