package store

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Icatme/wechatbot-go/internal/persist"
)

const (
	DefaultQuoteRetention = 30 * 24 * time.Hour
	DefaultQuoteMessages  = 10_000
	// Bound snapshot writes and memory even when every message is large.
	maxQuoteTextBytes = 64 * 1024
	maxQuoteBytes     = 16 * 1024 * 1024
	maxQuoteFileBytes = 128 * 1024 * 1024 // includes JSON escaping and metadata
)

// QuoteKey scopes server IDs to the authenticated account and remote peer.
type QuoteKey struct {
	Account string `json:"account"`
	Peer    string `json:"peer"`
	Message string `json:"message"`
}

type quoteRecord struct {
	Key     QuoteKey    `json:"key"`
	Text    string      `json:"text"`
	SavedAt int64       `json:"saved_at"`
	Media   *QuoteMedia `json:"media,omitempty"`
}

type quoteData struct {
	Version int           `json:"version"`
	Records []quoteRecord `json:"records"`
}

// QuoteStore is a bounded, atomic snapshot owned by one Bot. Its contents are
// optional context, never credentials or the authoritative delivery journal.
type QuoteStore struct {
	path        string
	retention   time.Duration
	maxMessages int
	mu          sync.Mutex
	loaded      bool
	loadErr     error
	records     map[QuoteKey]quoteRecord
	now         func() time.Time
	write       func(string, any) error
	mediaPolicy QuoteMediaPolicy
}

func NewQuoteStore(path string, retention time.Duration, maxMessages int) *QuoteStore {
	if retention <= 0 {
		retention = DefaultQuoteRetention
	}
	if maxMessages <= 0 {
		maxMessages = DefaultQuoteMessages
	}
	return &QuoteStore{path: path, retention: retention, maxMessages: maxMessages,
		records: make(map[QuoteKey]quoteRecord), now: time.Now, write: persist.WriteJSONAtomic,
		mediaPolicy: DefaultQuoteMediaPolicy()}
}

func (s *QuoteStore) Path() string { return s.path }

func validQuoteKey(key QuoteKey) bool {
	return key.Account != "" && key.Peer != "" && key.Message != "" && key.Message != "0" &&
		len(key.Account) <= 1024 && len(key.Peer) <= 1024 && len(key.Message) <= 1024
}

// loadLocked loads once. A corrupt or unreadable snapshot stays unavailable;
// later writes must never replace it with a silently empty cache.
func (s *QuoteStore) loadLocked() error {
	if s.loaded {
		return s.loadErr
	}
	s.loaded = true
	s.loadErr = s.readLocked()
	return s.loadErr
}

func (s *QuoteStore) readLocked() error {
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read quote cache: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxQuoteFileBytes+1))
	if err != nil {
		return fmt.Errorf("read quote cache: %w", err)
	}
	if len(raw) > maxQuoteFileBytes {
		return fmt.Errorf("quote cache exceeds size limit")
	}
	var data quoteData
	if err := json.Unmarshal(raw, &data); err != nil {
		// Do not expose the decoder's input fragment; it may contain message text.
		return fmt.Errorf("invalid quote cache JSON")
	}
	if data.Version != 1 {
		return fmt.Errorf("unsupported quote cache version %d", data.Version)
	}
	for _, record := range s.prune(data.Records) {
		s.records[record.Key] = record
	}
	return nil
}

func (s *QuoteStore) Get(key QuoteKey) (string, bool, error) {
	if !validQuoteKey(key) {
		return "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return "", false, err
	}
	record, ok := s.records[key]
	if !ok || record.SavedAt < s.now().Add(-s.retention).UnixMilli() {
		return "", false, nil
	}
	return record.Text, true, nil
}

func (s *QuoteStore) Put(key QuoteKey, text string) error {
	if !validQuoteKey(key) || strings.TrimSpace(text) == "" {
		return nil
	}
	if len(text) > maxQuoteTextBytes {
		return fmt.Errorf("quote text exceeds 64 KiB limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	now := s.now().UnixMilli()
	if old, ok := s.records[key]; ok && old.Text == text && old.SavedAt >= s.now().Add(-s.retention).UnixMilli() {
		return nil // a replay must not extend retention or cause another disk write
	}
	next := make([]quoteRecord, 0, len(s.records)+1)
	record := quoteRecord{Key: key, Text: text, SavedAt: now}
	if old, ok := s.records[key]; ok {
		record.Media = old.Media
	}
	next = append(next, record)
	for oldKey, record := range s.records {
		if oldKey != key {
			next = append(next, record)
		}
	}
	return s.saveLocked(next)
}

func (s *QuoteStore) saveLocked(next []quoteRecord) error {
	next = s.prune(next)
	if err := s.write(s.path, quoteData{Version: 1, Records: next}); err != nil {
		return fmt.Errorf("persist quote cache: %w", err)
	}
	records := make(map[QuoteKey]quoteRecord, len(next))
	for _, record := range next {
		records[record.Key] = record
	}
	s.records = records // publish only after the atomic replacement succeeds
	return s.cleanupMediaLocked()
}

func (s *QuoteStore) prune(records []quoteRecord) []quoteRecord {
	cutoff := s.now().Add(-s.retention).UnixMilli()
	records = slices.DeleteFunc(records, func(r quoteRecord) bool {
		return !validQuoteKey(r.Key) || (r.Text == "" && r.Media == nil) || len(r.Text) > maxQuoteTextBytes || r.SavedAt < cutoff || !validQuoteMedia(r.Media)
	})
	// Prefer the newly inserted record when multiple writes share a millisecond.
	slices.SortStableFunc(records, func(a, b quoteRecord) int {
		if a.SavedAt > b.SavedAt {
			return -1
		}
		if a.SavedAt < b.SavedAt {
			return 1
		}
		return 0
	})
	counts := make(map[string]int)
	bytes := 0
	records = slices.DeleteFunc(records, func(r quoteRecord) bool {
		size := len(r.Text) + len(r.Key.Account) + len(r.Key.Peer) + len(r.Key.Message)
		if r.Media != nil {
			size += len(r.Media.FileName) + len(r.Media.MIMEType) + 256
		}
		if counts[r.Key.Account] >= s.maxMessages || bytes+size > maxQuoteBytes {
			return true
		}
		counts[r.Key.Account]++
		bytes += size
		return false
	})
	s.pruneMedia(records)
	return records
}
