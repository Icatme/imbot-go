package gateway

import (
	"slices"

	"github.com/Icatme/imbot-go/receive"
)

func (g *Gateway) receivePolicy(account Account) receive.Policy {
	if policy, ok := g.opts.AccountReceivePolicies[account]; ok {
		return policy
	}
	return g.opts.ReceivePolicy
}

func receiveContent(e Event) receive.Content {
	c := receive.Content{Kinds: append([]receive.Kind(nil), e.ContentKinds...)}
	addText := func(text string, kinds []receive.Kind) {
		// A voice transcript is text in the projection but remains Voice in the
		// source. An explicit kind list is authoritative for this distinction.
		if text != "" && len(kinds) == 0 {
			c.Kinds = append(c.Kinds, receive.Text)
		}
	}
	addAttachment := func(a Attachment) {
		kind := receive.Kind(a.Kind)
		if a.Kind == "audio" {
			kind = receive.Voice
		}
		c.Attachments = append(c.Attachments, receive.Attachment{Kind: kind, Name: a.Name, MIME: a.MIME})
	}
	addText(e.Text, e.ContentKinds)
	for _, a := range e.Attachments {
		addAttachment(a)
	}
	for _, part := range e.Parts {
		addText(part.Text, e.ContentKinds)
		if part.Attachment != nil {
			addAttachment(*part.Attachment)
		}
	}
	// A current file does not supply missing metadata for a quoted file (or
	// vice versa). Keep an explicit unknown descriptor for each such group.
	if slices.Contains(e.ContentKinds, receive.File) && !slices.ContainsFunc(c.Attachments, func(a receive.Attachment) bool { return a.Kind == receive.File }) {
		c.Attachments = append(c.Attachments, receive.Attachment{Kind: receive.File})
	}
	if e.Quote != nil {
		c.Kinds = append(c.Kinds, e.Quote.ContentKinds...)
		addText(e.Quote.Text, e.Quote.ContentKinds)
		for _, a := range e.Quote.Attachments {
			addAttachment(a)
		}
		if slices.Contains(e.Quote.ContentKinds, receive.File) && !slices.ContainsFunc(e.Quote.Attachments, func(a Attachment) bool { return a.Kind == "file" }) {
			c.Attachments = append(c.Attachments, receive.Attachment{Kind: receive.File})
		}
	}
	if e.Interaction != nil {
		c.Kinds = append(c.Kinds, receive.Interaction)
	}
	return c
}
