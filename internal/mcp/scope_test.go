package mcp

import (
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

// TestMCPWhoamiScope: vx_whoami tells an agent which paths it may change
// and what is left of its budgets; people get no scope.
func TestMCPWhoamiScope(t *testing.T) {
	f := newFixture(t)
	f.seed() // one land by alice/claude today
	cs := f.connect("alice/claude")
	if who := call[Principal](t, cs, "vx_whoami", nil); who.Scope == nil || !who.Scope.Unlimited || who.Scope.LandsLeft != -1 {
		t.Fatalf("unscoped whoami = %+v", who.Scope)
	}
	n := 3
	if _, err := f.svc.SetAgentScope(f.ctx, f.actors["alice"], "claude", service.ScopeInput{
		SetAllow: true, Allow: []string{"internal/api/**"}, SetDeny: true, Deny: []string{"internal/api/gen/**"}, MaxLandsPerDay: &n,
	}); err != nil {
		t.Fatal(err)
	}
	who := call[Principal](t, cs, "vx_whoami", nil)
	sc := who.Scope
	if sc == nil || sc.Unlimited || len(sc.Allow) != 1 || sc.Allow[0] != "internal/api/**" || len(sc.Deny) != 1 ||
		sc.MaxLandsPerDay != 3 || sc.LandsToday != 1 || sc.LandsLeft != 2 || sc.ResetsAt == "" || sc.WorkspacesLeft != -1 {
		t.Fatalf("scoped whoami = %+v", sc)
	}
	if who := call[Principal](t, f.connect("alice"), "vx_whoami", nil); who.Scope != nil {
		t.Fatalf("a person has a scope: %+v", who.Scope)
	}
}
