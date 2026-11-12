package dingtalk

import (
	"strconv"
	"time"

	"github.com/Icatme/wechatbot-go/channels/internal/wire"
	"github.com/Icatme/wechatbot-go/gateway"
)

func (a *Adapter) bindResources(e *gateway.Event) {
	// Local lookup lifetime, not a promise about the platform's download-code TTL.
	expires := time.Now().Add(5 * time.Minute)
	for i := range e.Attachments {
		p := &e.Attachments[i]
		code := p.Ref
		p.Ref = e.MessageID + ":" + strconv.Itoa(i)
		p.MessageID = e.MessageID
		scope := e.Route
		p.Scope = &scope
		p.ExpiresAt = expires
		a.resources.Put(wire.ContextKey(e.Route, p.Ref), code, expires)
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
