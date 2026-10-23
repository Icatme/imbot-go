package wechatbot

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/Icatme/wechatbot-go/internal/remote"
	"github.com/Icatme/wechatbot-go/internal/store"
)

func quotedMediaFromItem(item *MessageItem) *QuotedMedia {
	if item == nil {
		return nil
	}
	var m *QuotedMedia
	switch item.Type {
	case ItemImage:
		if v := item.ImageItem; v != nil {
			m = &QuotedMedia{Type: ContentImage, Media: v.Media, AESKey: v.AESKey, URL: v.URL}
		}
	case ItemFile:
		if v := item.FileItem; v != nil {
			size, _ := strconv.ParseInt(v.Len, 10, 64)
			if size < 0 {
				size = 0
			}
			m = &QuotedMedia{Type: ContentFile, Media: v.Media, FileName: v.FileName, Size: size,
				MIMEType: mime.TypeByExtension(filepath.Ext(v.FileName))}
		}
	case ItemVideo:
		if v := item.VideoItem; v != nil {
			m = &QuotedMedia{Type: ContentVideo, Media: v.Media}
		}
	case ItemVoice:
		if v := item.VoiceItem; v != nil {
			m = &QuotedMedia{Type: ContentVoice, Media: v.Media, MIMEType: "audio/silk", Format: "silk"}
		}
	}
	if m != nil && m.Media != nil {
		copy := *m.Media
		m.Media = &copy
	}
	return m
}

func wireQuoteID(wire *WireMessage) MessageID {
	if wire == nil {
		return ""
	}
	if id := normalizeQuoteID(wire.MessageID); id != "" {
		return id
	}
	for _, item := range wire.ItemList {
		if id := normalizeQuoteID(item.MsgID); id != "" {
			return id
		}
	}
	return ""
}

func (b *Bot) cacheQuoteMedia(account, peer string, id MessageID, m *QuotedMedia, data []byte) {
	id = normalizeQuoteID(id)
	if b.quoteStore == nil || account == "" || peer == "" || id == "" || m == nil {
		return
	}
	if err := b.validateCurrentStatePaths(); err != nil {
		b.reportError(&QuoteCacheError{Cause: err})
		return
	}
	err := b.quoteStore.PutMedia(store.QuoteKey{Account: account, Peer: peer, Message: string(id)}, store.QuoteMedia{
		Type: string(m.Type), FileName: m.FileName, MIMEType: m.MIMEType, Size: m.Size, Format: m.Format,
	}, data)
	if err != nil {
		b.reportError(&QuoteCacheError{Cause: err})
	}
}

func downloadedMIME(m *DownloadedMedia) string {
	if m.Type == "voice" {
		return "audio/silk"
	}
	if m.FileName != "" {
		if value := mime.TypeByExtension(filepath.Ext(m.FileName)); value != "" {
			return value
		}
	}
	return http.DetectContentType(m.Data)
}

func (b *Bot) cacheDownloadedMedia(msg *IncomingMessage, data *DownloadedMedia) {
	if msg.Raw == nil {
		return
	}
	generation, err := b.incomingSessionGeneration(msg)
	if err != nil {
		return
	} // an old message must never write into a fresh session
	b.cacheQuoteMedia(b.quoteAccount(generation), peerUserID(msg.Raw), wireQuoteID(msg.Raw), &QuotedMedia{
		Type: ContentType(data.Type), FileName: data.FileName, MIMEType: data.MIMEType,
		Size: int64(len(data.Data)), Format: data.Format,
	}, data.Data)
}

// DownloadQuoted explicitly reads the referenced attachment, including an
// ID-only reference to retained media. It never downloads the current message's
// attachment. Missing, expired, evicted, or disabled media matches
// ErrQuoteMediaUnavailable. Cache failures do not become remote retries.
func (b *Bot) DownloadQuoted(ctx context.Context, msg *IncomingMessage) (*DownloadedMedia, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, fmt.Errorf("inbound message is nil")
	}
	if msg.QuotedMessage == nil {
		return nil, nil
	}
	if msg.QuotedMessage.Media == nil {
		switch msg.QuotedMessage.Resolution {
		case QuoteMissing, QuoteDisabled, QuoteUnavailable:
			return nil, ErrQuoteMediaUnavailable
		}
		return nil, nil
	}
	generation, err := b.incomingSessionGeneration(msg)
	if err != nil {
		return nil, err
	}
	q, m := msg.QuotedMessage, msg.QuotedMessage.Media
	key := store.QuoteKey{Account: b.quoteAccount(generation), Peer: msg.UserID, Message: string(normalizeQuoteID(q.MessageID))}
	if m.Cached || (m.Media == nil && m.URL == "") {
		if b.quoteStore == nil {
			return nil, ErrQuoteMediaUnavailable
		}
		if err := b.validateCurrentStatePaths(); err != nil {
			return nil, &QuoteCacheError{Cause: err}
		}
		data, info, err := b.quoteStore.ReadMedia(key)
		if errors.Is(err, store.ErrQuoteMediaUnavailable) {
			return nil, ErrQuoteMediaUnavailable
		}
		if err != nil {
			return nil, &QuoteCacheError{Cause: err}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := b.readySession(generation); err != nil {
			return nil, err
		}
		return &DownloadedMedia{Data: data, Type: info.Type, FileName: info.FileName, MIMEType: info.MIMEType, Format: info.Format}, nil
	}
	var data []byte
	if m.Media != nil {
		data, err = b.cdnDownload(ctx, m.Media, m.AESKey)
	} else {
		data, _, err = remote.DownloadWithClient(ctx, b.client.HTTP, m.URL)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := b.readySession(generation); err != nil {
		return nil, err
	}
	result := &DownloadedMedia{Data: data, Type: string(m.Type), FileName: m.FileName, Format: m.Format}
	result.MIMEType = downloadedMIME(result)
	retained := *m
	retained.MIMEType, retained.Size = result.MIMEType, int64(len(data))
	b.cacheQuoteMedia(key.Account, key.Peer, MessageID(key.Message), &retained, data)
	return result, nil
}

// PruneQuoteCache applies retention and capacity limits and removes orphaned
// SDK-owned media files. Call during idle periods if prompt disk cleanup is needed.
func (b *Bot) PruneQuoteCache() error {
	if b.quoteStore == nil {
		return nil
	}
	if err := b.validateCurrentStatePaths(); err != nil {
		return &QuoteCacheError{Cause: err}
	}
	if err := b.quoteStore.Prune(); err != nil {
		return &QuoteCacheError{Cause: err}
	}
	return nil
}
