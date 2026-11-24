package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

func (e *env) repo(actor service.Actor, name string) store.Repo {
	e.t.Helper()
	r, err := e.svc.CreateRepo(e.ctx, actor, service.CreateRepoInput{Name: name})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func codeOf(err error) service.Code {
	var se *service.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// sameShape compares two error messages with the hashes they name taken out.
func sameShape(a error, ha hash.Hash, b error, hb hash.Hash) bool {
	clean := func(err error, h hash.Hash) string {
		return strings.ReplaceAll(strings.ReplaceAll(err.Error(), h.String(), "H"), h.Short(), "H")
	}
	return clean(a, ha) == clean(b, hb)
}

func blobObj(data string) service.Object {
	return service.Object{Kind: hash.KindBlob, Data: []byte(data)}
}

// An object in the store is served only through a repository it is part
// of: bob cannot fetch alice's blob through his repository by its hash, and
// MissingObjects does not reveal that the server has it. Uploading the same
// content into his repository works and makes it fetchable there.
func TestObjectsScopedToRepositories(t *testing.T) {
	e := newEnv(t)
	bob := e.person("bob")
	e.repo(e.alice, "shop-api")
	e.repo(bob, "notes")

	secret := "API_KEY=alice-only\n"
	got, err := e.svc.PutObjects(e.ctx, e.alice, "shop-api", []service.Object{blobObj(secret)})
	if err != nil {
		t.Fatal(err)
	}
	h := got[0]
	never := hash.Of(hash.KindBlob, []byte("never stored anywhere"))
	if ok, _ := e.blobs.Has(e.ctx, h); !ok {
		t.Fatal("the blob should be in the shared store")
	}

	// Fetching through his own repository: not found, exactly as for a
	// hash nobody ever stored.
	_, errSecret := e.svc.GetObjects(e.ctx, bob, "notes", []hash.Hash{h}, 0)
	_, errNever := e.svc.GetObjects(e.ctx, bob, "notes", []hash.Hash{never}, 0)
	if codeOf(errSecret) != service.CodeNotFound || codeOf(errNever) != service.CodeNotFound {
		t.Fatalf("get errors = %v / %v, want not found", errSecret, errNever)
	}
	if !sameShape(errSecret, h, errNever, never) {
		t.Fatalf("errors differ, which leaks existence:\n  %v\n  %v", errSecret, errNever)
	}
	// MissingObjects answers for his repository only.
	missing, err := e.svc.MissingObjects(e.ctx, bob, "notes", []hash.Hash{h, never})
	if err != nil || len(missing) != 2 {
		t.Fatalf("missing = %v %v, want both", missing, err)
	}
	// Claiming the hash without the bytes does not make it his.
	_, err = e.svc.PutObjects(e.ctx, bob, "notes", []service.Object{{Kind: hash.KindBlob, Data: []byte("guess"), Hash: h}})
	if codeOf(err) != service.CodeInvalid {
		t.Fatalf("put with wrong bytes = %v, want invalid", err)
	}
	if missing, _ := e.svc.MissingObjects(e.ctx, bob, "notes", []hash.Hash{h}); len(missing) != 1 {
		t.Fatal("a failed upload must not make the object part of the repo")
	}

	// The same content uploaded by bob into his repository works, and is
	// then fetchable through it.
	got, err = e.svc.PutObjects(e.ctx, bob, "notes", []service.Object{{Kind: hash.KindBlob, Data: []byte(secret), Hash: h}})
	if err != nil || got[0] != h {
		t.Fatalf("put = %v %v", got, err)
	}
	objs, err := e.svc.GetObjects(e.ctx, bob, "notes", []hash.Hash{h}, 0)
	if err != nil || len(objs) != 1 || string(objs[0].Data) != secret {
		t.Fatalf("get after upload = %v %v", objs, err)
	}
	if missing, _ := e.svc.MissingObjects(e.ctx, bob, "notes", []hash.Hash{h}); len(missing) != 0 {
		t.Fatalf("missing after upload = %v", missing)
	}
	// Bob cannot read alice's repository, so asking through it reveals
	// nothing either: the repository itself is not found.
	if _, err := e.svc.GetObjects(e.ctx, bob, "shop-api", []hash.Hash{h}, 0); codeOf(err) != service.CodeNotFound {
		t.Fatalf("bob get through alice's repo = %v, want not found", err)
	}
	if _, err := e.svc.MissingObjects(e.ctx, bob, "shop-api", []hash.Hash{h}); codeOf(err) != service.CodeNotFound {
		t.Fatalf("bob missing through alice's repo = %v, want not found", err)
	}
	if _, err := e.svc.PutObjects(e.ctx, bob, "shop-api", []service.Object{blobObj("planted")}); codeOf(err) != service.CodeNotFound {
		t.Fatalf("bob put into alice's repo = %v, want not found", err)
	}
	// With read access he can fetch through it, but not upload into it.
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, "shop-api", "bob", service.RoleRead); err != nil {
		t.Fatal(err)
	}
	if objs, err := e.svc.GetObjects(e.ctx, bob, "shop-api", []hash.Hash{h}, 0); err != nil || len(objs) != 1 {
		t.Fatalf("reader get = %v %v", objs, err)
	}
	if _, err := e.svc.PutObjects(e.ctx, bob, "shop-api", []service.Object{blobObj("planted")}); codeOf(err) != service.CodePermission {
		t.Fatalf("reader put = %v, want permission denied", err)
	}
	// Alice's repository was never affected.
	if missing, _ := e.svc.MissingObjects(e.ctx, e.alice, "shop-api", []hash.Hash{h}); len(missing) != 0 {
		t.Fatalf("alice missing = %v", missing)
	}
}

// Requests without a repository come from clients older than v0.2 and get
// told to update.
func TestObjectRequestsWithoutRepoAreRefused(t *testing.T) {
	e := newEnv(t)
	e.repo(e.alice, "shop-api")
	h := hash.Of(hash.KindBlob, []byte("x"))
	_, err1 := e.svc.MissingObjects(e.ctx, e.alice, "", []hash.Hash{h})
	_, err2 := e.svc.GetObjects(e.ctx, e.alice, "", []hash.Hash{h}, 0)
	_, err3 := e.svc.PutObjects(e.ctx, e.alice, " ", []service.Object{blobObj("x")})
	for _, err := range []error{err1, err2, err3} {
		if codeOf(err) != service.CodeFailedPrecondition || !strings.Contains(err.Error(), "this vx is too old for the server: update vx") {
			t.Fatalf("err = %v, want the update message", err)
		}
	}
	if ok, _ := e.blobs.Has(e.ctx, h); ok {
		t.Fatal("a refused upload must not store anything")
	}
}

// A snapshot that points at another repository's blobs or trees, without
// uploading their bytes to this repository, cannot be landed, and the error
// for another repository's tree is the same as for a tree that does not
// exist.
func TestLandRefusesObjectsOfOtherRepositories(t *testing.T) {
	e := newEnv(t)
	bob := e.person("bob")
	alice := e.repo(e.alice, "shop-api")
	e.repo(bob, "notes")
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "shop-api", Name: "Config"})
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := e.svc.OpenWorkspace(e.ctx, e.agent, "shop-api", eff.Slug, "Add config")
	aliceTree := e.snapshot(alice, ws.Workspace.Base, map[string]*string{"config/secret.env": s("API_KEY=alice-only\n")})
	if res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: aliceTree}); err != nil || !res.Landed {
		t.Fatalf("alice land: %+v %v", res, err)
	}
	secretBlob, _, _ := object.Lookup(e.ctx, e.blobs, aliceTree, "config/secret.env")
	configTree, _, _ := object.Lookup(e.ctx, e.blobs, aliceTree, "config")

	bobEff, err := e.svc.CreateEffort(e.ctx, bob, service.CreateEffortInput{Repo: "notes", Name: "Notes"})
	if err != nil {
		t.Fatal(err)
	}
	land := func(entries ...object.Entry) error {
		t.Helper()
		w, err := e.svc.OpenWorkspace(e.ctx, bob, "notes", bobEff.Slug, "Borrow")
		if err != nil {
			t.Fatal(err)
		}
		tree := &object.Tree{Entries: entries}
		data, _ := tree.Encode()
		// Bob uploads his own root tree; it only names the other objects.
		if _, err := e.svc.PutObjects(e.ctx, bob, "notes", []service.Object{{Kind: hash.KindTree, Data: data}}); err != nil {
			t.Fatal(err)
		}
		_, err = e.svc.Land(e.ctx, bob, service.LandInput{WorkspaceID: w.Workspace.ID, Tree: hash.Of(hash.KindTree, data)})
		e.svc.Abandon(e.ctx, bob, w.Workspace.ID)
		return err
	}
	err = land(object.Entry{Name: "leak.env", Mode: object.ModeFile, Hash: secretBlob.Hash})
	if codeOf(err) != service.CodeFailedPrecondition || !strings.Contains(err.Error(), "file contents missing on the server: leak.env") {
		t.Fatalf("land with alice's blob = %v", err)
	}
	errTree := land(object.Entry{Name: "stolen", Mode: object.ModeDir, Hash: configTree.Hash})
	nowhere := hash.Of(hash.KindTree, []byte("no such tree"))
	errNone := land(object.Entry{Name: "stolen", Mode: object.ModeDir, Hash: nowhere})
	if codeOf(errTree) != service.CodeFailedPrecondition || codeOf(errNone) != service.CodeFailedPrecondition {
		t.Fatalf("land with alice's tree = %v / %v", errTree, errNone)
	}
	if !sameShape(errTree, configTree.Hash, errNone, nowhere) {
		t.Fatalf("errors differ, which leaks existence:\n  %v\n  %v", errTree, errNone)
	}
	if _, err := e.svc.GetObjects(e.ctx, bob, "notes", []hash.Hash{secretBlob.Hash}, 0); codeOf(err) != service.CodeNotFound {
		t.Fatalf("bob fetch after refused lands = %v", err)
	}
}

