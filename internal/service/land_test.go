package service_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

type env struct {
	t     *testing.T
	ctx   context.Context
	svc   *service.Service
	blobs blob.Store
	alice service.Actor
	agent service.Actor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	blobs := blob.NewMem()
	svc := service.New(st, blobs, nil)
	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/claude", ParentID: &alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := svc.ResolveActor(ctx, alice)
	g, _ := svc.ResolveActor(ctx, ag)
	return &env{t: t, ctx: ctx, svc: svc, blobs: blobs, alice: a, agent: g}
}

// snapshot writes files on top of the tree of changeset base and returns the
// new root tree. A nil value deletes the file.
func (e *env) snapshot(repo store.Repo, base hash.Hash, files map[string]*string) hash.Hash {
	e.t.Helper()
	row, err := e.svc.Store.GetChangeset(e.ctx, repo.ID, base)
	if err != nil {
		e.t.Fatal(err)
	}
	var changes []object.Change
	for p, c := range files {
		if c == nil {
			changes = append(changes, object.Change{Path: p, Op: object.OpDelete})
			continue
		}
		h, err := e.blobs.Put(e.ctx, hash.KindBlob, []byte(*c))
		if err != nil {
			e.t.Fatal(err)
		}
		changes = append(changes, object.Change{Path: p, Op: object.OpModify, New: object.File{Hash: h, Mode: object.ModeFile}})
	}
	tree, err := object.ApplyChanges(e.ctx, e.blobs, row.Tree, changes)
	if err != nil {
		e.t.Fatal(err)
	}
	storetest.AddTree(e.t, e.svc.Store, e.blobs, repo.ID, row.Tree, tree)
	return tree
}

func (e *env) read(repo store.Repo, cs hash.Hash) map[string]string {
	e.t.Helper()
	row, err := e.svc.Store.GetChangeset(e.ctx, repo.ID, cs)
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := object.ReadManifest(e.ctx, e.blobs, row.Tree)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]string{}
	for p, f := range m {
		_, data, _ := e.blobs.Get(e.ctx, f.Hash)
		out[p] = string(data)
	}
	return out
}

func s(v string) *string { return &v }

func (e *env) setup() (store.Repo, store.Effort) {
	e.t.Helper()
	repo, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "shop-api"})
	if err != nil {
		e.t.Fatal(err)
	}
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "shop-api", Name: "Rate limiting"})
	if err != nil {
		e.t.Fatal(err)
	}
	// Seed the effort with one file through a first land.
	ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, "shop-api", eff.Slug, "Add auth")
	if err != nil {
		e.t.Fatal(err)
	}
	tree := e.snapshot(repo, ws.Workspace.Base, map[string]*string{"auth.go": s("func Login() {\n}\n\nfunc Logout() {\n}\n")})
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree, Model: "claude-opus-5-5"})
	if err != nil || !res.Landed {
		e.t.Fatalf("seed land: %+v %v", res, err)
	}
	if res.Changeset.Author.OnBehalfOf != "user:alice" || res.Changeset.Author.Principal != "agent:alice/claude" {
		e.t.Fatalf("provenance = %+v", res.Changeset.Author)
	}
	eff, _ = e.svc.Store.EffortByID(e.ctx, eff.ID)
	return repo, eff
}

func TestLandFastPathAndRebase(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()

	// Two workspaces start from the same head.
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Add ctx to Login")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Audit Logout")
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{"auth.go": s("func Login(ctx) {\n}\n\nfunc Logout() {\n}\n")})
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{"auth.go": s("func Login() {\n}\n\nfunc Logout() {\n\taudit()\n}\n"), "audit.go": s("package audit\n")})

	r1, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1})
	if err != nil || !r1.Landed || r1.Rebased {
		t.Fatalf("first land: %+v %v", r1, err)
	}
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil || !r2.Landed || !r2.Rebased || len(r2.AutoMerged) != 1 {
		t.Fatalf("second land should rebase and auto-merge: %+v %v", r2, err)
	}
	got := e.read(repo, r2.EffortHead)
	if got["auth.go"] != "func Login(ctx) {\n}\n\nfunc Logout() {\n\taudit()\n}\n" || got["audit.go"] != "package audit\n" {
		t.Fatalf("merged effort = %v", got)
	}
	if r2.Changeset.FirstParent() != r1.Changeset.Hash {
		t.Fatal("second change must sit on the first")
	}
	// Landing a finished workspace again fails.
	if _, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1}); err == nil {
		t.Fatal("re-landing a landed workspace should fail")
	}
	log, err := e.svc.Log(e.ctx, e.alice, repo.Name, eff.Slug, 0, true)
	if err != nil || len(log) != 3 {
		t.Fatalf("log = %d entries, %v", len(log), err)
	}
	if log[0].Intent != "Audit Logout" || len(log[0].Files) != 2 {
		t.Fatalf("newest entry = %q with %d files", log[0].Intent, len(log[0].Files))
	}
	var added int
	for _, f := range log[0].Files {
		added += f.Added
	}
	if added != 2 {
		t.Fatalf("line stats added = %d, want 2", added)
	}
}

