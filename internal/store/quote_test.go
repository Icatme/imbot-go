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

func TestQuoteStoreRestartAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	s := NewQuoteStore(path, 0, 0)
	key := QuoteKey{"account-a", "peer-a", "18446744073709551615"}
	if err := s.Put(key, " 原文\n"); err != nil {
		t.Fatal(err)
	}
	restarted := NewQuoteStore(path, 0, 0)
	if text, ok, err := restarted.Get(key); err != nil || !ok || text != " 原文\n" {
		t.Fatalf("get=%q %v %v", text, ok, err)
	}
	for _, other := range []QuoteKey{{"account-b", key.Peer, key.Message}, {key.Account, "peer-b", key.Message}} {
		if _, ok, err := restarted.Get(other); ok || err != nil {
			t.Fatalf("cross-scope get=%v %v", ok, err)
		}
	}
}

func TestQuoteStoreRetentionCapacityAndReplay(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), time.Hour, 2)
	now := time.Unix(10000, 0)
	s.now = func() time.Time { return now }
	key := QuoteKey{"account", "peer", "1"}
	if err := s.Put(key, "first"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if err := s.Put(key, "first"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Minute)
	if _, ok, err := s.Get(key); ok || err != nil {
		t.Fatalf("replay extended retention: %v %v", ok, err)
	}
	for i := 2; i <= 4; i++ {
		now = now.Add(time.Second)
		if err := s.Put(QuoteKey{key.Account, key.Peer, fmt.Sprint(i)}, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.records) != 2 {
		t.Fatalf("records=%d", len(s.records))
	}
	if _, ok, _ := s.Get(QuoteKey{key.Account, key.Peer, "2"}); ok {
		t.Fatal("oldest record not evicted")
	}
	if err := s.Put(QuoteKey{"other", key.Peer, "5"}, "other account"); err != nil {
		t.Fatal(err)
	}
	if len(s.records) != 3 {
		t.Fatal("capacity must apply per account")
	}
}

func TestQuoteStoreFailedWriteDoesNotPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	s := NewQuoteStore(path, 0, 0)
	key := QuoteKey{"account", "peer", "1"}
	if err := s.Put(key, "accepted"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("disk unavailable")
	s.write = func(string, any) error { return want }
	if err := s.Put(key, "uncommitted"); !errors.Is(err, want) {
		t.Fatalf("write error=%v", err)
	}
	if text, ok, err := s.Get(key); text != "accepted" || !ok || err != nil {
		t.Fatalf("get=%q %v %v", text, ok, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed write replaced disk data")
	}
}

func TestQuoteStoreCorruptFileRemainsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	broken := []byte(`{"records": [private text`)
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	s := NewQuoteStore(path, 0, 0)
	key := QuoteKey{"account", "peer", "1"}
	if _, _, err := s.Get(key); err == nil || strings.Contains(err.Error(), "private text") {
		t.Fatalf("load=%v", err)
	}
	if err := s.Put(key, "new"); err == nil {
		t.Fatal("corrupt snapshot overwritten")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(broken, after) {
		t.Fatal("corrupt snapshot changed")
	}
}

func TestQuoteStoreConcurrentWritesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	s := NewQuoteStore(path, 0, 0)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := QuoteKey{"account", fmt.Sprint(i), "1"}
			if err := s.Put(key, "text"); err != nil {
				t.Error(err)
			}
			if _, ok, err := s.Get(key); !ok || err != nil {
				t.Errorf("get=%v %v", ok, err)
			}
		}()
	}
	wg.Wait()
	restarted := NewQuoteStore(path, 0, 0)
	for i := range 20 {
		if _, ok, err := restarted.Get(QuoteKey{"account", fmt.Sprint(i), "1"}); !ok || err != nil {
			t.Fatalf("lost record %d: %v %v", i, ok, err)
		}
	}
}

func TestQuoteStoreBounds(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), 0, 0)
	key := QuoteKey{"account", "peer", "1"}
	if err := s.Put(key, strings.Repeat("x", maxQuoteTextBytes+1)); err == nil {
		t.Fatal("oversized message accepted")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatalf("oversized message wrote file: %v", err)
	}
	var records []quoteRecord
	for i := range 300 {
		records = append(records, quoteRecord{Key: QuoteKey{"account", "peer", fmt.Sprint(i)}, Text: strings.Repeat("x", maxQuoteTextBytes), SavedAt: time.Now().UnixMilli()})
	}
	pruned := s.prune(records)
	if len(pruned) >= 256 {
		t.Fatalf("text budget not applied: %d records", len(pruned))
	}
}

func TestQuoteStoreKeepsNewWriteWhenTimestampsTie(t *testing.T) {
	s := NewQuoteStore(filepath.Join(t.TempDir(), "quotes.json"), 0, 1)
	s.now = func() time.Time { return time.Unix(10000, 0) }
	for _, id := range []string{"1", "2"} {
		if err := s.Put(QuoteKey{"account", "peer", id}, id); err != nil {
			t.Fatal(err)
		}
	}
	if text, ok, err := s.Get(QuoteKey{"account", "peer", "2"}); text != "2" || !ok || err != nil {
		t.Fatalf("new write evicted: %q %v %v", text, ok, err)
	}
}
