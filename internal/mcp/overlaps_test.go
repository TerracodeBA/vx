package mcp

import (
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

func TestMCPOverlaps(t *testing.T) {
	f := newFixture(t)
	repo, eff, _ := f.seed()
	alice, agent := f.actors["alice"], f.actors["alice/claude"]
	auth, err := f.svc.CreateEffort(f.ctx, alice, service.CreateEffortInput{Repo: "shop-api", Name: "Auth"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := f.svc.OpenWorkspace(f.ctx, agent, "shop-api", auth.Slug, "Limit differently")
	if err != nil {
		t.Fatal(err)
	}
	tree := f.snapshot(repo, ws.Workspace.Base, map[string]string{"internal/http/limit.go": "package http\n\nfunc Limit() int { return 1 }\n"})
	if res, err := f.svc.Land(f.ctx, agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree}); err != nil || !res.Landed {
		t.Fatalf("land: %+v %v", res, err)
	}
	f.svc.WaitForecasts()

	bob := f.connect("bob/claude")
	out := call[overlapsOutput](t, bob, "vx_overlaps", map[string]any{"repo": "shop-api", "effort": auth.Slug})
	if len(out.Overlaps) != 1 {
		t.Fatalf("overlaps = %+v", out.Overlaps)
	}
	o := out.Overlaps[0]
	if o.Effort != auth.Slug || o.OtherEffort != eff.Slug || o.OtherOwner != "user:alice" || o.Status != "conflict" ||
		len(o.SharedFiles) != 1 || o.SharedFiles[0] != "internal/http/limit.go" {
		t.Errorf("overlap = %+v", o)
	}
	// Both efforts added the file with different contents: shipping both conflicts.
	cx := call[contextOutput](t, bob, "vx_context", map[string]any{"repo": "shop-api", "effort": auth.Slug, "targets": []string{"internal/http/limit.go"}})
	if len(cx.Overlaps) != 1 || cx.Overlaps[0].OtherEffort != eff.Slug {
		t.Errorf("vx_context overlaps = %+v", cx.Overlaps)
	}
	// Every pair of the repository, from the first effort's side.
	all := call[overlapsOutput](t, bob, "vx_overlaps", map[string]any{"repo": "shop-api"})
	if len(all.Overlaps) != 1 {
		t.Errorf("repo overlaps = %+v", all.Overlaps)
	}
}
