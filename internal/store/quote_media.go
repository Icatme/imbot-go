package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

var ErrQuoteMediaUnavailable = errors.New("quote media unavailable")

// QuoteMedia contains display metadata and an SDK-owned file name, never CDN
// credentials. FileName is display-only and is never used as a disk path.
type QuoteMedia struct {
	Type     string `json:"type"`
	FileName string `json:"file_name,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Format   string `json:"format,omitempty"`
	Size     int64  `json:"size,omitempty"`
	File     string `json:"file,omitempty"`
	SavedAt  int64  `json:"saved_at,omitempty"`
}

type QuoteMediaPolicy struct {
	Disabled     bool
	Retention    time.Duration
	MaxBytes     int64
	MaxFileBytes int64
}

func DefaultQuoteMediaPolicy() QuoteMediaPolicy {
	return QuoteMediaPolicy{Retention: 7 * 24 * time.Hour, MaxBytes: 256 << 20, MaxFileBytes: 25 << 20}
}

// ConfigureMedia is called before the store is used.
func (s *QuoteStore) ConfigureMedia(policy QuoteMediaPolicy) {
	defaults := DefaultQuoteMediaPolicy()
	if policy.Retention <= 0 {
		policy.Retention = defaults.Retention
	}
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = defaults.MaxBytes
	}
	if policy.MaxFileBytes <= 0 {
		policy.MaxFileBytes = defaults.MaxFileBytes
	}
	s.mediaPolicy = policy
}

func (s *QuoteStore) MediaDir() string { return s.path + ".media" }

func validMediaFile(name string) bool {
	if !strings.HasPrefix(name, "media-") || len(name) < 12 || len(name) > 64 {
		return false
	}
	for _, c := range name[6:] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func validQuoteMedia(m *QuoteMedia) bool {
	if m == nil {
		return true
	}
	if m.Type != "image" && m.Type != "file" && m.Type != "video" && m.Type != "voice" {
		return false
	}
	return len(m.FileName) <= 4096 && len(m.MIMEType) <= 256 && len(m.Format) <= 32 && m.Size >= 0 && (m.File == "" || validMediaFile(m.File))
}

func (s *QuoteStore) mediaRoot(create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(s.MediaDir(), 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(s.MediaDir())
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("quote media root must be a directory, not a link")
	}
	return os.OpenRoot(s.MediaDir())
}

// GetMedia returns metadata even when no retained file is available. Such a
// record has an empty File, so consumers never mistake an expired file for a hit.
func (s *QuoteStore) GetMedia(key QuoteKey) (QuoteMedia, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return QuoteMedia{}, false, err
	}
	r, ok := s.records[key]
	if !ok || r.Media == nil || r.SavedAt < s.now().Add(-s.retention).UnixMilli() {
		return QuoteMedia{}, false, nil
	}
	m := *r.Media
	if s.mediaPolicy.Disabled || m.SavedAt < s.now().Add(-s.mediaPolicy.Retention).UnixMilli() {
		m.File = ""
	}
	if m.File != "" {
		root, err := s.mediaRoot(false)
		if os.IsNotExist(err) {
			m.File = ""
		} else if err != nil {
			return QuoteMedia{}, false, err
		} else {
			defer root.Close()
			info, err := root.Lstat(m.File)
			if os.IsNotExist(err) {
				m.File = ""
			} else if err != nil {
				return QuoteMedia{}, false, err
			} else if !info.Mode().IsRegular() || info.Size() != m.Size {
				return QuoteMedia{}, false, fmt.Errorf("invalid retained quote media file")
			}
		}
	}
	return m, true, nil
}

// PutMedia records metadata. Non-nil data retains a decrypted payload within
// policy limits. Metadata-only observations preserve an existing retained file.
func (s *QuoteStore) PutMedia(key QuoteKey, media QuoteMedia, data []byte) (resultErr error) {
	if !validQuoteKey(key) {
		return nil
	}
	media.File, media.SavedAt = "", 0
	if data != nil {
		media.Size = int64(len(data))
	}
	if !validQuoteMedia(&media) {
		return fmt.Errorf("invalid quote media metadata")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	record := quoteRecord{Key: key, SavedAt: s.now().UnixMilli(), Media: &media}
	if old, ok := s.records[key]; ok && old.SavedAt >= s.now().Add(-s.retention).UnixMilli() {
		record.Text, record.Kinds, record.SavedAt = old.Text, slices.Clone(old.Kinds), old.SavedAt
		if data == nil && old.Media != nil {
			// A metadata observation cannot replace the payload identified by key.
			// In particular, a replay may omit the display name added by Download.
			if old.Media.File != "" || (old.Media.Type == media.Type && old.Media.FileName == media.FileName) {
				return nil // preserve downloaded metadata and the original retention deadline
			}
		}
	}
	if data != nil && !s.mediaPolicy.Disabled && int64(len(data)) <= s.mediaPolicy.MaxFileBytes && int64(len(data)) <= s.mediaPolicy.MaxBytes {
		root, err := s.mediaRoot(true)
		if err != nil {
			return fmt.Errorf("open quote media root: %w", err)
		}
		defer root.Close()
		name := "media-" + rand.Text()
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		n, writeErr := file.Write(data)
		if writeErr == nil && n != len(data) {
			writeErr = io.ErrShortWrite
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		writeErr = errors.Join(writeErr, file.Close())
		if writeErr != nil {
			return errors.Join(writeErr, root.Remove(name))
		}
		media.File, media.SavedAt = name, s.now().UnixMilli()
		// A failed metadata commit must not strand a new payload.
		defer func() {
			if r, ok := s.records[key]; !ok || r.Media == nil || r.Media.File != name {
				if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
					resultErr = errors.Join(resultErr, fmt.Errorf("rollback quote media: %w", err))
				}
			}
		}()
	}
	next := []quoteRecord{record}
	for oldKey, old := range s.records {
		if oldKey != key {
			next = append(next, old)
		}
	}
	return s.saveLocked(next)
}

func (s *QuoteStore) pruneMedia(records []quoteRecord) {
	indices := make([]int, 0, len(records))
	for i := range records {
		if records[i].Media == nil {
			continue
		}
		copy := *records[i].Media
		records[i].Media = &copy
		if s.mediaPolicy.Disabled || copy.SavedAt < s.now().Add(-s.mediaPolicy.Retention).UnixMilli() || copy.Size > s.mediaPolicy.MaxFileBytes {
			records[i].Media.File = ""
		}
		if records[i].Media.File != "" {
			indices = append(indices, i)
		}
	}
	slices.SortStableFunc(indices, func(i, j int) int {
		if records[i].Media.SavedAt > records[j].Media.SavedAt {
			return -1
		}
		if records[i].Media.SavedAt < records[j].Media.SavedAt {
			return 1
		}
		return 0
	})
	bytes := make(map[string]int64)
	for _, i := range indices {
		r := &records[i]
		if r.Media.Size > s.mediaPolicy.MaxBytes-bytes[r.Key.Account] {
			r.Media.File = ""
		} else {
			bytes[r.Key.Account] += r.Media.Size
		}
	}
}

func (s *QuoteStore) ReadMedia(key QuoteKey) ([]byte, QuoteMedia, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, QuoteMedia{}, err
	}
	r, ok := s.records[key]
	if !ok || r.Media == nil || r.Media.File == "" || s.mediaPolicy.Disabled || r.SavedAt < s.now().Add(-s.retention).UnixMilli() || r.Media.SavedAt < s.now().Add(-s.mediaPolicy.Retention).UnixMilli() {
		return nil, QuoteMedia{}, ErrQuoteMediaUnavailable
	}
	root, err := s.mediaRoot(false)
	if os.IsNotExist(err) {
		return nil, QuoteMedia{}, ErrQuoteMediaUnavailable
	}
	if err != nil {
		return nil, QuoteMedia{}, err
	}
	defer root.Close()
	info, err := root.Lstat(r.Media.File)
	if os.IsNotExist(err) {
		return nil, QuoteMedia{}, ErrQuoteMediaUnavailable
	}
	if err != nil {
		return nil, QuoteMedia{}, err
	}
	if !info.Mode().IsRegular() || info.Size() != r.Media.Size || info.Size() > s.mediaPolicy.MaxFileBytes || info.Size() > s.mediaPolicy.MaxBytes {
		return nil, QuoteMedia{}, fmt.Errorf("invalid retained quote media file")
	}
	f, err := root.Open(r.Media.File)
	if err != nil {
		return nil, QuoteMedia{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, r.Media.Size+1))
	if err != nil {
		return nil, QuoteMedia{}, err
	}
	if int64(len(data)) != r.Media.Size {
		return nil, QuoteMedia{}, fmt.Errorf("retained quote media size changed")
	}
	return data, *r.Media, nil
}

func (s *QuoteStore) cleanupMediaLocked() error {
	root, err := s.mediaRoot(false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	retained := make(map[string]bool)
	for _, r := range s.records {
		if r.Media != nil && r.Media.File != "" {
			retained[r.Media.File] = true
		}
	}
	var errs []error
	for _, e := range entries {
		if validMediaFile(e.Name()) && !retained[e.Name()] && !e.IsDir() {
			if err := root.Remove(e.Name()); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Prune persists policy eviction and removes only SDK-owned, unreferenced files.
// Applications may invoke this during idle periods; writes also prune the cache.
func (s *QuoteStore) Prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	next := make([]quoteRecord, 0, len(s.records))
	for _, record := range s.records {
		next = append(next, record)
	}
	return s.saveLocked(next)
}
