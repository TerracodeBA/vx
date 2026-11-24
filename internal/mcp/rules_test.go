package mcp

import (
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

func TestMCPRulesAndReserve(t *testing.T) {
	f := newFixture(t)
	repo, eff, _ := f.seed()
	agent := f.actors["alice/claude"]
	ws, err := f.svc.OpenWorkspace(f.ctx, agent, repo.Name, eff.Slug, "Rules")
	if err != nil {
		t.Fatal(err)
	}
	tree := f.snapshot(repo, ws.Workspace.Base, map[string]string{
		"vx.toml":          "[[unique]]\nname = \"migration\"\nglob = \"db/*.sql\"\nkey = '^(\\d+)_'\n\n[checks]\nrequired = [\"go test ./...\"]\n",
		"db/007_users.sql": "x",
		"db/notes.md":      "x",
	})
	if res, err := f.svc.Land(f.ctx, agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree}); err != nil || !res.Landed {
		t.Fatalf("land: %+v %v", res, err)
	}
	cs := f.connect("alice/claude")
	r := call[Rules](t, cs, "vx_rules", map[string]any{"repo": "shop-api", "effort": eff.Slug})
	if len(r.Unique) != 1 || r.Unique[0].Name != "migration" || len(r.RequiredChecks) != 1 || r.Source == "" {
		t.Fatalf("rules = %+v", r)
	}
	out := call[reserveOutput](t, cs, "vx_reserve", map[string]any{"repo": "shop-api", "effort": eff.Slug, "rule": "migration"})
	if out.Value != "008" || out.Example != "db/008_<name>.sql" {
		t.Fatalf("reserve = %+v", out)
	}
	r = call[Rules](t, cs, "vx_rules", map[string]any{"repo": "shop-api"})
	if len(r.Reservations) != 1 || r.Reservations[0].Value != "008" || r.Reservations[0].Effort != eff.Slug || r.Reservations[0].ReservedBy != "agent:alice/claude" {
		t.Fatalf("reservations = %+v", r.Reservations)
	}
}
