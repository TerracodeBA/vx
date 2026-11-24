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

func TestPages(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	blobs := blob.NewMem()
	svc := service.New(st, blobs, nil)
	alice, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	ag, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/backend", ParentID: &alice.ID})
	human, _ := svc.ResolveActor(ctx, alice)
	agent, _ := svc.ResolveActor(ctx, ag)
	token, _ := st.CreateToken(ctx, alice.ID, "web")

	repo, err := svc.CreateRepo(ctx, human, service.CreateRepoInput{Name: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	eff, _ := svc.CreateEffort(ctx, human, service.CreateEffortInput{Repo: "shop", Name: "Rate limiting", Goal: "Limit <logins>"})
	ws, _ := svc.OpenWorkspace(ctx, agent, "shop", eff.Slug, "Add limiter")
	row, _ := st.GetChangeset(ctx, repo.ID, ws.Workspace.Base)
	h, _ := blobs.Put(ctx, hash.KindBlob, []byte("package limit\n\nfunc Allow() bool { return true }\n"))
	tree, _ := object.ApplyChanges(ctx, blobs, row.Tree, []object.Change{{Path: "limit/limit.go", Op: object.OpAdd, New: object.File{Hash: h, Mode: object.ModeFile}}})
	res, err := svc.Land(ctx, agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree, Model: "claude-opus-5-5",
		Evidence: &object.Evidence{Checks: []object.Check{{Name: "go test ./...", Result: "pass", RanByVX: true, DurationMs: 1200}}}})
	if err != nil || !res.Landed {
		t.Fatal(err)
	}
	svc.OpenWorkspace(ctx, agent, "shop", eff.Slug, "Wire limiter into login")

	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Signed out: redirected to the login page.
	if code, body := get("/ui"); code != 200 || !strings.Contains(body, "Sign in to vx") {
		t.Fatalf("login redirect = %d", code)
	}
	resp, err := c.PostForm(srv.URL+"/ui/login", url.Values{"token": {"vxt_wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "not valid") {
		t.Fatal("bad token should be refused")
	}
	if _, err := c.PostForm(srv.URL+"/ui/login", url.Values{"token": {token}, "next": {"/ui"}}); err != nil {
		t.Fatal(err)
	}
	for path, wants := range map[string][]string{
		"/ui":                                   {"Rate limiting", "1 change", "1 working now"},
		"/ui/repo?name=shop":                    {"Branches vx follows", "main"},
		"/ui/effort?repo=shop&slug=" + eff.Slug: {"Add limiter", "alice/backend", "run by vx", "Wire limiter into login", "Limit &lt;logins&gt;", "claude-opus-5-5"},
		"/ui/change?repo=shop&ref=" + res.Changeset.ChangeID: {"limit/limit.go", "func Allow() bool", "run by vx"},
		"/ui/ship?repo=shop&slug=" + eff.Slug:                {"has no git upstream"},
	} {
		code, body := get(path)
		if code != 200 {
			t.Fatalf("%s: status %d\n%s", path, code, body)
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", path, w)
			}
		}
		if strings.Contains(body, "<logins>") {
			t.Errorf("%s: unescaped user content", path)
		}
	}
	if code, body := get("/ui/effort/head?repo=shop&slug=" + eff.Slug); code != 200 || !strings.HasPrefix(body, res.Changeset.Hash.String()) {
		t.Fatalf("head = %d %s", code, body)
	}
	if code, _ := get("/ui/effort?repo=shop&slug=nope"); code != 404 {
		t.Fatalf("missing effort = %d", code)
	}
	if code, body := get("/ui/static/vx.css"); code != 200 || !strings.Contains(body, "--accent") {
		t.Fatal("css not served")
	}
}
