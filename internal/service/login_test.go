package service_test

import (
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
)

func TestLoginCodes(t *testing.T) {
	e := newEnv(t)
	code, expires, err := e.svc.CreateLoginCode(e.ctx, e.agent)
	if err != nil || code == "" || expires.Sub(time.Now()) > service.LoginCodeTTL+time.Second {
		t.Fatalf("create = %q %v %v", code, expires, err)
	}
	p, err := e.svc.PeekLoginCode(e.ctx, code)
	if err != nil || p.ID != e.agent.ID {
		t.Fatalf("peek = %v %v", p.Handle, err)
	}
	p, token, err := e.svc.RedeemLoginCode(e.ctx, code)
	if err != nil || p.ID != e.agent.ID || token == "" {
		t.Fatalf("redeem = %v %q %v", p.Handle, token, err)
	}
	if got, err := e.svc.Store.Authenticate(e.ctx, token); err != nil || got.ID != e.agent.ID {
		t.Fatalf("issued token does not authenticate: %v", err)
	}
	if _, _, err := e.svc.RedeemLoginCode(e.ctx, code); err == nil {
		t.Fatal("a code must work only once")
	}
	if _, _, err := e.svc.RedeemLoginCode(e.ctx, "not-a-code"); err == nil {
		t.Fatal("unknown codes must fail")
	}
	// Expired codes fail.
	old, _, _ := e.svc.CreateLoginCode(e.ctx, e.alice)
	e.svc.Now = func() time.Time { return time.Now().Add(service.LoginCodeTTL + time.Second) }
	if _, err := e.svc.PeekLoginCode(e.ctx, old); err == nil {
		t.Fatal("expired code should not peek")
	}
	if _, _, err := e.svc.RedeemLoginCode(e.ctx, old); err == nil {
		t.Fatal("expired code should not redeem")
	}
}
