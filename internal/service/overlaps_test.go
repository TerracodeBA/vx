package service_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

const authBase = "package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {\n\tclear()\n}\n"

// overlapEnv is a repository whose main has auth.go, with two open efforts
// started on it.
type overlapEnv struct {
	*env
	repo store.Repo
	a, b store.Effort
}

func newOverlapEnv(t *testing.T, mainFiles map[string]*string) *overlapEnv {
	t.Helper()
	e := &overlapEnv{env: newEnv(t)}
	var err error
	if e.repo, err = e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "shop-api"}); err != nil {
		t.Fatal(err)
	}
	files := map[string]*string{"auth.go": s(authBase)}
	for p, c := range mainFiles {
		files[p] = c
	}
	e.moveMain(e.repo, "Seed", files)
	if e.a, err = e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "shop-api", Name: "Rate limiting"}); err != nil {
		t.Fatal(err)
	}
	if e.b, err = e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "shop-api", Name: "Audit log"}); err != nil {
		t.Fatal(err)
	}
	return e
}

// landIn lands files into eff and waits for the background forecast.
func (e *overlapEnv) landIn(eff store.Effort, intent string, files map[string]*string) service.LandResult {
	e.t.Helper()
	eff = e.effort(eff)
	ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, e.repo.Name, eff.Slug, intent)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: e.snapshot(e.repo, ws.Workspace.Base, files)})
	if err != nil || !res.Landed {
		e.t.Fatalf("land %q: %+v %v", intent, res, err)
	}
	e.svc.WaitForecasts()
	return res
}

func (e *overlapEnv) overlaps(eff store.Effort) []service.Overlap {
	e.t.Helper()
	os, err := e.svc.ListOverlaps(e.ctx, e.alice, e.repo.Name, eff.Slug)
	if err != nil {
		e.t.Fatal(err)
	}
	return os
}

// one returns the single forecast of eff, which must be with other.
func (e *overlapEnv) one(eff, other store.Effort) service.Overlap {
	e.t.Helper()
	os := e.overlaps(eff)
	if len(os) != 1 || os[0].Other.ID != other.ID || os[0].Effort.ID != eff.ID {
		e.t.Fatalf("overlaps of %s = %+v", eff.Slug, os)
	}
	return os[0]
}

// overlapNotes returns the overlap notes on eff's board that are still
// current (not superseded), newest first.
func (e *overlapEnv) overlapNotes(eff store.Effort) []service.Note {
	e.t.Helper()
	notes, err := e.svc.ListNotes(e.ctx, e.alice, service.ListNotesInput{Repo: e.repo.Name, Effort: eff.Slug})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []service.Note
	for _, n := range notes {
		if n.Kind == service.NoteOverlap && n.SupersededBy == nil {
			out = append(out, n)
		}
	}
	return out
}

func (e *overlapEnv) wantNote(eff store.Effort, contains ...string) service.Note {
	e.t.Helper()
	ns := e.overlapNotes(eff)
	if len(ns) != 1 {
		e.t.Fatalf("current overlap notes on %s = %d, want 1: %+v", eff.Slug, len(ns), ns)
	}
	n := ns[0]
	if !n.System || n.Author.Handle != service.SystemPrincipalHandle {
		e.t.Fatalf("overlap note must be a system note by vx: %+v", n)
	}
	for _, c := range contains {
		if !strings.Contains(n.Body, c) {
			e.t.Fatalf("note on %s = %q, want it to contain %q", eff.Slug, n.Body, c)
		}
	}
	return n
}

