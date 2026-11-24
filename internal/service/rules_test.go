package service_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/object"
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

// Required checks vx did not run are recorded on the change's evidence.
func TestLandRecordsRequiredChecksNotVerified(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	// No vx.toml: nothing is added.
	if c := e.land(repo, eff, "Plain", map[string]*string{"a.txt": s("a")}, nil); c.Evidence != nil {
		t.Fatalf("evidence without vx.toml: %+v", c.Evidence)
	}
	toml := "[checks]\nrequired = [\"go vet ./...\", \"go test ./...\"]\n"
	c := e.land(repo, e.effort(eff), "Rules", map[string]*string{"vx.toml": s(toml)}, nil)
	if c.Evidence == nil || !reflect.DeepEqual(c.Evidence.NotVerified, []string{"go vet ./...", "go test ./..."}) {
		t.Fatalf("no evidence at all: %+v", c.Evidence)
	}
	// Run by vx (spaces trimmed) counts; reported by the author does not.
	ev := &object.Evidence{Checks: []object.Check{
		{Name: " go vet ./... ", Result: "pass", RanByVX: true},
		{Name: "go test ./...", Result: "pass"},
	}, NotVerified: []string{"forged"}}
	c = e.land(repo, e.effort(eff), "Some", map[string]*string{"b.txt": s("b")}, ev)
	if !reflect.DeepEqual(c.Evidence.NotVerified, []string{"go test ./..."}) || len(c.Evidence.Checks) != 2 {
		t.Fatalf("partly verified: %+v", c.Evidence)
	}
	if len(ev.NotVerified) != 1 || ev.NotVerified[0] != "forged" {
		t.Fatal("the caller's evidence was modified")
	}
	all := &object.Evidence{Checks: []object.Check{
		{Name: "go vet ./...", Result: "pass", RanByVX: true},
		{Name: "go test ./...", Result: "fail", RanByVX: true},
	}}
	c = e.land(repo, e.effort(eff), "All", map[string]*string{"c.txt": s("c")}, all)
	if len(c.Evidence.NotVerified) != 0 {
		t.Fatalf("all run by vx: %+v", c.Evidence)
	}
}

// Reserve hands out the next number after everything in use: the target
// branch, every open effort, earlier reservations, with the same padding.
func TestReserve(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	e.moveMain(repo, "Main", map[string]*string{
		"vx.toml":                 s(rulesToml),
		"db/migrations/001_a.sql": s("a"),
		"db/migrations/015_b.sql": s("b"),
		"db/migrations/README.md": s("not a migration"),
	})
	if v, err := e.svc.RebaseEffort(e.ctx, e.alice, repo.Name, eff.Slug, ""); err != nil || v.Rebase.State != "done" {
		t.Fatalf("rebase: %+v %v", v, err)
	}
	eff = e.effort(eff)
	v, ex, err := e.svc.Reserve(e.ctx, e.agent, repo.Name, eff.Slug, "migration")
	if err != nil || v != "016" || ex != "db/migrations/016_<name>.sql" {
		t.Fatalf("first: %q %q %v", v, ex, err)
	}
	// Another open effort already added 016 and 017 (one in its head only).
	other, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: repo.Name, Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	e.land(repo, other, "Two migrations", map[string]*string{
		"db/migrations/016_c.sql": s("c"),
		"db/migrations/017_d.sql": s("d"),
	}, nil)
	v, ex, err = e.svc.Reserve(e.ctx, e.agent, repo.Name, eff.Slug, "migration")
	if err != nil || v != "018" || ex != "db/migrations/018_<name>.sql" {
		t.Fatalf("after the other effort: %q %q %v", v, ex, err)
	}
	// Earlier reservations count even when nothing uses them yet.
	v, _, err = e.svc.Reserve(e.ctx, e.agent, repo.Name, other.Slug, "migration")
	if err != nil || v != "019" {
		t.Fatalf("third: %q %v", v, err)
	}
	res, err := e.svc.ListReservations(e.ctx, e.alice, repo.Name, "migration")
	if err != nil || len(res) != 3 || res[0].Value != "019" || res[0].EffortSlug != other.Slug || res[0].ReservedBy != "agent:alice/claude" || !res[0].EffortOpen {
		t.Fatalf("reservations: %+v %v", res, err)
	}

	// Errors say what to do.
	if _, _, err := e.svc.Reserve(e.ctx, e.agent, repo.Name, eff.Slug, "migrations"); err == nil || !strings.Contains(err.Error(), `vx.toml has no [[unique]] rule "migrations"; its rules: migration`) {
		t.Fatalf("unknown rule: %v", err)
	}
	if _, _, err := e.svc.Reserve(e.ctx, e.agent, repo.Name, "nope", "migration"); err == nil {
		t.Fatal("unknown effort")
	}

	// GetRules: the effort head, or the default branch.
	r, err := e.svc.GetRules(e.ctx, e.alice, repo.Name, eff.Slug)
	if err != nil || len(r.Unique) != 1 || !strings.HasPrefix(r.Source, "vx.toml at effort "+eff.Slug+" ") {
		t.Fatalf("rules at the effort: %+v %v", r, err)
	}
	r, err = e.svc.GetRules(e.ctx, e.alice, repo.Name, "")
	if err != nil || len(r.Generated) != 1 || !strings.HasPrefix(r.Source, "vx.toml at main ") {
		t.Fatalf("rules at main: %+v %v", r, err)
	}
}

func TestReserveFormatsAndConcurrency(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	toml := "[[unique]]\nname = \"n\"\nglob = \"n/*\"\nkey = '^(\\d+)'\n\n[[unique]]\nname = \"word\"\nglob = \"w/*.txt\"\nkey = '^([a-z]+)'\n\n[[unique]]\nname = \"empty\"\nglob = \"e/*.sql\"\nkey = '^(\\d+)_'\n"
	e.land(repo, eff, "Rules", map[string]*string{
		"vx.toml": s(toml), "n/9.txt": s("x"), "n/10.txt": s("x"), "w/abc.txt": s("x"),
	}, nil)
	eff = e.effort(eff)
	if v, ex, err := e.svc.Reserve(e.ctx, e.agent, repo.Name, eff.Slug, "n"); err != nil || v != "11" || ex != "n/11<name>.txt" {
		t.Fatalf("unpadded: %q %q %v", v, ex, err)
	}
	if v, ex, err := e.svc.Reserve(e.ctx, e.agent, repo.Name, eff.Slug, "empty"); err != nil || v != "1" || ex != "e/1_<name>.sql" {
		t.Fatalf("no files yet: %q %q %v", v, ex, err)
	}
	if _, _, err := e.svc.Reserve(e.ctx, e.agent, repo.Name, eff.Slug, "word"); err == nil || !strings.Contains(err.Error(), "not numbers") {
		t.Fatalf("words: %v", err)
	}

	// Concurrent calls never get the same value.
	const n = 12
	got := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			v, _, err := e.svc.Reserve(e.ctx, e.agent, repo.Name, eff.Slug, "n")
			got <- v
			errs <- err
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		v := <-got
		if seen[v] {
			t.Fatalf("value %s handed out twice", v)
		}
		seen[v] = true
	}
	for i := 12; i < 12+n; i++ {
		if !seen[strconv.Itoa(i)] {
			t.Fatalf("values %v: want 12..%d", seen, 12+n-1)
		}
	}
}
