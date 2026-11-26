package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// moveMain commits files on top of the repo's main branch, as if another
// effort shipped and its pull request merged.
func (e *env) moveMain(repo store.Repo, intent string, files map[string]*string) store.ChangesetRow {
	e.t.Helper()
	b, err := e.svc.Store.GetBranch(e.ctx, repo.ID, "main")
	if err != nil {
		e.t.Fatal(err)
	}
	cs := &object.Changeset{
		Version: object.CurrentVersion, ChangeID: object.NewChangeID(), Parents: []hash.Hash{b.Head},
		Tree: e.snapshot(repo, b.Head, files), Origin: object.OriginUpstream,
		Author: object.Author{Name: "Bob", Email: "bob@example.com"}, Intent: intent, CreatedAt: e.svc.Clock(),
	}
	h, err := e.svc.Store.InsertChangeset(e.ctx, repo.ID, nil, cs, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.svc.Store.UpsertBranch(e.ctx, store.Branch{RepoID: repo.ID, Name: "main", Head: h}); err != nil {
		e.t.Fatal(err)
	}
	row, err := e.svc.Store.GetChangeset(e.ctx, repo.ID, h)
	if err != nil {
		e.t.Fatal(err)
	}
	return row
}

// land opens a workspace, writes files on its base and lands them.
func (e *env) land(repo store.Repo, eff store.Effort, intent string, files map[string]*string, ev *object.Evidence) store.ChangesetRow {
	e.t.Helper()
	ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, intent)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{
		WorkspaceID: ws.Workspace.ID, Tree: e.snapshot(repo, ws.Workspace.Base, files), Evidence: ev, Model: "claude-opus-5-5",
	})
	if err != nil || !res.Landed {
		e.t.Fatalf("land %q: %+v %v", intent, res, err)
	}
	return res.Changeset
}

