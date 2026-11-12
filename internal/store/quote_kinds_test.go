package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestQuoteKindsPersistAndDoNotAliasCaller(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	s := NewQuoteStore(path, 0, 0)
	key := QuoteKey{"account", "peer", "mixed"}
	kinds := []string{"text", "voice", "file"}
	if err := s.Put(key, QuoteText{Text: "caption and transcript", Kinds: kinds}); err != nil {
		t.Fatal(err)
	}
	kinds[0] = "changed input"
	got, ok, err := s.Get(key)
	if err != nil || !ok || got.Text != "caption and transcript" || !slices.Equal(got.Kinds, []string{"text", "voice", "file"}) {
		t.Fatalf("initial get=%+v %v %v", got, ok, err)
	}
	got.Kinds[1] = "changed output"
	for _, current := range []*QuoteStore{s, NewQuoteStore(path, 0, 0)} {
		got, ok, err = current.Get(key)
		if err != nil || !ok || !slices.Equal(got.Kinds, []string{"text", "voice", "file"}) {
			t.Fatalf("mutated/reopened get=%+v %v %v", got, ok, err)
		}
	}
}

func TestQuoteKindsOnlySurviveMediaWrites(t *testing.T) {
	for _, caption := range []string{"", "caption"} {
		t.Run(caption, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "quotes.json")
			s := NewQuoteStore(path, 0, 0)
			key := QuoteKey{"account", "peer", "source"}
			want := QuoteText{Text: caption, Kinds: []string{"image"}}
			if caption != "" {
				want.Kinds = append(want.Kinds, "text")
			}
			if err := s.Put(key, want); err != nil {
				t.Fatal(err)
			}
			for _, payload := range [][]byte{nil, []byte("image bytes"), nil} {
				if err := s.PutMedia(key, QuoteMedia{Type: "image"}, payload); err != nil {
					t.Fatal(err)
				}
				got, ok, err := NewQuoteStore(path, 0, 0).Get(key)
				if err != nil || !ok || got.Text != want.Text || !slices.Equal(got.Kinds, want.Kinds) {
					t.Fatalf("media write lost semantics: %+v %v %v", got, ok, err)
				}
			}
		})
	}
}

func TestQuoteKindsEnrichmentDoesNotRenewRetention(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), time.Hour, 0)
	now := time.Unix(10000, 0)
	s.now = func() time.Time { return now }
	writes := 0
	write := s.write
	s.write = func(path string, value any) error { writes++; return write(path, value) }
	key := QuoteKey{"account", "peer", "old"}
	legacy := QuoteText{Text: "text"}
	if err := s.Put(key, legacy); err != nil {
		t.Fatal(err)
	}
	savedAt := s.records[key].SavedAt
	now = now.Add(30 * time.Minute)
	if err := s.Put(key, legacy); err != nil || writes != 1 {
		t.Fatalf("unchanged replay wrote: writes=%d err=%v", writes, err)
	}
	enriched := QuoteText{Text: legacy.Text, Kinds: []string{"text", "voice"}}
	if err := s.Put(key, enriched); err != nil || writes != 2 {
		t.Fatalf("kinds enrichment not saved: writes=%d err=%v", writes, err)
	}
	if s.records[key].SavedAt != savedAt {
		t.Fatal("kinds enrichment renewed message retention")
	}
	if err := s.Put(key, enriched); err != nil || writes != 2 {
		t.Fatalf("semantic replay wrote: writes=%d err=%v", writes, err)
	}
	now = now.Add(31 * time.Minute)
	if _, ok, err := s.Get(key); err != nil || ok {
		t.Fatalf("enriched record outlived original deadline: %v %v", ok, err)
	}
}

func TestQuoteKindsFailedEnrichmentDoesNotPublish(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), 0, 0)
	key := QuoteKey{"account", "peer", "id"}
	if err := s.Put(key, QuoteText{Text: "caption", Kinds: []string{"text"}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("disk unavailable")
	s.write = func(string, any) error { return failure }
	if err := s.Put(key, QuoteText{Text: "caption", Kinds: []string{"text", "voice"}}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	got, ok, err := s.Get(key)
	if err != nil || !ok || !slices.Equal(got.Kinds, []string{"text"}) {
		t.Fatalf("uncommitted kinds published: %+v %v %v", got, ok, err)
	}
	after, err := os.ReadFile(s.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("uncommitted kinds replaced disk data: %v", err)
	}
}

func TestQuoteKindsEnrichmentDoesNotReviveMedia(t *testing.T) {
	for _, expired := range []string{"message", "payload"} {
		t.Run(expired, func(t *testing.T) {
			retention, mediaRetention := time.Hour, 24*time.Hour
			if expired == "payload" {
				retention, mediaRetention = mediaRetention, retention
			}
			s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), retention, 0)
			s.ConfigureMedia(QuoteMediaPolicy{Retention: mediaRetention})
			now := time.Unix(10000, 0)
			s.now = func() time.Time { return now }
			key := QuoteKey{"account", "peer", "old"}
			if err := s.PutMedia(key, QuoteMedia{Type: "file"}, []byte("old bytes")); err != nil {
				t.Fatal(err)
			}
			now = now.Add(2 * time.Hour)
			if err := s.Put(key, QuoteText{Kinds: []string{"file"}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.ReadMedia(key); !errors.Is(err, ErrQuoteMediaUnavailable) {
				t.Fatalf("metadata update revived expired %s: %v", expired, err)
			}
			entries, err := os.ReadDir(s.MediaDir())
			if err != nil || len(entries) != 0 {
				t.Fatalf("expired payload survived cleanup: %v %v", entries, err)
			}
		})
	}
}

func TestQuoteKindsLegacySnapshotAndBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	legacy := fmt.Sprintf(`{"version":1,"records":[{"key":{"account":"a","peer":"p","message":"m"},"text":"legacy","saved_at":%d}]}`, time.Now().UnixMilli())
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewQuoteStore(path, 0, 0)
	key := QuoteKey{"a", "p", "m"}
	if got, ok, err := s.Get(key); err != nil || !ok || got.Text != "legacy" || got.Kinds != nil {
		t.Fatalf("version 1 text-only snapshot unreadable: %+v %v %v", got, ok, err)
	}
	tooMany := make([]string, maxQuoteKinds+1)
	for i := range tooMany {
		tooMany[i] = "image"
	}
	for _, kinds := range [][]string{tooMany, {strings.Repeat("x", maxQuoteKindBytes+1)}, {""}} {
		if err := s.Put(key, QuoteText{Text: "invalid", Kinds: kinds}); err == nil {
			t.Fatal("invalid kinds accepted")
		}
	}
	var records []quoteRecord
	kinds := make([]string, maxQuoteKinds)
	for i := range kinds {
		kinds[i] = strings.Repeat("x", maxQuoteKindBytes)
	}
	if err := s.Put(key, QuoteText{Kinds: kinds}); err != nil {
		t.Fatalf("valid metadata at both size boundaries rejected: %v", err)
	}
	for i := range 256 {
		records = append(records, quoteRecord{Key: QuoteKey{"account", "peer", fmt.Sprint(i)}, Text: strings.Repeat("x", maxQuoteTextBytes-256), Kinds: kinds, SavedAt: time.Now().UnixMilli()})
	}
	if got := len(s.prune(records)); got == 0 || got >= 256 {
		t.Fatalf("semantic metadata not counted in byte budget: %d records", got)
	}
}
