package gateway

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// BatchReceipt records exactly which operations the platform accepted. A batch
// is not atomic, and callers must not replay its successful prefix on failure.
type BatchReceipt struct{ Receipts []Receipt }
type PartialSendError struct {
	Completed int
	Cause     error
}

func (e *PartialSendError) Error() string {
	return fmt.Sprintf("gateway: send stopped after %d accepted messages: %v", e.Completed, e.Cause)
}
func (e *PartialSendError) Unwrap() error { return e.Cause }

// SendBatch is explicit, ordered multi-message delivery. It stops at the first
// failure, preserves uncertainty on that operation, and never retries or retargets.
func (g *Gateway) SendBatch(ctx context.Context, r Route, messages []Message) (BatchReceipt, error) {
	out := BatchReceipt{}
	if len(messages) == 0 {
		return out, ErrInvalidEvent
	}
	for _, m := range messages {
		receipt, err := g.Send(ctx, r, m)
		if err != nil {
			return out, &PartialSendError{Completed: len(out.Receipts), Cause: err}
		}
		if !receipt.Accepted {
			return out, &PartialSendError{Completed: len(out.Receipts), Cause: &APIError{Platform: r.Platform, Code: "missing_acceptance", Uncertain: true}}
		}
		out.Receipts = append(out.Receipts, receipt)
	}
	return out, nil
}

// SendText explicitly opts into long-text splitting. It preserves every UTF-8
// byte and reply route. Markdown remains raw Markdown; no lossy rich-text rewrite
// is attempted. Use Send for one atomic platform operation.
func (g *Gateway) SendText(ctx context.Context, r Route, m Message) (BatchReceipt, error) {
	if err := ctx.Err(); err != nil {
		return BatchReceipt{}, err
	}
	if r.Validate() != nil || m.Text == "" || !utf8.ValidString(m.Text) || len(m.Attachments) != 0 || len(m.Card) != 0 || m.Sequence < 0 {
		return BatchReceipt{}, ErrInvalidEvent
	}
	a, err := g.adapter(r.Account)
	if err != nil {
		return BatchReceipt{}, err
	}
	limit := a.Capabilities().MaxTextBytes
	if limit < utf8.UTFMax {
		return BatchReceipt{}, ErrUnsupported
	}
	var messages []Message
	for rest := m.Text; len(rest) > 0; {
		n := min(len(rest), limit)
		if n < len(rest) {
			for n > 0 && !utf8.RuneStart(rest[n]) {
				n--
			}
		}
		part := m
		part.Text = rest[:n]
		if m.ClientID != "" {
			part.ClientID = m.ClientID + "/" + strconv.Itoa(len(messages))
		}
		if m.Sequence > 0 {
			if len(messages) > math.MaxInt-m.Sequence {
				return BatchReceipt{}, ErrInvalidEvent
			}
			part.Sequence = m.Sequence + len(messages)
		} else if r.Platform == "qqbot" {
			part.Sequence = 1 + len(messages)
		}
		// The QQ adapter accepts a signed 32-bit msg_seq. Validate every part
		// before sending the batch, rather than failing after an accepted prefix.
		if r.Platform == "qqbot" && part.Sequence > math.MaxInt32 {
			return BatchReceipt{}, ErrInvalidEvent
		}
		messages = append(messages, part)
		rest = rest[n:]
	}
	return g.SendBatch(ctx, r, messages)
}