func (e *env) effort(eff store.Effort) store.Effort {
	e.t.Helper()
	out, err := e.svc.Store.EffortByID(e.ctx, eff.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// chain returns the effort's changes oldest first.
func (e *env) chain(repo store.Repo, eff store.Effort) []service.LogEntry {
	e.t.Helper()
	log, err := e.svc.Log(e.ctx, e.alice, repo.Name, eff.Slug, 0, false)
	if err != nil {
		e.t.Fatal(err)
	}
	for i, j := 0, len(log)-1; i < j; i, j = i+1, j-1 {
		log[i], log[j] = log[j], log[i]
	}
	return log
}

// treeWith returns the tree of changeset base with files written on top.
func (e *env) treeWith(repo store.Repo, base hash.Hash, files map[string]*string) hash.Hash {
	e.t.Helper()
	return e.snapshot(repo, base, files)
}

var passing = &object.Evidence{Checks: []object.Check{{Name: "go test ./...", Result: "pass", RanByVX: true}}}

func TestRebaseReplaysCleanly(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	c2 := e.land(repo, eff, "Add limiter", map[string]*string{"limit.go": s("package limit\n")}, passing)
	c3 := e.land(repo, eff, "Use limiter in Login", map[string]*string{"auth.go": s("func Login() {\n\tlimit()\n}\n\nfunc Logout() {\n}\n")}, nil)
	before := e.chain(repo, eff)
	main := e.moveMain(repo, "Docs", map[string]*string{"README.md": s("# shop\n")})

	v, err := e.svc.RebaseEffort(e.ctx, e.alice, repo.Name, eff.Slug, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Rebase.State != store.RebaseDone || v.UpToDate {
		t.Fatalf("rebase = %+v", v.Rebase)
	}
	after := e.effort(eff)
	if after.Base != main.Hash || after.Head == eff.Head || after.Head != v.Rebase.NewHead {
		t.Fatalf("effort base %s head %s, want base %s and the rebased head %s", after.Base.Short(), after.Head.Short(), main.Hash.Short(), v.Rebase.NewHead.Short())
	}
	chain := e.chain(repo, after)
	if len(chain) != len(before) {
		t.Fatalf("rebased chain has %d changes, want %d", len(chain), len(before))
	}
	for i, c := range chain {
		old := before[i]
		if c.ChangeID != old.ChangeID || c.Intent != old.Intent || c.Author != old.Author || !c.CreatedAt.Equal(old.CreatedAt) {
			t.Errorf("change %d: %s %q %+v, want %s %q %+v", i, c.ChangeID, c.Intent, c.Author, old.ChangeID, old.Intent, old.Author)
		}
		if c.Hash == old.Hash || c.FirstParent() == old.FirstParent() {
			t.Errorf("change %s kept its hash or parent", c.ChangeID)
		}
		if !c.Rebased() || c.Rebase.From != old.Hash || c.Rebase.Onto != main.Hash || c.Rebase.Branch != "main" || c.Rebase.By != "user:alice" || c.Rebase.ResolvedBy != "" {
			t.Errorf("change %s rebase = %+v", c.ChangeID, c.Rebase)
		}
	}
	if chain[0].FirstParent() != main.Hash {
		t.Fatalf("first rebased change sits on %s, want main %s", chain[0].FirstParent().Short(), main.Hash.Short())
	}
	if chain[1].ChangeID != c2.ChangeID || chain[1].Evidence == nil || len(chain[1].Evidence.Checks) != 1 || !chain[1].Evidence.Checks[0].RanByVX {
		t.Fatalf("evidence not kept: %+v", chain[1].Evidence)
	}
	got := e.read(repo, after.Head)
	if got["README.md"] != "# shop\n" || got["limit.go"] != "package limit\n" || !strings.Contains(got["auth.go"], "limit()") {
		t.Fatalf("rebased tree = %v", got)
	}
	// The change id now resolves to the rebased version.
	_, entry, err := e.svc.GetChangeset(e.ctx, e.alice, repo.Name, c3.ChangeID)
	if err != nil || entry.Hash != after.Head {
		t.Fatalf("change id %s resolves to %s, want the rebased head %s (%v)", c3.ChangeID, entry.Hash.Short(), after.Head.Short(), err)
	}
	// Up to date now.
	v, err = e.svc.RebaseEffort(e.ctx, e.alice, repo.Name, eff.Slug, "")
	if err != nil || !v.UpToDate {
		t.Fatalf("second rebase = %+v %v", v, err)
	}
}

// conflicting sets up an effort whose second change rewrites the line main
// also rewrites, and starts a rebase that stops there.
func conflicting(t *testing.T) (*env, store.Repo, store.Effort, store.ChangesetRow, service.RebaseView) {
	t.Helper()
	e := newEnv(t)
	repo, eff := e.setup()
	e.land(repo, eff, "Add limiter", map[string]*string{"limit.go": s("package limit\n")}, passing)
	c := e.land(repo, eff, "Rename Login to SignIn", map[string]*string{"auth.go": s("func SignIn() {\n}\n\nfunc Logout() {\n}\n")}, passing)
	e.land(repo, eff, "Document limiter", map[string]*string{"limit.go": s("// Package limit limits.\npackage limit\n")}, nil)
	// main gets the seed file too (a first rebase replays cleanly), then
	// changes the line the second change rewrites.
	e.moveMain(repo, "Add auth on main", map[string]*string{"auth.go": s("func Login() {\n}\n\nfunc Logout() {\n}\n")})
	if v, err := e.svc.RebaseEffort(e.ctx, e.alice, repo.Name, eff.Slug, ""); err != nil || v.Rebase.State != store.RebaseDone {
		t.Fatalf("first rebase: %+v %v", v.Rebase, err)
	}
	e.moveMain(repo, "Rename Login to LogIn", map[string]*string{"auth.go": s("func LogIn() {\n}\n\nfunc Logout() {\n}\n")})
	eff = e.effort(eff)
	c, _ = e.svc.Store.ResolveChangeset(e.ctx, repo.ID, c.ChangeID)
	v, err := e.svc.RebaseEffort(e.ctx, e.alice, repo.Name, eff.Slug, "")
	if err != nil {
		t.Fatal(err)
	}
	return e, repo, eff, c, v
}

func TestRebaseStopsOnConflictAndContinues(t *testing.T) {
	e, repo, eff, c, v := conflicting(t)
	if v.Rebase.State != store.RebaseStopped || v.StoppedAt == nil || v.StoppedAt.ChangeID != c.ChangeID {
		t.Fatalf("rebase should stop at %s: %+v", c.ChangeID, v.Rebase)
	}
	if len(v.Rebase.Conflicts) != 1 || v.Rebase.Conflicts[0].Path != "auth.go" || v.Rebase.Conflicts[0].Generated {
		t.Fatalf("conflicts = %+v", v.Rebase.Conflicts)
	}
	if len(v.Rebase.Replayed) != 2 || len(v.Rebase.Pending) != 2 || v.BaseTree.IsZero() || v.ChangeTree != c.Tree || v.OntoTree.IsZero() {
		t.Fatalf("stopped state = %+v", v)
	}
	// Nothing moved yet.
	if now := e.effort(eff); now.Head != eff.Head || now.Base != eff.Base || now.Version != eff.Version {
		t.Fatalf("a stopped rebase moved the effort: %+v", now)
	}
	// Only one rebase at a time.
	if _, err := e.svc.RebaseEffort(e.ctx, e.agent, repo.Name, eff.Slug, ""); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second rebase: %v", err)
	}
	got, ok, err := e.svc.GetRebase(e.ctx, e.alice, repo.Name, eff.Slug)
	if err != nil || !ok || got.Rebase.ID != v.Rebase.ID || got.StartedBy != "user:alice" {
		t.Fatalf("GetRebase = %+v %v %v", got, ok, err)
	}
	ev, _ := e.svc.GetEffort(e.ctx, e.alice, repo.Name, eff.Slug)
	if ev.Rebase == nil || ev.Rebase.ID != v.Rebase.ID {
		t.Fatalf("effort view has no rebase: %+v", ev.Rebase)
	}

	// The agent resolves on top of the rebased chain so far.
	cur, _ := e.svc.Store.GetChangeset(e.ctx, repo.ID, v.Rebase.NewHead)
	resolved := e.treeWith(repo, cur.Hash, map[string]*string{"auth.go": s("func SignIn() {\n}\n\nfunc Logout() {\n}\n")})
	if _, err := e.svc.ContinueRebase(e.ctx, e.agent, service.ContinueInput{Repo: repo.Name, Effort: eff.Slug, StoppedAt: v.Rebase.Pending[1], Tree: resolved}); err == nil {
		t.Fatal("continue with the wrong change should fail")
	}
	done, err := e.svc.ContinueRebase(e.ctx, e.agent, service.ContinueInput{Repo: repo.Name, Effort: eff.Slug, StoppedAt: c.Hash, Tree: resolved})
	if err != nil || done.Rebase.State != store.RebaseDone {
		t.Fatalf("continue: %+v %v", done.Rebase, err)
	}
	chain := e.chain(repo, e.effort(eff))
	if len(chain) != 4 {
		t.Fatalf("chain = %d changes", len(chain))
	}
	r := chain[2]
	if r.ChangeID != c.ChangeID || r.Rebase == nil || r.Rebase.ResolvedBy != "agent:alice/claude" || r.Rebase.By != "agent:alice/claude" || r.Tree != resolved {
		t.Fatalf("resolved change = %+v %+v", r.Changeset, r.Rebase)
	}
	if chain[1].Rebase.ResolvedBy != "" || chain[1].Rebase.By != "user:alice" {
		t.Fatalf("clean change before the stop = %+v", chain[1].Rebase)
	}
	files := e.read(repo, e.effort(eff).Head)
	if files["auth.go"] != "func SignIn() {\n}\n\nfunc Logout() {\n}\n" || files["limit.go"] != "// Package limit limits.\npackage limit\n" {
		t.Fatalf("final tree = %v", files)
	}
	if _, err := e.svc.ContinueRebase(e.ctx, e.agent, service.ContinueInput{Repo: repo.Name, Effort: eff.Slug, StoppedAt: c.Hash, Tree: resolved}); err == nil || !strings.Contains(err.Error(), "no rebase in progress") {
		t.Fatalf("continue after done: %v", err)
	}
}

func TestRebaseAbortLeavesEffortUnchanged(t *testing.T) {
	e, repo, eff, c, v := conflicting(t)
	if v.Rebase.State != store.RebaseStopped {
		t.Fatalf("not stopped: %+v", v.Rebase)
	}
	a, err := e.svc.AbortRebase(e.ctx, e.agent, repo.Name, eff.Slug)
	if err != nil || a.Rebase.State != store.RebaseAborted {
		t.Fatalf("abort: %+v %v", a.Rebase, err)
	}
	now := e.effort(eff)
	if now.Head != eff.Head || now.Base != eff.Base || now.Version != eff.Version {
		t.Fatalf("abort moved the effort: %+v", now)
	}
	// The change id still resolves to the effort's own version, not to the
	// replayed one the aborted rebase stored.
	_, entry, err := e.svc.GetChangeset(e.ctx, e.alice, repo.Name, c.ChangeID)
	if err != nil || entry.Hash != c.Hash {
		t.Fatalf("change %s resolves to %s after abort, want %s (%v)", c.ChangeID, entry.Hash.Short(), c.Hash.Short(), err)
	}
	for _, le := range e.chain(repo, now) {
		if le.Rebased() && le.Rebase.Onto == v.Rebase.Onto {
			t.Fatalf("change %s is marked rebased onto the aborted target", le.ChangeID)
		}
	}
	if _, err := e.svc.AbortRebase(e.ctx, e.agent, repo.Name, eff.Slug); err == nil {
		t.Fatal("second abort should fail")
	}
	// A new rebase can start and stops at the same place.
	v2, err := e.svc.RebaseEffort(e.ctx, e.agent, repo.Name, eff.Slug, "")
	if err != nil || v2.Rebase.State != store.RebaseStopped || v2.Rebase.ID == v.Rebase.ID {
		t.Fatalf("rebase after abort: %+v %v", v2.Rebase, err)
	}
}

func TestRebaseReplaysLandsMadeWhileStopped(t *testing.T) {
	e, repo, eff, c, v := conflicting(t)
	// Someone lands on the old chain while the rebase waits.
	late := e.land(repo, eff, "Add audit", map[string]*string{"audit.go": s("package audit\n")}, passing)
	if now := e.effort(eff); now.Head != late.Hash {
		t.Fatalf("land during a stopped rebase did not land on the effort: head %s", now.Head.Short())
	}
	cur, _ := e.svc.Store.GetChangeset(e.ctx, repo.ID, v.Rebase.NewHead)
	resolved := e.treeWith(repo, cur.Hash, map[string]*string{"auth.go": s("func SignIn() {\n}\n\nfunc Logout() {\n}\n")})
	done, err := e.svc.ContinueRebase(e.ctx, e.agent, service.ContinueInput{Repo: repo.Name, Effort: eff.Slug, StoppedAt: c.Hash, Tree: resolved})
	if err != nil || done.Rebase.State != store.RebaseDone {
		t.Fatalf("continue: %+v %v", done.Rebase, err)
	}
	now := e.effort(eff)
	chain := e.chain(repo, now)
	last := chain[len(chain)-1]
	if last.ChangeID != late.ChangeID || !last.Rebased() || last.Rebase.From != late.Hash || done.Rebase.SeenHead != late.Hash {
		t.Fatalf("late land not replayed last: %s %+v", last.ChangeID, last.Rebase)
	}
	files := e.read(repo, now.Head)
	if files["audit.go"] != "package audit\n" || files["auth.go"] != "func SignIn() {\n}\n\nfunc Logout() {\n}\n" {
		t.Fatalf("final tree = %v", files)
	}
}

func TestWorkspaceOpenedBeforeRebaseStillLands(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Add Logout audit")
	if err != nil {
		t.Fatal(err)
	}
	oldBase := ws.Workspace.Base
	e.moveMain(repo, "Docs", map[string]*string{"README.md": s("# shop\n")})
	if v, err := e.svc.RebaseEffort(e.ctx, e.alice, repo.Name, eff.Slug, ""); err != nil || v.Rebase.State != store.RebaseDone {
		t.Fatalf("rebase: %+v %v", v.Rebase, err)
	}
	// Its files still sit on the old chain: that base is still accepted.
	if _, err := e.svc.SetBase(e.ctx, e.agent, ws.Workspace.ID, oldBase); err != nil {
		t.Fatalf("SetBase on the old chain: %v", err)
	}
	tree := e.snapshot(repo, oldBase, map[string]*string{"auth.go": s("func Login() {\n}\n\nfunc Logout() {\n\taudit()\n}\n")})
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Base: oldBase, Tree: tree})
	if err != nil || !res.Landed || !res.Rebased {
		t.Fatalf("land after rebase: %+v %v", res, err)
	}
	files := e.read(repo, res.EffortHead)
	if files["README.md"] != "# shop\n" || !strings.Contains(files["auth.go"], "audit()") {
		t.Fatalf("landed tree = %v", files)
	}
	// A base from nowhere is still refused.
	other := e.moveMain(repo, "Unrelated", map[string]*string{"x.go": s("package x\n")})
	ws2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "x")
	if _, err := e.svc.SetBase(e.ctx, e.agent, ws2.Workspace.ID, other.Hash); err == nil {
		t.Fatal("SetBase accepted a changeset that is not part of the effort")
	}
}

