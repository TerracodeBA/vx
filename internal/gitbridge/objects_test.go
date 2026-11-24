package gitbridge_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// Imported objects are part of the importing repository, also when another
// repository imported the same content first (the store holds it once), and
// of no other repository.
func TestImportedObjectsArePartOfTheRepo(t *testing.T) {
	ctx := context.Background()
	svc, _, actor, up := setup(t)
	up.commit("Initial", map[string]string{"README.md": "# shop\n", "pkg/a.go": "package pkg\n"})

	all := func(repo string) []hash.Hash {
		t.Helper()
		r, err := svc.Store.RepoByName(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		b, err := svc.Store.GetBranch(ctx, r.ID, "main")
		if err != nil {
			t.Fatal(err)
		}
		row, err := svc.Store.GetChangeset(ctx, r.ID, b.Head)
		if err != nil {
			t.Fatal(err)
		}
		trees, blobs, err := object.NewObjects(ctx, svc.Blobs, hash.Zero, row.Tree)
		if err != nil {
			t.Fatal(err)
		}
		return append(trees, blobs...)
	}
	for _, name := range []string{"shop", "shop-fork"} {
		if _, err := svc.CreateRepo(ctx, actor, service.CreateRepoInput{Name: name, Provider: store.ProviderGit, Remote: up.dir}); err != nil {
			t.Fatal(err)
		}
	}
	up.commit("Edit a", map[string]string{"pkg/a.go": "package pkg // a2\n"})
	if _, _, err := svc.SyncBranch(ctx, actor, "shop", "main"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shop", "shop-fork"} {
		objs := all(name)
		got, err := svc.GetObjects(ctx, actor, name, objs, 0)
		if err != nil || len(got) != len(objs) {
			t.Fatalf("%s: fetched %d of %d imported objects: %v", name, len(got), len(objs), err)
		}
	}
	if _, err := svc.CreateRepo(ctx, actor, service.CreateRepoInput{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.GetObjects(ctx, actor, "other", all("shop")[1:], 0)
	var se *service.Error
	if !errors.As(err, &se) || se.Code != service.CodeNotFound {
		t.Fatalf("other repo fetch = %v, want not found", err)
	}
}
