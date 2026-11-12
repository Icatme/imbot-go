package imbot

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"time"

	"github.com/Icatme/imbot-go/internal/store"
)

// QuoteCacheOptions controls local reference history. The zero value enables
// 30-day text history (10,000 records/account, 16 MiB metadata, 64 KiB/text), plus
// 7-day media retention (256 MiB/account, 25 MiB/file). Only explicitly downloaded
// or successfully sent payloads are retained; polling never downloads media.
// Path defaults to <CredPath>.quotes.json; media is stored in Path + ".media".
// A cache path must have one owning Bot/process, just like other state files.
type QuoteCacheOptions struct {
	Disabled            bool
	Path                string
	Retention           time.Duration
	MaxMessages         int
	MediaDisabled       bool          // retain metadata but no payloads
	MediaRetention      time.Duration // defaults to 7 days
	MaxMediaBytes       int64         // per account; defaults to 256 MiB
	MaxSingleMediaBytes int64         // defaults to 25 MiB
}

// QuoteResolution distinguishes restored text from an unresolved reference.
type QuoteResolution string

const (
	QuoteEmbedded       QuoteResolution = "embedded"
	QuoteCached         QuoteResolution = "cached"
	QuoteMissing        QuoteResolution = "missing"
	QuoteDisabled       QuoteResolution = "disabled"
	QuoteUnavailable    QuoteResolution = "unavailable"
	QuoteInvalidPartial QuoteResolution = "invalid_partial"
)

func (b *Bot) quoteAccount(generation uint64) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.creds == nil || b.reauth != nil || b.sessionGeneration != generation {
		return ""
	}
	return b.creds.AccountID
}

func (b *Bot) cacheInboundQuote(generation uint64, wire *WireMessage) {
	id := wireQuoteID(wire)
	b.cacheQuote(b.quoteAccount(generation), peerUserID(wire), id, quoteText(wire.ItemList))
	for _, item := range wire.ItemList {
		if media := quotedMediaFromItem(&item); media != nil {
			b.cacheQuoteMedia(b.quoteAccount(generation), peerUserID(wire), id, media, nil)
			break
		}
	}
}

func (b *Bot) cacheQuote(account, peer string, id MessageID, text string) {
	id = normalizeQuoteID(id)
	if b.quoteStore == nil || account == "" || peer == "" || id == "" || text == "" {
		return
	}
	// Validate before lazy IO too, so direct SDK sends cannot overwrite another
	// state file when a caller accidentally configures colliding paths.
	if err := b.validateCurrentStatePaths(); err != nil {
		b.reportError(&QuoteCacheError{Cause: err})
		return
	}
	if err := b.quoteStore.Put(store.QuoteKey{Account: account, Peer: peer, Message: string(id)}, text); err != nil {
		b.reportError(&QuoteCacheError{Cause: err})
	}
}

func (b *Bot) resolveQuote(generation uint64, wire *WireMessage, incoming *IncomingMessage) {
	q := incoming.QuotedMessage
	if q == nil {
		return
	}
	if q.Resolution == QuoteMissing && q.MessageID != "" {
		if b.quoteStore == nil {
			q.Resolution = QuoteDisabled
		} else if err := b.validateCurrentStatePaths(); err != nil {
			q.Resolution = QuoteUnavailable
			b.reportError(&QuoteCacheError{Cause: err})
		} else {
			text, ok, err := b.quoteStore.Get(store.QuoteKey{
				Account: b.quoteAccount(generation), Peer: peerUserID(wire), Message: string(q.MessageID),
			})
			if err != nil {
				q.Resolution = QuoteUnavailable
				b.reportError(&QuoteCacheError{Cause: err})
				return
			} else if ok {
				q.Text, q.Type, q.Resolution = text, ContentText, QuoteCached
			}
			media, found, mediaErr := b.quoteStore.GetMedia(store.QuoteKey{
				Account: b.quoteAccount(generation), Peer: peerUserID(wire), Message: string(q.MessageID),
			})
			if mediaErr != nil {
				q.Resolution = QuoteUnavailable
				b.reportError(&QuoteCacheError{Cause: mediaErr})
			} else if found {
				q.Type, q.Resolution = ContentType(media.Type), QuoteCached
				q.Media = &QuotedMedia{Type: q.Type, FileName: media.FileName, MIMEType: media.MIMEType,
					Size: media.Size, Format: media.Format, Cached: media.File != ""}
			}
		}
	}
	if q.PartialText != nil && q.Text != "" {
		selected, ok := resolvePartialQuote(q.Text, *q.PartialText)
		if !ok {
			q.Text = ""
			q.Resolution = QuoteInvalidPartial
			return
		}
		q.Text = selected
	}
}

func normalizeQuoteID(id MessageID) MessageID {
	id = MessageID(strings.TrimSpace(string(id)))
	if id == "0" {
		return ""
	}
	return id
}

func quoteReferenceID(ref *RefMessage) MessageID {
	if id := normalizeQuoteID(ref.ServerID); id != "" {
		return id
	}
	if ref.MessageItem != nil {
		return normalizeQuoteID(ref.MessageItem.MsgID)
	}
	return ""
}

// An ID/type placeholder carries no inline content. Media references remain
// embedded even though text-only quote restoration does not download them.
func hasInlineQuoteMedia(item *MessageItem) bool {
	switch item.Type {
	case ItemImage:
		media := item.ImageItem
		return media != nil && (media.Media != nil || media.ThumbMedia != nil || media.URL != "")
	case ItemVoice:
		return item.VoiceItem != nil && item.VoiceItem.Media != nil
	case ItemFile:
		return item.FileItem != nil && (item.FileItem.Media != nil || item.FileItem.FileName != "")
	case ItemVideo:
		return item.VideoItem != nil && (item.VideoItem.Media != nil || item.VideoItem.ThumbMedia != nil)
	default:
		return false
	}
}

// Keep the exact text for substring hashing, including leading/trailing spaces.
func quoteText(items []MessageItem) string {
	var parts []string
	for _, item := range items {
		switch item.Type {
		case ItemText:
			if item.TextItem != nil {
				parts = append(parts, item.TextItem.Text)
			}
		case ItemVoice:
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				parts = append(parts, item.VoiceItem.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// Resolve the two observed end-index interpretations only when MD5 can
// disambiguate them. Without a hash, use the protocol's global indexes.
func resolvePartialQuote(text string, partial PartialText) (string, bool) {
	start := nthQuoteIndex(text, partial.Start, partial.StartIndex, 0)
	if start < 0 {
		return "", false
	}
	ends := []int{nthQuoteIndex(text, partial.End, partial.EndIndex, 0)}
	if partial.QuoteMD5 != "" {
		ends = append(ends, nthQuoteIndex(text, partial.End, partial.EndIndex, start+len(partial.Start)))
	}
	for _, end := range ends {
		if end < start {
			continue
		}
		candidate := text[start : end+len(partial.End)]
		if partial.QuoteMD5 == "" {
			return candidate, true
		}
		sum := md5.Sum([]byte(candidate))
		if strings.EqualFold(hex.EncodeToString(sum[:]), partial.QuoteMD5) {
			return candidate, true
		}
	}
	return "", false
}

func nthQuoteIndex(text, marker string, occurrence, from int) int {
	if marker == "" || occurrence < 0 || from > len(text) {
		return -1
	}
	for current := 0; current <= occurrence; current++ {
		index := strings.Index(text[from:], marker)
		if index < 0 {
			return -1
		}
		index += from
		if current == occurrence {
			return index
		}
		from = index + len(marker)
	}
	return -1
}
