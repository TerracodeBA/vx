package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// The repository page shows vx.toml and open reservations; the effort and
// change pages show required checks vx did not run.
func TestRulesOnRepoAndChangePages(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	blobs := blob.NewMem()
	svc := service.New(st, blobs, nil)
	alice, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	ag, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/backend", ParentID: &alice.ID})
	human, _ := svc.ResolveActor(ctx, alice)
	agent, _ := svc.ResolveActor(ctx, ag)
	token, _ := st.CreateToken(ctx, alice.ID, "web")
	repo, err := svc.CreateRepo(ctx, human, service.CreateRepoInput{Name: "acme/shop"})
	if err != nil {
		t.Fatal(err)
	}
	// main gets a vx.toml and one migration.
	put := func(s string) object.File {
		h, _ := blobs.Put(ctx, hash.KindBlob, []byte(s))
		return object.File{Hash: h, Mode: object.ModeFile}
	}
	main, err := st.GetBranch(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := st.GetChangeset(ctx, repo.ID, main.Head)
	tree, err := object.ApplyChanges(ctx, blobs, base.Tree, []object.Change{
		{Path: "vx.toml", Op: object.OpAdd, New: put("[[unique]]\nname = \"migration\"\nglob = \"db/*.sql\"\nkey = '^(\\d+)_'\n\n[[generated]]\nglob = \"gen/**\"\ncommand = \"buf generate\"\n\n[checks]\nrequired = [\"go vet ./...\"]\n")},
		{Path: "db/004_x.sql", Op: object.OpAdd, New: put("x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	storetest.AddTree(t, st, blobs, repo.ID, base.Tree, tree)
	h, err := st.InsertChangeset(ctx, repo.ID, nil, &object.Changeset{
		Version: object.CurrentVersion, ChangeID: object.NewChangeID(), Parents: []hash.Hash{main.Head}, Tree: tree,
		Origin: object.OriginUpstream, Author: object.Author{Name: "Bob"}, Intent: "Rules", CreatedAt: svc.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBranch(ctx, store.Branch{RepoID: repo.ID, Name: "main", Head: h}); err != nil {
		t.Fatal(err)
	}
	eff, _ := svc.CreateEffort(ctx, human, service.CreateEffortInput{Repo: "acme/shop", Name: "Login audit"})
	ws, _ := svc.OpenWorkspace(ctx, agent, "acme/shop", eff.Slug, "Audit table")
	wsBase, _ := st.GetChangeset(ctx, repo.ID, ws.Workspace.Base)
	next, _ := object.ApplyChanges(ctx, blobs, wsBase.Tree, []object.Change{{Path: "a.go", Op: object.OpAdd, New: put("package a\n")}})
	storetest.AddTree(t, st, blobs, repo.ID, wsBase.Tree, next)
	res, err := svc.Land(ctx, agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: next})
	if err != nil || !res.Landed {
		t.Fatalf("land: %+v %v", res, err)
	}
	if v, _, err := svc.Reserve(ctx, agent, "acme/shop", eff.Slug, "migration"); err != nil || v != "005" {
		t.Fatalf("reserve: %q %v", v, err)
	}

	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if _, err := postForm(c, srv.URL, "/ui/login", url.Values{"token": {token}, "next": {"/ui"}}); err != nil {
		t.Fatal(err)
	}
	get := func(path string) string {
		t.Helper()
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d\n%s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	page := get("/ui/repo?name=acme%2Fshop")
	for _, want := range []string{"Rules (vx.toml)", "vx.toml at main " + h.Short(), "db/*.sql", "key ^(\\d&#43;)_", "gen/**", "buf generate", "go vet ./...", "Open reservations", "migration 005", eff.Slug} {
		if !strings.Contains(page, want) {
			t.Errorf("repo page lacks %q", want)
		}
	}
	page = get("/ui/change?repo=acme%2Fshop&ref=" + res.Changeset.ChangeID)
	if !strings.Contains(page, `<span class="pill warn">not verified</span><span class="mono small">go vet ./...</span>`) {
		t.Errorf("change page lacks the not verified check")
	}
	if page := get("/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug); !strings.Contains(page, "go vet ./... · not verified") {
		t.Errorf("effort page lacks the not verified check")
	}
}