func TestOverlapForecastConflictNotesSupersedeAndResolve(t *testing.T) {
	e := newOverlapEnv(t, nil)
	e.landIn(e.a, "Limit Login", map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\tlimit()\n}\n\nfunc Logout() {\n\tclear()\n}\n")})
	if os := e.overlaps(e.a); len(os) != 1 || os[0].Status != service.OverlapClean {
		t.Fatalf("one effort alone: %+v", os)
	}
	if ns := e.overlapNotes(e.a); len(ns) != 0 {
		t.Fatalf("no note without an overlap: %+v", ns)
	}

	// B changes the same lines: shipping both conflicts.
	res := e.landIn(e.b, "Audit Login", map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\taudit()\n}\n\nfunc Logout() {\n\tclear()\n}\n")})
	_ = res
	o := e.one(e.b, e.a)
	if o.Status != service.OverlapConflict || strings.Join(o.Conflicts, ",") != "auth.go" || strings.Join(o.SharedFiles, ",") != "auth.go" {
		t.Fatalf("forecast = %+v", o)
	}
	if o.OtherOwner.Handle != "alice" || o.RepoName != "shop-api" {
		t.Fatalf("other side = %+v", o)
	}
	if back := e.one(e.a, e.b); back.Status != service.OverlapConflict {
		t.Fatalf("the other side sees %+v", back)
	}
	first := e.wantNote(e.a, `Effort "Audit log"`, "also changes auth.go", "Shipping both conflicts in auth.go")
	e.wantNote(e.b, `Effort "Rate limiting"`, "user:alice", "Shipping both conflicts in auth.go")
	if strings.Join(first.Targets, ",") != "auth.go" {
		t.Fatalf("targets = %v", first.Targets)
	}

	// The land returns the stored forecasts of its effort.
	res = e.landIn(e.b, "Audit Logout too", map[string]*string{"audit.go": s("package auth\n")})
	if len(res.Overlaps) != 1 || res.Overlaps[0].Other.ID != e.a.ID || res.Overlaps[0].Status != service.OverlapConflict {
		t.Fatalf("land overlaps = %+v", res.Overlaps)
	}
	// Nothing changed in the outcome: no new note.
	if n := e.wantNote(e.a); n.ID != first.ID {
		t.Fatal("an unchanged forecast must not post again")
	}

	// B moves its edit to Logout: same file, different lines, merges.
	e.landIn(e.b, "Audit Logout instead", map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {\n\taudit()\n}\n")})
	o = e.one(e.a, e.b)
	if o.Status != service.OverlapShared || len(o.Conflicts) != 0 || strings.Join(o.SharedFiles, ",") != "auth.go" {
		t.Fatalf("forecast after the fix = %+v", o)
	}
	second := e.wantNote(e.a, "also changes auth.go", "Shipping both merges cleanly.")
	if second.ID == first.ID || second.Supersedes == nil || *second.Supersedes != first.ID {
		t.Fatalf("the new note must supersede the old one: %+v (old %s)", second, first.ID)
	}

	// B reverts auth.go entirely: nothing shared any more.
	e.landIn(e.b, "Leave auth alone", map[string]*string{"auth.go": s(authBase)})
	if o := e.one(e.a, e.b); o.Status != service.OverlapClean || len(o.SharedFiles) != 0 {
		t.Fatalf("forecast after revert = %+v", o)
	}
	resolved := e.wantNote(e.a, "Resolved", `"Audit log"`)
	if resolved.Supersedes == nil || *resolved.Supersedes != second.ID {
		t.Fatalf("resolved note must supersede the last one: %+v", resolved)
	}
	e.wantNote(e.b, "Resolved", `"Rate limiting"`)

	// Clean stays clean: no more notes.
	e.landIn(e.b, "More audit", map[string]*string{"audit.go": s("package auth\n\nfunc Audit() {}\n")})
	if n := e.wantNote(e.a); n.ID != resolved.ID {
		t.Fatal("clean after clean must stay quiet")
	}
}

func TestOverlapForecastSameFileDifferentLines(t *testing.T) {
	e := newOverlapEnv(t, nil)
	e.landIn(e.a, "Limit Login", map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\tlimit()\n}\n\nfunc Logout() {\n\tclear()\n}\n")})
	e.landIn(e.b, "Audit Logout", map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {\n\taudit()\n}\n")})
	o := e.one(e.a, e.b)
	if o.Status != service.OverlapShared || len(o.Conflicts) != 0 || len(o.Warnings) != 0 {
		t.Fatalf("forecast = %+v", o)
	}
	e.wantNote(e.a, "also changes auth.go", "Shipping both merges cleanly.")
	e.wantNote(e.b, "also changes auth.go", "Shipping both merges cleanly.")
}

func TestOverlapForecastDuplicateGoFunc(t *testing.T) {
	e := newOverlapEnv(t, nil)
	e.landIn(e.a, "Add helper", map[string]*string{"util_a.go": s("package auth\n\nfunc helper() {}\n")})
	e.landIn(e.b, "Add helper too", map[string]*string{"util_b.go": s("package auth\n\nfunc helper() {}\n")})
	o := e.one(e.a, e.b)
	if o.Status != service.OverlapShared || len(o.SharedFiles) != 0 || len(o.Warnings) != 1 {
		t.Fatalf("forecast = %+v", o)
	}
	if w := o.Warnings[0]; w.Kind != "duplicate" || w.Name != "helper" {
		t.Fatalf("warning = %+v", w)
	}
	n := e.wantNote(e.b, "changes util_a.go, next to this effort's util_b.go", "Shipping both merges cleanly, but: both declare func helper in the root package (util_a.go, util_b.go).")
	if strings.Join(n.Targets, ",") != "util_a.go,util_b.go" {
		t.Fatalf("targets = %v", n.Targets)
	}
	// A duplicate inside one effort alone is that effort's own warning.
	if os := e.overlaps(e.a); len(os[0].Warnings) != 1 {
		t.Fatalf("one warning, not more: %+v", os)
	}
}

func TestOverlapForecastMigrationRule(t *testing.T) {
	toml := "[[unique]]\nname = \"migration\"\nglob = \"migrations/*.sql\"\nkey = '^(\\d+)_'\n"
	e := newOverlapEnv(t, map[string]*string{"vx.toml": s(toml), "migrations/015_init.sql": s("create table t ();\n")})
	e.landIn(e.a, "Add limits table", map[string]*string{"migrations/016_limits.sql": s("create table limits ();\n")})
	e.landIn(e.b, "Add audit table", map[string]*string{"migrations/016_audit.sql": s("create table audit ();\n")})
	o := e.one(e.a, e.b)
	if o.Status != service.OverlapShared || len(o.Warnings) != 1 || o.Warnings[0].Kind != service.WarningRule {
		t.Fatalf("forecast = %+v", o)
	}
	e.wantNote(e.a, "migrations/016_audit.sql", "migration 016 is used by")
	// Renumbering resolves it.
	e.landIn(e.b, "Renumber", map[string]*string{"migrations/016_audit.sql": nil, "migrations/017_audit.sql": s("create table audit ();\n")})
	if o := e.one(e.a, e.b); o.Status != service.OverlapClean || len(o.Warnings) != 0 {
		t.Fatalf("after renumbering = %+v", o)
	}
	e.wantNote(e.a, "Resolved")
}

func TestOverlapForecastRunsAfterLand(t *testing.T) {
	e := newOverlapEnv(t, nil)
	release := make(chan struct{})
	e.svc.SetForecastHook(func() { <-release })
	land := func(eff store.Effort, files map[string]*string) service.LandResult {
		ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, e.repo.Name, e.effort(eff).Slug, "edit")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan service.LandResult, 1)
		go func() {
			res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: e.snapshot(e.repo, ws.Workspace.Base, files)})
			if err != nil || !res.Landed {
				t.Errorf("land: %+v %v", res, err)
			}
			done <- res
		}()
		select {
		case res := <-done:
			return res
		case <-time.After(10 * time.Second):
			t.Fatal("the land waited for the forecast")
		}
		return service.LandResult{}
	}
	land(e.a, map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\tlimit()\n}\n\nfunc Logout() {\n\tclear()\n}\n")})
	res := land(e.b, map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\taudit()\n}\n\nfunc Logout() {\n\tclear()\n}\n")})
	if len(res.Overlaps) != 0 {
		t.Fatalf("the forecast has not run yet, the land must not wait for it: %+v", res.Overlaps)
	}
	close(release)
	e.svc.WaitForecasts()
	e.svc.SetForecastHook(nil)
	if o := e.one(e.a, e.b); o.Status != service.OverlapConflict {
		t.Fatalf("forecast after the background run = %+v", o)
	}
	e.wantNote(e.a, "conflicts in auth.go")
}