// allObjects lists every tree and blob under tree.
func (e *env) allObjects(tree hash.Hash) []hash.Hash {
	e.t.Helper()
	trees, blobs, err := object.NewObjects(e.ctx, e.blobs, hash.Zero, tree)
	if err != nil {
		e.t.Fatal(err)
	}
	return append(trees, blobs...)
}

// A land merged onto newer work makes new trees and blobs on the server;
// they are part of the repository, so clients can fetch the result.
func TestMergedLandIsFetchable(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Login")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Logout")
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{"auth.go": s("func Login(ctx) {\n}\n\nfunc Logout() {\n}\n")})
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{"auth.go": s("func Login() {\n}\n\nfunc Logout() {\n\taudit()\n}\n")})
	if _, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1}); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil || !res.Landed || !res.Rebased {
		t.Fatalf("second land: %+v %v", res, err)
	}
	all := e.allObjects(res.Changeset.Tree)
	objs, err := e.svc.GetObjects(e.ctx, e.agent, repo.Name, all, 0)
	if err != nil || len(objs) != len(all) {
		t.Fatalf("fetch merged tree: %d of %d, %v", len(objs), len(all), err)
	}
}

// Repositories from before repo_objects are indexed from their changesets:
// on first use, by the startup backfill (which also finds repositories that
// were never queued), and by hand. Running it again changes nothing.
func TestObjectBackfill(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	ctx := e.ctx
	log, err := e.svc.Log(ctx, e.alice, repo.Name, eff.Slug, 1, false)
	if err != nil || len(log) == 0 {
		t.Fatalf("log: %v %v", log, err)
	}
	all := e.allObjects(log[0].Tree)
	want, _ := e.svc.Store.CountRepoObjects(ctx, repo.ID)

	// Data from an old server: no index, repository queued by the migration.
	if err := e.svc.Store.ResetObjectIndex(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.Store.CountRepoObjects(ctx, repo.ID); n != 0 {
		t.Fatalf("reset left %d objects", n)
	}
	// A fresh server process serves the repository by backfilling it first.
	fresh := service.New(e.svc.Store, e.blobs, nil)
	objs, err := fresh.GetObjects(ctx, e.agent, repo.Name, all, 0)
	if err != nil || len(objs) != len(all) {
		t.Fatalf("fetch after reset: %d of %d, %v", len(objs), len(all), err)
	}
	if pending, _ := e.svc.Store.ObjectBackfillPending(ctx, repo.ID); pending {
		t.Fatal("the backfill should have dequeued the repository")
	}
	if n, _ := e.svc.Store.CountRepoObjects(ctx, repo.ID); n < want {
		t.Fatalf("backfill recorded %d objects, the live index had %d", n, want)
	}

	// Not queued at all (a database migrated by hand): the startup scan
	// finds it because it has changesets and no objects.
	if err := e.svc.Store.ResetObjectIndex(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Store.FinishObjectBackfill(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	fresh = service.New(e.svc.Store, e.blobs, nil)
	done, err := fresh.BackfillObjects(ctx, nil)
	if err != nil || len(done) != 1 || done[0].Repo != repo.Name || done[0].Objects == 0 {
		t.Fatalf("backfill = %+v %v", done, err)
	}
	n1, _ := e.svc.Store.CountRepoObjects(ctx, repo.ID)
	if objs, err := fresh.GetObjects(ctx, e.agent, repo.Name, all, 0); err != nil || len(objs) != len(all) {
		t.Fatalf("fetch after backfill: %v", err)
	}
	// Idempotent: nothing left to do, and a forced walk records nothing new.
	if again, err := fresh.BackfillObjects(ctx, nil); err != nil || len(again) != 0 {
		t.Fatalf("second backfill = %+v %v", again, err)
	}
	if _, err := fresh.BackfillRepoObjects(ctx, repo.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	if n2, _ := e.svc.Store.CountRepoObjects(ctx, repo.ID); n2 != n1 {
		t.Fatalf("forced backfill changed the index: %d -> %d", n1, n2)
	}
}

// A database from before migration 010: a repository with a changeset whose
// objects were stored without any membership. After the migration the
// repository is queued, and its first object request backfills it.
func TestRepoObjectsMigrationBackfillsOldRepos(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, storetest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.MigrateTo(ctx, 9); err != nil {
		t.Fatal(err)
	}
	blobs := blob.NewMem()
	old := service.New(st, blobs, nil)
	// The old schema is written with store calls only: today's service
	// reads columns (GitHub links, migration 011) that version 9 lacks.
	// Alice is a site admin, so she reads the repository in "default".
	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice", SiteAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := old.ResolveActor(ctx, alice)
	repo, err := st.CreateRepo(ctx, store.Repo{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := object.EmptyTree(ctx, blobs)
	if err != nil {
		t.Fatal(err)
	}
	rootCS := &object.Changeset{Version: object.CurrentVersion, ChangeID: object.NewChangeID(), Tree: empty,
		Origin: object.OriginUpstream, Author: object.Author{Principal: "user:alice"}, Intent: "Empty repository", CreatedAt: time.Now()}
	rootHash, err := st.InsertChangeset(ctx, repo.ID, nil, rootCS, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBranch(ctx, store.Branch{RepoID: repo.ID, Name: repo.DefaultBranch, Head: rootHash}); err != nil {
		t.Fatal(err)
	}
	root, err := st.GetChangeset(ctx, repo.ID, rootHash)
	if err != nil {
		t.Fatal(err)
	}
	// What an old server stored: the objects, and a changeset naming them.
	fh, _ := blobs.Put(ctx, hash.KindBlob, []byte("package legacy\n"))
	tree, err := object.ApplyChanges(ctx, blobs, root.Tree, []object.Change{{Path: "pkg/legacy.go", Op: object.OpAdd, New: object.File{Hash: fh, Mode: object.ModeFile}}})
	if err != nil {
		t.Fatal(err)
	}
	cs := &object.Changeset{Version: object.CurrentVersion, ChangeID: object.NewChangeID(), Parents: []hash.Hash{root.Hash},
		Tree: tree, Origin: object.OriginVX, Author: object.Author{Principal: "user:alice"}, Intent: "Old work", CreatedAt: time.Now()}
	if _, err := st.InsertChangeset(ctx, repo.ID, nil, cs, nil); err != nil {
		t.Fatal(err)
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if pending, err := st.ObjectBackfillPending(ctx, repo.ID); err != nil || !pending {
		t.Fatalf("migration should queue the old repo: %v %v", pending, err)
	}
	svc := service.New(st, blobs, nil)
	trees, files, _ := object.NewObjects(ctx, blobs, hash.Zero, tree)
	all := append(trees, files...)
	objs, err := svc.GetObjects(ctx, actor, "legacy", all, 0)
	if err != nil || len(objs) != len(all) {
		t.Fatalf("old repo after migration: %d of %d objects, %v", len(objs), len(all), err)
	}
	if missing, _ := svc.MissingObjects(ctx, actor, "legacy", []hash.Hash{fh}); len(missing) != 0 {
		t.Fatalf("missing after backfill = %v", missing)
	}
}
