package main

import (
	"context"
	"errors"
	"testing"
	"github.com/Icatme/wechatbot-go/gateway"
)

func TestPolicyFailClosed(t *testing.T) {
	for _, s := range []string{"", `[]`, `[{"platform":"x"}]`, `[{"platform":"x","account":"a","user":"u","typo":true}]`} {
		if _, err := policy(s); err == nil { t.Fatal(s) }
	}
}
func TestPolicyExactTenantAndAccount(t *testing.T) {
	p, err := policy(`[{"platform":"x","account":"a","tenant":"t","user":"u"}]`)
	if err != nil { t.Fatal(err) }
	e := gateway.Event{SenderID: "u", Route: gateway.Route{Account: gateway.Account{Platform: "x", ID: "a"}, TenantID: "t", ChatID: "chat"}}
	if p(context.Background(), e) != nil { t.Fatal("valid principal rejected") }
	e.Route.TenantID = "other"
	if !errors.Is(p(context.Background(), e), gateway.ErrUnauthorized) { t.Fatal("cross tenant") }
	e.Route.TenantID = "t"
	e.Route.ID = "other"
	if !errors.Is(p(context.Background(), e), gateway.ErrUnauthorized) { t.Fatal("cross account") }
}
func TestPolicyRejectsTrailingData(t *testing.T) {
	if _, err := policy(`[{"platform":"x","account":"a","user":"u"}] {}`); err == nil { t.Fatal("trailing data") }
}