func TestOverlapForecastStaleOnDemandAndOtherBranches(t *testing.T) {
	e := newOverlapEnv(t, nil)
	e.landIn(e.a, "Limit Login", map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\tlimit()\n}\n\nfunc Logout() {\n\tclear()\n}\n")})
	e.landIn(e.b, "Audit Logout", map[string]*string{"auth.go": s("package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {\n\taudit()\n}\n")})
	before := e.one(e.a, e.b)
	// main moves: the stored forecast is stale and ListOverlaps recomputes.
	e.moveMain(e.repo, "Upstream", map[string]*string{"readme.md": s("hi\n")})
	time.Sleep(2 * time.Millisecond)
	after := e.one(e.a, e.b)
	if !after.CheckedAt.After(before.CheckedAt) || after.Status != service.OverlapShared {
		t.Fatalf("stale forecast was not recomputed: %v then %v", before.CheckedAt, after.CheckedAt)
	}
	// The repository listing has the pair once.
	all, err := e.svc.ListOverlaps(e.ctx, e.alice, e.repo.Name, "")
	if err != nil || len(all) != 1 {
		t.Fatalf("repo overlaps = %+v %v", all, err)
	}
	// An abandoned effort drops out.
	if err := e.svc.Store.SetEffortState(e.ctx, e.b.ID, store.EffortAbandoned); err != nil {
		t.Fatal(err)
	}
	if os := e.overlaps(e.a); len(os) != 0 {
		t.Fatalf("overlaps with a closed effort: %+v", os)
	}
}

