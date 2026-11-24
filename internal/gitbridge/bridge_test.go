package gitbridge_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	gitobject "github.com/go-git/go-git/v5/plumbing/object"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// upstream is a local git repository standing in for GitHub.
type upstream struct {
	t    *testing.T
	dir  string
	repo *git.Repository
	wt   *git.Worktree
	n    int
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	dir := t.TempDir()
	r, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	return &upstream{t: t, dir: dir, repo: r, wt: wt}
}

func (u *upstream) commit(msg string, files map[string]string) plumbing.Hash {
	u.t.Helper()
	for p, c := range files {
		full := filepath.Join(u.dir, p)
		if c == "" {
			if _, err := u.wt.Remove(p); err != nil {
				u.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			u.t.Fatal(err)
		}
		perm := os.FileMode(0o644)
		if filepath.Ext(p) == ".sh" {
			perm = 0o755
		}
		if err := os.WriteFile(full, []byte(c), perm); err != nil {
			u.t.Fatal(err)
		}
		if _, err := u.wt.Add(p); err != nil {
			u.t.Fatal(err)
		}
	}
	u.n++
	h, err := u.wt.Commit(msg, &git.CommitOptions{Author: &gitobject.Signature{
		Name: "Bob", Email: "bob@example.com", When: time.Date(2026, 11, 1, 10, u.n, 0, 0, time.UTC),
	}})
	if err != nil {
		u.t.Fatal(err)
	}
	return h
}

func (u *upstream) branch(name string) {
	u.t.Helper()
	head, _ := u.repo.Head()
	if err := u.wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(name), Create: true, Hash: head.Hash()}); err != nil {
		u.t.Fatal(err)
	}
}

func setup(t *testing.T) (*service.Service, *gitbridge.Bridge, service.Actor, *upstream) {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	blobs := blob.NewMem()
	br := gitbridge.New(st, blobs, t.TempDir(), nil)
	svc := service.New(st, blobs, br)
	p, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := svc.ResolveActor(ctx, p)
	return svc, br, actor, newUpstream(t)
}

func files(t *testing.T, svc *service.Service, repo store.Repo, cs store.Branch) map[string]string {
	t.Helper()
	ctx := context.Background()
	row, err := svc.Store.GetChangeset(ctx, repo.ID, cs.Head)
	if err != nil {
		t.Fatal(err)
	}
	m, err := object.ReadManifest(ctx, svc.Blobs, row.Tree)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for p, f := range m {
		_, data, _ := svc.Blobs.Get(ctx, f.Hash)
		out[p] = string(data)
		if f.Mode == object.ModeExec {
			out[p] = "exec:" + out[p]
		}
	}
	return out
}

func TestImportAndIncrementalSync(t *testing.T) {
	ctx := context.Background()
	svc, _, actor, up := setup(t)
	up.commit("Initial", map[string]string{"README.md": "# shop\n", "cmd/run.sh": "#!/bin/sh\n", "pkg/a.go": "package pkg\n"})
	tip := up.commit("Add b", map[string]string{"pkg/b.go": "package pkg // b\n"})

	repo, err := svc.CreateRepo(ctx, actor, service.CreateRepoInput{Name: "shop", Provider: store.ProviderGit, Remote: up.dir})
	if err != nil {
		t.Fatal(err)
	}
	if repo.DefaultBranch != "main" {
		t.Fatalf("default branch = %q", repo.DefaultBranch)
	}
	main, err := svc.Store.GetBranch(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if main.GitSHA != tip.String() {
		t.Fatalf("branch at %s, want %s", main.GitSHA, tip)
	}
	got := files(t, svc, repo, main)
	if got["pkg/b.go"] != "package pkg // b\n" || got["cmd/run.sh"] != "exec:#!/bin/sh\n" || len(got) != 4 {
		t.Fatalf("imported files = %v", got)
	}
	row, _ := svc.Store.GetChangeset(ctx, repo.ID, main.Head)
	if row.Origin != object.OriginUpstream || row.Author.Email != "bob@example.com" || row.Intent != "Add b" || len(row.Parents) != 0 {
		t.Fatalf("imported changeset = %+v", row.Changeset)
	}

	// Three more commits upstream; a sync imports exactly those, in order.
	up.commit("Edit a", map[string]string{"pkg/a.go": "package pkg // a2\n"})
	up.commit("Remove readme", map[string]string{"README.md": ""})
	last := up.commit("Add c", map[string]string{"pkg/c.go": "package pkg // c\n"})
	b, n, err := svc.SyncBranch(ctx, actor, "shop", "main")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || b.GitSHA != last.String() {
		t.Fatalf("sync imported %d, head %s", n, b.GitSHA)
	}
	chain, err := svc.Store.Chain(ctx, repo.ID, b.Head, main.Head, 10)
	if err != nil || len(chain) != 3 || chain[0].Intent != "Add c" || chain[2].Intent != "Edit a" {
		t.Fatalf("chain = %d, %v", len(chain), err)
	}
	got = files(t, svc, repo, b)
	if _, ok := got["README.md"]; ok || got["pkg/a.go"] != "package pkg // a2\n" || got["pkg/c.go"] == "" {
		t.Fatalf("synced files = %v", got)
	}
	// Syncing again is a no-op.
	if _, n, err := svc.SyncBranch(ctx, actor, "shop", "main"); err != nil || n != 0 {
		t.Fatalf("second sync = %d, %v", n, err)
	}
	// Re-importing the same content keeps changeset hashes stable.
	fc, _ := svc.Store.FileChanges(ctx, repo.ID, chain[0].Hash)
	if len(fc) != 1 || fc[0].Path != "pkg/c.go" || fc[0].Added != 1 {
		t.Fatalf("file changes = %+v", fc)
	}
}

func TestEffortOnAnyBranchOnTheFly(t *testing.T) {
	ctx := context.Background()
	svc, _, actor, up := setup(t)
	up.commit("Initial", map[string]string{"a.go": "a\n"})
	up.branch("release/1.4")
	up.commit("Release fix", map[string]string{"a.go": "a fixed\n"})

	if _, err := svc.CreateRepo(ctx, actor, service.CreateRepoInput{Name: "shop", Provider: store.ProviderGit, Remote: up.dir, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	// vx has never seen release/1.4; creating an effort on it imports it.
	eff, err := svc.CreateEffort(ctx, actor, service.CreateEffortInput{Repo: "shop", Name: "Hotfix", BaseBranch: "release/1.4"})
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := svc.Store.RepoByName(ctx, "shop")
	b, err := svc.Store.GetBranch(ctx, repo.ID, "release/1.4")
	if err != nil || eff.Base != b.Head {
		t.Fatalf("effort base %s, branch %+v, %v", eff.Base.Short(), b, err)
	}
	if got := files(t, svc, repo, b); got["a.go"] != "a fixed\n" {
		t.Fatalf("release files = %v", got)
	}
	if _, err := svc.CreateEffort(ctx, actor, service.CreateEffortInput{Repo: "shop", Name: "Nope", BaseBranch: "does-not-exist"}); err == nil {
		t.Fatal("effort on a missing branch should fail")
	}
}
