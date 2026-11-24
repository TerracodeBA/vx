package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

const rulesToml = `[[unique]]
name = "migration"
glob = "db/migrations/*.sql"
key  = '^(\d+)_'

[[generated]]
glob    = "gen/**"
command = "buf generate"
`

func warningLines(ws []string) string { return strings.Join(ws, "\n") }

// Two workspaces of one effort each regenerate gen/ and each add migration
// 002. The second land merges: gen/ is not text-merged (the workspace's
// version wins), and both problems are warnings stored with the change.
func TestLandGeneratedFilesAndUniqueRules(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	e.land(repo, eff, "Rules", map[string]*string{
		"vx.toml":                 s(rulesToml),
		"gen/api.go":              s("package gen\n\nvar A = 1\n\nvar B = 1\n"),
		"db/migrations/001_a.sql": s("a"),
	}, nil)
	eff = e.effort(eff)

	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "One")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Two")
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{
		"gen/api.go":                s("package gen\n\nvar A = 2\n\nvar B = 1\n"),
		"db/migrations/002_one.sql": s("one"),
	})
	r1, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1})
	if err != nil || !r1.Landed || len(r1.Warnings) != 0 {
		t.Fatalf("first land: %+v %v", r1, err)
	}
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{
		"gen/api.go":                s("package gen\n\nvar A = 1\n\nvar B = 2\n"), // would text-merge cleanly
		"db/migrations/002_two.sql": s("two"),
	})
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil || !r2.Landed || !r2.Rebased {
		t.Fatalf("second land: %+v %v", r2, err)
	}
	files := e.read(repo, r2.EffortHead)
	if files["gen/api.go"] != "package gen\n\nvar A = 1\n\nvar B = 2\n" {
		t.Fatalf("gen/api.go = %q, want the workspace's version", files["gen/api.go"])
	}
	var got []string
	for _, w := range r2.Warnings {
		got = append(got, string(w.Kind)+": "+w.Detail)
	}
	want := []string{
		"generated: generated file gen/api.go: regenerate with `buf generate`",
		"rule: migration 002 is used by 002_one.sql and 002_two.sql (rule migration in vx.toml)",
	}
	if warningLines(got) != warningLines(want) {
		t.Fatalf("warnings:\n%s", warningLines(got))
	}
	row, err := e.svc.Store.GetChangeset(e.ctx, repo.ID, r2.EffortHead)
	if err != nil || len(row.Warnings) != 2 || row.Warnings[1].Name != "migration" || row.Warnings[1].Decl != "002" {
		t.Fatalf("stored warnings: %+v %v", row.Warnings, err)
	}

	// The next unrelated land does not repeat the clash.
	c := e.land(repo, e.effort(eff), "Docs", map[string]*string{"README.md": s("x\n")}, nil)
	if len(c.Warnings) != 0 {
		t.Fatalf("unrelated land warns: %+v", c.Warnings)
	}
}

// A land that breaks vx.toml says so, once; the rules then do nothing.
func TestLandWarnsAboutBrokenRules(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	c := e.land(repo, eff, "Bad rules", map[string]*string{"vx.toml": s("[[unique]]\nname = \"m\"\nglob = \"db/*.sql\"\nkey = '^(\\d+_'\n")}, nil)
	if len(c.Warnings) != 1 || c.Warnings[0].Kind != "rule" ||
		!strings.Contains(c.Warnings[0].Detail, "vx.toml does not parse, so its rules are not applied: vx.toml line 4:") {
		t.Fatalf("warnings: %+v", c.Warnings)
	}
	c = e.land(repo, e.effort(eff), "Docs", map[string]*string{"README.md": s("x\n")}, nil)
	if len(c.Warnings) != 0 {
		t.Fatalf("repeated: %+v", c.Warnings)
	}
	tree := e.snapshot(repo, e.effort(eff).Head, nil)
	if _, err := e.svc.RulesAt(e.ctx, tree); err == nil {
		t.Fatal("RulesAt accepted a broken vx.toml")
	}
}
