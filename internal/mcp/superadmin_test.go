package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// TestMCPSuperadminNotFound: a superadmin's agent gets "not found" from
// every repository tool, like anyone without a role; with support access
// it reads.
func TestMCPSuperadminNotFound(t *testing.T) {
	f := newFixture(t)
	_, eff, landed := f.seed()
	root, err := f.svc.Store.CreatePrincipal(f.ctx, store.Principal{Kind: store.KindHuman, Handle: "root", SiteAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := f.svc.Store.CreatePrincipal(f.ctx, store.Principal{Kind: store.KindAgent, Handle: "root/ops", ParentID: &root.ID})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := f.svc.Store.CreateToken(f.ctx, agent.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	f.tokens["root/ops"] = tok
	cs := f.connect("root/ops")

	if effs := call[effortsOutput](t, cs, "vx_efforts", nil); len(effs.Repos) != 0 {
		t.Fatalf("superadmin's agent lists %+v", effs.Repos)
	}
	at := map[string]any{"repo": "shop-api", "effort": eff.Slug}
	with := func(kv ...any) map[string]any {
		out := map[string]any{"repo": "shop-api", "effort": eff.Slug}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	tools := []struct {
		name string
		args map[string]any
	}{
		{"vx_efforts", map[string]any{"repo": "shop-api"}},
		{"vx_status", at},
		{"vx_log", with("files", true)},
		{"vx_show", map[string]any{"repo": "shop-api", "ref": landed.ChangeID}},
		{"vx_diff", map[string]any{"repo": "shop-api", "ref": landed.ChangeID, "path": "internal/http/limit.go"}},
		{"vx_board", at},
		{"vx_context", with("targets", []string{"internal/http/limit.go"})},
		{"vx_claims", with("targets", []string{"internal/http/limit.go"})},
		{"vx_tasks", at},
		{"vx_overlaps", at},
		{"vx_rules", at},
		{"vx_check_jobs", at},
		{"vx_check_request", with("check", "e2e")},
		{"vx_post_note", with("kind", "plan", "targets", []string{"a.go"}, "body", "I am the operator")},
		{"vx_claim", with("targets", []string{"a.go"})},
	}
	// What a repository that does not exist says, tool by tool: the
	// superadmin must get exactly that.
	missing := func(args map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range args {
			out[k] = v
		}
		out["repo"] = "no-such-repo"
		return out
	}
	alice := f.connect("alice/claude")
	for _, tool := range tools {
		msg := callErr(t, cs, tool.name, tool.args)
		want := strings.ReplaceAll(callErr(t, alice, tool.name, missing(tool.args)), "no-such-repo", "shop-api")
		if msg != want {
			t.Errorf("%s by a superadmin's agent = %q, want what a missing repository says: %q", tool.name, msg, want)
		}
	}

	if _, err := f.svc.GrantSupport(f.ctx, f.actors["alice"], service.SupportInput{Repo: "shop-api", Person: "root", For: time.Hour, Reason: "ticket 1"}); err != nil {
		t.Fatal(err)
	}
	if lg := call[logOutput](t, cs, "vx_log", at); len(lg.Changes) != 1 {
		t.Fatalf("log with support access = %+v", lg)
	}
	if msg := callErr(t, cs, "vx_post_note", with("kind", "plan", "targets", []string{"a.go"}, "body", "I am the operator")); !strings.Contains(msg, "needs write") {
		t.Errorf("post with read support access = %q, want a permission error", msg)
	}
}