// A generated file the effort and the branch both changed is not
// text-merged: the rebase keeps the effort's version, finishes, and the
// replayed change warns to regenerate.
func TestRebaseKeepsEffortsGeneratedFiles(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	gen := func(body string) *string {
		return s("// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage v1\n\n" + body)
	}
	e.moveMain(repo, "Add pb", map[string]*string{
		"gen/v1/vx.pb.go": gen("var A = 1\n"),
		"api/x.txt":       s("x\n"),
		"vx.toml":         s("[[generated]]\nglob = \"api/**\"\ncommand = \"make api\"\n"),
	})
	eff = e.effort(eff)
	if v, err := e.svc.RebaseEffort(e.ctx, e.alice, repo.Name, eff.Slug, ""); err != nil || v.Rebase.State != store.RebaseDone {
		t.Fatalf("first rebase: %+v %v", v.Rebase, err)
	}
	eff = e.effort(eff)
	e.land(repo, eff, "Regenerate with B", map[string]*string{"gen/v1/vx.pb.go": gen("var A = 2\n"), "api/x.txt": s("effort\n")}, nil)
	e.moveMain(repo, "Regenerate with C", map[string]*string{"gen/v1/vx.pb.go": gen("var A = 3\n"), "api/x.txt": s("main\n")})
	v, err := e.svc.RebaseEffort(e.ctx, e.alice, repo.Name, eff.Slug, "")
	if err != nil || v.Rebase.State != store.RebaseDone {
		t.Fatalf("rebase: %+v %v", v.Rebase, err)
	}
	eff = e.effort(eff)
	files := e.read(repo, eff.Head)
	if !strings.Contains(files["gen/v1/vx.pb.go"], "var A = 2") || strings.Contains(files["gen/v1/vx.pb.go"], "<<<<<<<") || files["api/x.txt"] != "effort\n" {
		t.Fatalf("rebased files = %q", files)
	}
	head, err := e.svc.Store.GetChangeset(e.ctx, repo.ID, eff.Head)
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, w := range head.Warnings {
		details = append(details, w.Kind+": "+w.Detail)
	}
	want := []string{
		"generated: generated file api/x.txt: regenerate with `make api`",
		"generated: generated file gen/v1/vx.pb.go: regenerate it (both sides changed it; vx kept one side instead of merging the text)",
	}
	if strings.Join(details, "\n") != strings.Join(want, "\n") {
		t.Fatalf("warnings:\n%s", strings.Join(details, "\n"))
	}
}
