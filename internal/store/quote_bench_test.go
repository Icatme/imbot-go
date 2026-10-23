package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func BenchmarkQuoteStorePut(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := NewQuoteStore(filepath.Join(b.TempDir(), "quotes.json"), 0, count)
			s.loaded = true
			now := time.Now().UnixMilli()
			for i := range count {
				key := QuoteKey{"account", "peer", fmt.Sprint(i)}
				s.records[key] = quoteRecord{Key: key, Text: strings.Repeat("x", 128), SavedAt: now}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Put(QuoteKey{"account", "peer", "latest"}, fmt.Sprint(i)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
