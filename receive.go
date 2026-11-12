package imbot

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Icatme/imbot-go/receive"
)

func (b *Bot) receiveMessage(ctx context.Context, handler MessageHandler, msg *IncomingMessage) MessageResult {
	if err := b.opts.ReceivePolicy.Check(nativeReceiveContent(msg)); err != nil {
		var rejected *receive.Rejection
		if !errors.As(err, &rejected) {
			return RetryMessage(err)
		}
		// Reuse the handler panic boundary; a broken application callback must
		// not crash the receive loop or commit the message as successfully dropped.
		return b.invokeHandler(ctx, MessageHandlerFunc(func(ctx context.Context, msg *IncomingMessage) MessageResult {
			if b.opts.OnReceiveRejected != nil {
				b.opts.OnReceiveRejected(ctx, msg, rejected)
			}
			return DropMessage(rejected)
		}), msg)
	}
	b.cacheInboundQuote(msg.sessionGeneration, msg.Raw)
	if err := b.hooks.AfterReceive.Run(msg); err != nil {
		return RetryMessage(fmt.Errorf("AfterReceive hook failed: %w", err))
	}
	return b.invokeHandler(ctx, handler, msg)
}

func nativeReceiveContent(msg *IncomingMessage) receive.Content {
	var c receive.Content
	var primaryReference *MessageItem
	if msg.Raw != nil {
		for _, item := range msg.Raw.ItemList {
			if item.RefMsg != nil {
				primaryReference = item.RefMsg.MessageItem
			}
		}
	}
	seen := make(map[*MessageItem]bool)
	var addItem func(*MessageItem, bool)
	addItem = func(item *MessageItem, quoted bool) {
		if item == nil || seen[item] {
			return
		}
		seen[item] = true
		c.Kinds = append(c.Kinds, messageContentKinds([]MessageItem{*item})...)
		if item.Type == ItemFile && item.FileItem == nil && item != primaryReference {
			c.Attachments = append(c.Attachments, receive.Attachment{Kind: receive.File})
		}
		if f := item.FileItem; f != nil {
			c.Kinds = append(c.Kinds, receive.File)
			// An ID-only quote placeholder has no attachment yet; resolved cache
			// metadata below supplies it when available.
			if !quoted || item != primaryReference || f.Media != nil || f.FileName != "" || f.Len != "" {
				c.Attachments = append(c.Attachments, receive.Attachment{Kind: receive.File, Name: f.FileName})
			}
		}
		if ref := item.RefMsg; ref != nil {
			addItem(ref.MessageItem, true)
		}
	}
	if msg.Raw != nil {
		for i := range msg.Raw.ItemList {
			addItem(&msg.Raw.ItemList[i], false)
		}
	}
	if q := msg.QuotedMessage; q != nil {
		if len(q.ContentKinds) == 0 {
			c.Kinds = append(c.Kinds, receive.Kind(q.Type))
		}
		c.Kinds = append(c.Kinds, q.ContentKinds...)
		// Include missing metadata too: a known file quote without a filename
		// must not bypass an extension allowlist when the cache cannot restore it.
		if q.Media != nil {
			c.Attachments = append(c.Attachments, receive.Attachment{Kind: receive.Kind(q.Media.Type), Name: q.Media.FileName, MIME: q.Media.MIMEType})
		} else if q.Type == ContentFile {
			c.Attachments = append(c.Attachments, receive.Attachment{Kind: receive.File})
		}
	}
	return c
}

// messageContentKinds records the current message's semantic types before the
// quote cache flattens text and retains only one attachment. References belong
// to another message and are intentionally excluded here.
func messageContentKinds(items []MessageItem) []receive.Kind {
	var kinds []receive.Kind
	add := func(kind receive.Kind) {
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	for _, item := range items {
		switch item.Type {
		case ItemText:
			add(receive.Text)
		case ItemImage:
			add(receive.Image)
		case ItemVoice:
			add(receive.Voice)
		case ItemFile:
			add(receive.File)
		case ItemVideo:
			add(receive.Video)
		case ItemToolCallStart, ItemToolCallResult:
			add(receive.ToolCall)
		}
		// The native parser also exposes populated media fields whose Type is
		// inconsistent. Include them instead of letting them bypass the policy.
		if item.ImageItem != nil {
			add(receive.Image)
		}
		if item.VoiceItem != nil {
			add(receive.Voice)
		}
		if item.FileItem != nil {
			add(receive.File)
		}
		if item.VideoItem != nil {
			add(receive.Video)
		}
	}
	return kinds
}