func TestLandConflictKeepsWorkspaceOpen(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Rename Login to SignIn")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Rename Login to LogIn")
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{"auth.go": s("func SignIn() {\n}\n\nfunc Logout() {\n}\n")})
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{"auth.go": s("func LogIn() {\n}\n\nfunc Logout() {\n}\n")})
	if r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1}); err != nil || !r.Landed {
		t.Fatal(err)
	}
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Landed || len(r2.Conflicts) != 1 || r2.Conflicts[0].Path != "auth.go" {
		t.Fatalf("expected one conflict on auth.go: %+v", r2)
	}
	w, _ := e.svc.GetWorkspace(e.ctx, e.alice, w2.Workspace.ID)
	if w.Workspace.State != store.WorkspaceOpen {
		t.Fatal("workspace must stay open after a conflict")
	}
}

func TestLandValidation(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	w, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Nothing")
	row, _ := e.svc.Store.GetChangeset(e.ctx, repo.ID, w.Workspace.Base)
	_, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w.Workspace.ID, Tree: row.Tree})
	if !errors.Is(err, service.ErrNothingToLand) {
		t.Fatalf("unchanged snapshot err = %v", err)
	}
	// Someone else cannot land your workspace.
	if _, err := e.svc.Land(e.ctx, e.alice, service.LandInput{WorkspaceID: w.Workspace.ID, Tree: row.Tree}); err == nil {
		t.Fatal("foreign land should fail")
	}
	// A tree that references content the server does not have is rejected.
	missing := hash.Of(hash.KindBlob, []byte("never uploaded"))
	tree, err := object.ApplyChanges(e.ctx, e.blobs, row.Tree, []object.Change{{Path: "x.go", Op: object.OpAdd, New: object.File{Hash: missing, Mode: object.ModeFile}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Store.AddRepoObjects(e.ctx, w.Repo.ID, []hash.Hash{tree}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w.Workspace.ID, Tree: tree})
	var se *service.Error
	if !errors.As(err, &se) || se.Code != service.CodeFailedPrecondition {
		t.Fatalf("missing content err = %v", err)
	}
	// Abandon closes it.
	if _, err := e.svc.Abandon(e.ctx, e.agent, w.Workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "  "); err == nil {
		t.Fatal("empty intent should fail")
	}
}

// TestConcurrentLands has many agents land into one effort at once. Every
// change must end up in the effort, none lost to the race.
func TestConcurrentLands(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		w, err := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, fmt.Sprintf("Add file %d", i))
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]*string{fmt.Sprintf("pkg/f%02d.go", i): s(fmt.Sprintf("package pkg // %d\n", i))}
		tree := e.snapshot(repo, w.Workspace.Base, files)
		wg.Add(1)
		go func(id store.Workspace, tree hash.Hash) {
			defer wg.Done()
			r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: id.ID, Tree: tree})
			if err == nil && !r.Landed {
				err = fmt.Errorf("conflicts: %+v", r.Conflicts)
			}
			errs <- err
		}(w.Workspace, tree)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	final, _ := e.svc.Store.EffortByID(e.ctx, eff.ID)
	got := e.read(repo, final.Head)
	if len(got) != n+1 {
		t.Fatalf("effort has %d files, want %d", len(got), n+1)
	}
	log, _ := e.svc.Log(e.ctx, e.alice, repo.Name, eff.Slug, 0, false)
	if len(log) != n+1 {
		t.Fatalf("log has %d entries, want %d", len(log), n+1)
	}
}
