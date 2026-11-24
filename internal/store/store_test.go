package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

func TestPrincipalsAndTokens(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	alice, err := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate handle err = %v", err)
	}
	if _, err := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/claude"}); err == nil {
		t.Fatal("agent without parent should fail")
	}
	agent, err := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/claude", ParentID: &alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Ref() != "agent:alice/claude" || alice.Ref() != "user:alice" {
		t.Fatalf("refs %q %q", agent.Ref(), alice.Ref())
	}
	for _, bad := range []string{"Alice", "a b", "x/y"} {
		if _, err := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: bad}); err == nil {
			t.Errorf("handle %q accepted", bad)
		}
	}
	tok, err := s.CreateToken(ctx, agent.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Authenticate(ctx, tok)
	if err != nil || got.ID != agent.ID {
		t.Fatalf("Authenticate = %v, %v", got.Handle, err)
	}
	if _, err := s.Authenticate(ctx, tok+"x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bad token err = %v", err)
	}
	agents, err := s.ListAgents(ctx, alice.ID)
	if err != nil || len(agents) != 1 {
		t.Fatalf("ListAgents = %d, %v", len(agents), err)
	}
}

func TestChangesetsAndEfforts(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	owner, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	repo, err := s.CreateRepo(ctx, store.Repo{Name: "acme/shop-api", Provider: store.ProviderGitHub, Remote: "acme/shop-api"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepo(ctx, store.Repo{Name: "acme/shop-api"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate repo err = %v", err)
	}
	got, err := s.RepoByName(ctx, "acme/shop-api")
	if err != nil || got.DefaultBranch != "main" || got.Provider != store.ProviderGitHub {
		t.Fatalf("RepoByName = %+v, %v", got, err)
	}

	mk := func(parent hash.Hash, intent string, origin object.Origin) *object.Changeset {
		cs := &object.Changeset{
			Version: object.CurrentVersion, ChangeID: object.NewChangeID(), Tree: hash.Of(hash.KindTree, []byte(intent)),
			Origin: origin, Intent: intent, CreatedAt: time.Now(),
		}
		if !parent.IsZero() {
			cs.Parents = []hash.Hash{parent}
		}
		return cs
	}
	root := mk(hash.Zero, "Initial import", object.OriginUpstream)
	root.GitSHA = "abc123"
	h0, err := s.InsertChangeset(ctx, repo.ID, nil, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.InsertChangeset(ctx, repo.ID, nil, root, nil)
	if err != nil || again != h0 {
		t.Fatalf("re-insert should be a no-op: %v", err)
	}
	if err := s.UpsertBranch(ctx, store.Branch{RepoID: repo.ID, Name: "main", Head: h0, GitSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if b, err := s.GetBranch(ctx, repo.ID, "main"); err != nil || b.Head != h0 {
		t.Fatalf("GetBranch = %+v, %v", b, err)
	}
	if r, err := s.ChangesetByGitSHA(ctx, repo.ID, "abc123"); err != nil || r.Hash != h0 {
		t.Fatalf("ChangesetByGitSHA = %v", err)
	}

	eff, err := s.CreateEffort(ctx, store.Effort{RepoID: repo.ID, Name: "Rate limiting and login audit", BaseBranch: "main", Base: h0, OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Slug != "rate-limiting-and-login-audit" || eff.Head != h0 || eff.Version != 0 {
		t.Fatalf("effort = %+v", eff)
	}
	eff2, err := s.CreateEffort(ctx, store.Effort{RepoID: repo.ID, Name: "Rate limiting and login audit", BaseBranch: "main", Base: h0, OwnerID: owner.ID})
	if err != nil || eff2.Slug != "rate-limiting-and-login-audit-2" {
		t.Fatalf("second effort slug = %q, %v", eff2.Slug, err)
	}

	c1 := mk(h0, "Add Redis config", object.OriginVX)
	files := []store.FileChange{{Change: object.Change{Path: "config/redis.go", Op: object.OpAdd, New: object.File{Hash: hash.Of(hash.KindBlob, []byte("r")), Mode: object.ModeFile}}, Added: 24}}
	h1, err := s.InsertChangeset(ctx, repo.ID, &eff.ID, c1, files)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.AdvanceEffort(ctx, eff.ID, 0, h1)
	if err != nil || !ok {
		t.Fatalf("AdvanceEffort = %v, %v", ok, err)
	}
	if ok, _ := s.AdvanceEffort(ctx, eff.ID, 0, h1); ok {
		t.Fatal("stale version must lose the compare-and-swap")
	}
	c2 := mk(h1, "Add middleware", object.OriginVX)
	h2, _ := s.InsertChangeset(ctx, repo.ID, &eff.ID, c2, nil)
	if ok, _ := s.AdvanceEffort(ctx, eff.ID, 1, h2); !ok {
		t.Fatal("advance at version 1 failed")
	}

	chain, err := s.Chain(ctx, repo.ID, h2, h0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || chain[0].Hash != h2 || chain[1].Hash != h1 {
		t.Fatalf("chain = %v", len(chain))
	}
	all, _ := s.Chain(ctx, repo.ID, h2, hash.Zero, 100)
	if len(all) != 3 {
		t.Fatalf("full chain = %d", len(all))
	}
	limited, _ := s.Chain(ctx, repo.ID, h2, hash.Zero, 1)
	if len(limited) != 1 {
		t.Fatalf("limited chain = %d", len(limited))
	}

	fc, err := s.FileChanges(ctx, repo.ID, h1)
	if err != nil || len(fc) != 1 || fc[0].Path != "config/redis.go" || fc[0].Added != 24 || fc[0].New.Mode != object.ModeFile {
		t.Fatalf("FileChanges = %+v, %v", fc, err)
	}
	for _, ref := range []string{h1.String(), h1.Short(), c1.ChangeID} {
		r, err := s.ResolveChangeset(ctx, repo.ID, ref)
		if err != nil || r.Hash != h1 {
			t.Errorf("ResolveChangeset(%q) = %v", ref, err)
		}
	}

	w, err := s.CreateWorkspace(ctx, store.Workspace{EffortID: eff.ID, OwnerID: owner.ID, Intent: "Wire limiter", Base: h2})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWorkspace(ctx, w.ID, store.WorkspaceLanded, h2); err != nil {
		t.Fatal(err)
	}
	w, _ = s.GetWorkspace(ctx, w.ID)
	if w.State != store.WorkspaceLanded || w.LandedAs != h2 {
		t.Fatalf("workspace = %+v", w)
	}
	if err := s.LogOp(ctx, &repo.ID, &owner.ID, "land", eff.Slug, map[string]string{"head": h1.String()}, map[string]string{"head": h2.String()}); err != nil {
		t.Fatal(err)
	}
	ops, err := s.ListOps(ctx, repo.ID, 10)
	if err != nil || len(ops) != 1 || ops[0].Op != "land" {
		t.Fatalf("ops = %+v, %v", ops, err)
	}
}
