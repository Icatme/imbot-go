package feishu

import (
	"strings"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

type messageResource struct{ MessageID, Key, Kind string }
type uploadedResource struct{ Key, Kind string }

const uploadRefPrefix = "feishu-upload:"

// Upload references are local capabilities bound to the complete original route.
// Explicit preuploaded native keys remain usable independently of this cache.
func (a *Adapter) resolveUploads(r gateway.Route, m gateway.Message) (gateway.Message, error) {
	attachments := append([]gateway.Attachment(nil), m.Attachments...)
	for i := range attachments {
		p := &attachments[i]
		if !strings.HasPrefix(p.Ref, uploadRefPrefix) {
			continue
		}
		resource, ok := a.uploads.Get(wire.ContextKey(r, p.Ref))
		if !ok {
			return gateway.Message{}, gateway.ErrMediaExpired
		}
		if p.Kind != resource.Kind {
			return gateway.Message{}, gateway.ErrInvalidEvent
		}
		p.Ref = resource.Key
	}
	m.Attachments = attachments
	return m, nil
}

func (a *Adapter) bindResources(e *gateway.Event) {
	expires := time.Now().Add(30 * time.Minute)
	for i := range e.Attachments {
		p := &e.Attachments[i]
		p.MessageID = e.MessageID
		scope := e.Route
		p.Scope = &scope
		p.ExpiresAt = expires
		a.resources.Put(wire.ContextKey(e.Route, e.MessageID+":"+p.Ref), messageResource{e.MessageID, p.Ref, p.Kind}, expires)
	}
	if len(e.Parts) == 0 {
		if e.Text != "" {
			e.Parts = append(e.Parts, gateway.ContentPart{Text: e.Text})
		}
		for _, p := range e.Attachments {
			e.Parts = append(e.Parts, gateway.ContentPart{Attachment: &p})
		}
	}
	index := 0
	for i := range e.Parts {
		if e.Parts[i].Attachment != nil {
			p := e.Attachments[index]
			e.Parts[i].Attachment = &p
			index++
		}
	}
}