func TestOverlapNotesNeedReadAccess(t *testing.T) {
	e := newAuthzEnv(t)
	other, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Pricing"})
	if err != nil {
		t.Fatal(err)
	}
	e.land(e.repo, e.eff, "Cart v2", map[string]*string{"cart.go": s("package shop\n\nvar v = 2\n")}, nil)
	e.land(e.repo, other, "Cart v3", map[string]*string{"cart.go": s("package shop\n\nvar v = 3\n")}, nil)
	e.svc.WaitForecasts()
	os, err := e.svc.ListOverlaps(e.ctx, e.alice, "acme/shop", other.Slug)
	if err != nil || len(os) != 1 || os[0].Status != service.OverlapConflict {
		t.Fatalf("alice's forecast = %+v %v", os, err)
	}
	for _, a := range []service.Actor{e.bob, e.bobAgent} {
		if _, err := e.svc.ListOverlaps(e.ctx, a, "acme/shop", other.Slug); !isCode(err, service.CodeNotFound) {
			t.Fatalf("ListOverlaps for %s: %v", a.Ref(), err)
		}
		if _, err := e.svc.ListNotes(e.ctx, a, service.ListNotesInput{Repo: "acme/shop", Effort: other.Slug}); !isCode(err, service.CodeNotFound) {
			t.Fatalf("ListNotes for %s: %v", a.Ref(), err)
		}
		in, err := e.svc.GetInbox(e.ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		for _, list := range [][]service.Note{in.Questions, in.Answers, in.Escalated} {
			for _, n := range list {
				if n.Kind == service.NoteOverlap {
					t.Fatalf("%s sees an overlap note: %+v", a.Ref(), n)
				}
			}
		}
	}
}

const landProto = "syntax = \"proto3\";\n\nmessage LandRequest {\n  string workspace_id = 1;\n}\n\nmessage LandResponse {\n  bool landed = 1;\n  string hash = 2;\n  repeated string files = 3;\n  string why = 4;\n  string when = 5;\n  string who = 6;\n}\n"

// Two efforts both add field 12 to message LandResponse, at different
// places: the file merges, and the combined proto breaks the proto-field
// rule of vx's own vx.toml.
func TestOverlapForecastProtoFieldRule(t *testing.T) {
	toml, err := os.ReadFile(filepath.Join("..", "..", "vx.toml"))
	if err != nil {
		t.Fatal(err)
	}
	e := newOverlapEnv(t, map[string]*string{"vx.toml": s(string(toml)), "proto/vx/v1/vx.proto": s(landProto)})
	e.landIn(e.a, "Add forecast", map[string]*string{"proto/vx/v1/vx.proto": s(strings.Replace(landProto, "  bool landed = 1;\n", "  bool landed = 1;\n  string forecast = 12;\n", 1))})
	e.landIn(e.b, "Add shipped", map[string]*string{"proto/vx/v1/vx.proto": s(strings.Replace(landProto, "  string who = 6;\n", "  string who = 6;\n  string shipped = 12;\n", 1))})
	o := e.one(e.a, e.b)
	if o.Status != service.OverlapShared || len(o.Conflicts) != 0 || len(o.Warnings) != 1 {
		t.Fatalf("forecast = %+v", o)
	}
	if w := o.Warnings[0]; w.Kind != service.WarningRule || w.Name != "proto-field" || w.Decl != "LandResponse 12" {
		t.Fatalf("warning = %+v", w)
	}
	e.wantNote(e.a, "also changes proto/vx/v1/vx.proto", "proto-field 12 in LandResponse is used twice: proto/vx/v1/vx.proto lines 9 and 15")
	// Field 12 in another message is no clash.
	e.landIn(e.b, "Move shipped", map[string]*string{"proto/vx/v1/vx.proto": s(strings.Replace(landProto, "  string workspace_id = 1;\n", "  string workspace_id = 1;\n  string shipped = 12;\n", 1))})
	if o := e.one(e.a, e.b); len(o.Warnings) != 0 {
		t.Fatalf("after moving the field: %+v", o)
	}
}

// Both efforts change a generated file: the forecast says to regenerate it
// when shipping, not that shipping both merges cleanly.
func TestOverlapForecastGeneratedFiles(t *testing.T) {
	toml := "[[generated]]\nglob = \"gen/**\"\ncommand = \"buf generate\"\n"
	gen := "// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage gen\n\nconst A = 1\n\nconst B = 2\n"
	e := newOverlapEnv(t, map[string]*string{"vx.toml": s(toml), "gen/x.pb.go": s(gen)})
	e.landIn(e.a, "Regenerate A", map[string]*string{"gen/x.pb.go": s(strings.Replace(gen, "A = 1", "A = 10", 1))})
	e.landIn(e.b, "Regenerate B", map[string]*string{"gen/x.pb.go": s(strings.Replace(gen, "B = 2", "B = 20", 1))})
	o := e.one(e.a, e.b)
	if o.Status != service.OverlapShared || len(o.Conflicts) != 0 || len(o.Warnings) != 1 || o.Warnings[0].Kind != "generated" || o.Warnings[0].Path != "gen/x.pb.go" {
		t.Fatalf("forecast = %+v", o)
	}
	n := e.wantNote(e.a, "also changes gen/x.pb.go", "Shipping both: regenerate when shipping (gen/x.pb.go) with `buf generate`.")
	if strings.Contains(n.Body, "merges cleanly") {
		t.Fatalf("a generated file does not merge cleanly: %q", n.Body)
	}
	// With another shared file that merges, the note says so without
	// calling the generated one clean.
	e2 := newOverlapEnv(t, map[string]*string{"vx.toml": s(toml), "gen/x.pb.go": s(gen)})
	e2.landIn(e2.a, "Limit Login", map[string]*string{"gen/x.pb.go": s(strings.Replace(gen, "A = 1", "A = 10", 1)),
		"auth.go": s("package auth\n\nfunc Login() {\n\tlimit()\n}\n\nfunc Logout() {\n\tclear()\n}\n")})
	e2.landIn(e2.b, "Audit Logout", map[string]*string{"gen/x.pb.go": s(strings.Replace(gen, "B = 2", "B = 20", 1)),
		"auth.go": s("package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {\n\taudit()\n}\n")})
	n = e2.wantNote(e2.b, "also changes auth.go, gen/x.pb.go", "Shipping both: regenerate when shipping (gen/x.pb.go) with `buf generate`; the other shared files merge.")
	if strings.Contains(n.Body, "merges cleanly") {
		t.Fatalf("a generated file does not merge cleanly: %q", n.Body)
	}
}
