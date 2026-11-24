package gitbridge_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/provider"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// Git mirrors are a cache: a server that lost its mirrors, or a second
// server that never had them, fetches again and imports and ships as before.
func TestMirrorsAreRebuiltFromUpstream(t *testing.T) {
	ctx := context.Background()
	svc, br, actor, up := setup(t)
	up.commit("Initial", map[string]string{"a.go": "package a\n"})
	repo, err := svc.CreateRepo(ctx, actor, service.CreateRepoInput{Name: "shop", Provider: store.ProviderGit, Remote: up.dir})
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(br.Dir, repo.ID.String()+".git")
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("no mirror after import: %v", err)
	}

	// The data volume lost the mirrors: the next sync fetches again.
	if err := os.RemoveAll(br.Dir); err != nil {
		t.Fatal(err)
	}
	up.commit("Second", map[string]string{"b.go": "package b\n"})
	b, n, err := svc.SyncBranch(ctx, actor, "shop", "main")
	if err != nil || n != 1 {
		t.Fatalf("sync without a mirror = %d, %v", n, err)
	}
	if got := files(t, svc, repo, b); got["b.go"] != "package b\n" || got["a.go"] != "package a\n" {
		t.Fatalf("files = %v", got)
	}

	// A mirror that no longer opens is made again.
	if err := os.WriteFile(filepath.Join(mirror, "config"), []byte("[core\n garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	br.Log = slog.New(slog.NewTextHandler(&logs, nil))
	up.commit("Third", map[string]string{"c.go": "package c\n"})
	if _, n, err := svc.SyncBranch(ctx, actor, "shop", "main"); err != nil || n != 1 {
		t.Fatalf("sync with a broken mirror = %d, %v", n, err)
	}
	if !strings.Contains(logs.String(), "git mirror is unreadable") {
		t.Fatalf("the broken mirror was not noticed:\n%s", logs.String())
	}

	// A second server (its own empty mirror directory, the same database
	// and blobs) syncs and ships without ever having imported the repo.
	replica := gitbridge.New(svc.Store, svc.Blobs, t.TempDir(), nil)
	tip := up.commit("Fourth", map[string]string{"d.go": "package d\n"})
	nb, n, err := replica.SyncBranch(ctx, repo, "main")
	if err != nil || n != 1 || nb.GitSHA != tip.String() {
		t.Fatalf("replica sync = %d, %+v, %v", n, nb, err)
	}
	row, err := svc.Store.GetChangeset(ctx, repo.ID, nb.Head)
	if err != nil {
		t.Fatal(err)
	}
	m, err := object.ReadManifest(ctx, svc.Blobs, row.Tree)
	if err != nil {
		t.Fatal(err)
	}
	eh, _ := svc.Blobs.Put(ctx, hash.KindBlob, []byte("package e\n"))
	m["e.go"] = object.File{Mode: object.ModeFile, Hash: eh}
	tree, err := object.WriteManifest(ctx, svc.Blobs, m)
	if err != nil {
		t.Fatal(err)
	}
	shas, err := replica.ExportBranch(ctx, repo, nb.GitSHA, "vx/replica", []provider.Commit{{
		Tree: tree, Message: "Add e", AuthorName: "Alice", AuthorEmail: "alice@example.com", When: time.Now(),
	}})
	if err != nil || len(shas) != 1 {
		t.Fatalf("export from the replica = %v, %v", shas, err)
	}
	ref, err := up.repo.Reference(plumbing.NewBranchReferenceName("vx/replica"), true)
	if err != nil || ref.Hash().String() != shas[0] {
		t.Fatalf("upstream vx/replica = %v, %v", ref, err)
	}
	c, _ := up.repo.CommitObject(ref.Hash())
	if f, err := c.File("e.go"); err != nil {
		t.Fatalf("pushed commit lacks e.go: %v", err)
	} else if s, _ := f.Contents(); s != "package e\n" {
		t.Fatalf("e.go = %q", s)
	}
}

// Two servers import one branch at the same time: the branch ends at the
// newest commit, never moved back by the slower one.
func TestConcurrentSyncsDoNotMoveBranchBack(t *testing.T) {
	ctx := context.Background()
	svc, _, actor, up := setup(t)
	up.commit("Initial", map[string]string{"a.go": "package a\n"})
	repo, err := svc.CreateRepo(ctx, actor, service.CreateRepoInput{Name: "shop", Provider: store.ProviderGit, Remote: up.dir})
	if err != nil {
		t.Fatal(err)
	}
	replicas := []*gitbridge.Bridge{
		gitbridge.New(svc.Store, svc.Blobs, t.TempDir(), nil),
		gitbridge.New(svc.Store, svc.Blobs, t.TempDir(), nil),
		gitbridge.New(svc.Store, svc.Blobs, t.TempDir(), nil),
	}
	var tip plumbing.Hash
	for round := range 4 {
		tip = up.commit(fmt.Sprintf("Round %d", round), map[string]string{fmt.Sprintf("r%d.go", round): "package r\n"})
		var wg sync.WaitGroup
		errs := make([]error, len(replicas))
		for i, br := range replicas {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[i] = br.SyncBranch(ctx, repo, "main")
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d replica %d: %v", round, i, err)
			}
		}
		b, err := svc.Store.GetBranch(ctx, repo.ID, "main")
		if err != nil || b.GitSHA != tip.String() {
			t.Fatalf("round %d: branch at %s, want %s (%v)", round, b.GitSHA, tip, err)
		}
	}
	b, _ := svc.Store.GetBranch(ctx, repo.ID, "main")
	chain, err := svc.Store.Chain(ctx, repo.ID, b.Head, hash.Zero, 20)
	if err != nil || len(chain) != 5 {
		t.Fatalf("history has %d changes, want 5 (%v)", len(chain), err)
	}
}
