package service_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// A one-file snapshot (limit.go: "package shop\n") that is the base tree
// itself, on a repository whose imported base never had its objects
// recorded, says there is nothing to land; it used to say "snapshot trees
// are missing on the server: upload objects first (1 trees ...)", which no
// upload could fix.
func TestLandTinyTreeEqualToBase(t *testing.T) {
	e := newFeedbackEnv(t)
	// main as an old import left it: the changeset is there, its tree is
	// not recorded as part of the repository.
	tree := e.treeOf(nil, map[string]*string{"limit.go": s("package shop\n")})
	cs := &object.Changeset{
		Version: object.CurrentVersion, ChangeID: object.NewChangeID(), Tree: tree, Origin: object.OriginUpstream,
		Author: object.Author{Name: "Bob", Email: "bob@example.com"}, Intent: "Initial", CreatedAt: e.svc.Clock(), GitSHA: gitSHA("tiny"),
	}
	h, err := e.svc.Store.InsertChangeset(e.ctx, e.repo.ID, nil, cs, nil)
	e.must(err)
	e.must(e.svc.Store.UpsertBranch(e.ctx, store.Branch{RepoID: e.repo.ID, Name: "main", Head: h, GitSHA: cs.GitSHA}))
	if miss, err := e.svc.Store.RepoObjectsMissing(e.ctx, e.repo.ID, []hash.Hash{tree}); err != nil || len(miss) != 1 {
		t.Fatalf("setup: tree should not be recorded: %v %v", miss, err)
	}

	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: e.repo.Name, Name: "Limiter"})
	e.must(err)
	ws, err := e.svc.OpenWorkspace(e.ctx, e.claude, e.repo.Name, eff.Slug, "Add limiter")
	e.must(err)
	_, err = e.svc.Land(e.ctx, e.claude, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
	if !errors.Is(err, service.ErrNothingToLand) || strings.Contains(err.Error(), "missing") {
		t.Fatalf("land of the base tree: %v", err)
	}

	// A real one-file change on top of it uploads and lands.
	next := e.snapshot(e.repo, ws.Workspace.Base, map[string]*string{"limit.go": s("package shop\n\nconst Limit = 10\n")})
	res, err := e.svc.Land(e.ctx, e.claude, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: next})
	if err != nil || !res.Landed {
		t.Fatalf("one-file land: %+v %v", res, err)
	}
}

// The reported case: the test host's main already has limit.go as
// "package shop\n", so landing it again is nothing to land.
func TestLandTinyTreeOnFeedbackEnv(t *testing.T) {
	e := newFeedbackEnv(t)
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: e.repo.Name, Name: "Limiter"})
	e.must(err)
	ws, err := e.svc.OpenWorkspace(e.ctx, e.claude, e.repo.Name, eff.Slug, "Add limiter")
	e.must(err)
	_, err = e.svc.Land(e.ctx, e.claude, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: e.snapshot(e.repo, ws.Workspace.Base, map[string]*string{"limit.go": s("package shop\n")})})
	if !errors.Is(err, service.ErrNothingToLand) {
		t.Fatalf("land: %v", err)
	}
}
