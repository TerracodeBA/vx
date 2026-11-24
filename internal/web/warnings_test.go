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

// Two agents add the same func in different files of one package. The
// second land carries a warning, and the web UI shows it to everyone: a
// pill on the effort timeline and a Warnings section on the change page.
func TestWarningsShownOnEffortAndChange(t *testing.T) {
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
	eff, _ := svc.CreateEffort(ctx, human, service.CreateEffortInput{Repo: "acme/shop", Name: "Login audit"})

	fn, _ := blobs.Put(ctx, hash.KindBlob, []byte("package httpapi\n\nfunc clientIP() string { return \"\" }\n"))
	w1, _ := svc.OpenWorkspace(ctx, agent, "acme/shop", eff.Slug, "Rate limit by client IP")
	w2, _ := svc.OpenWorkspace(ctx, agent, "acme/shop", eff.Slug, "Audit log with client IP")
	var landed []service.LandResult
	for _, l := range []struct {
		ws   service.WorkspaceView
		path string
	}{{w1, "internal/httpapi/limit.go"}, {w2, "internal/httpapi/audit.go"}} {
		base, _ := st.GetChangeset(ctx, repo.ID, l.ws.Workspace.Base)
		tree, err := object.ApplyChanges(ctx, blobs, base.Tree, []object.Change{{Path: l.path, Op: object.OpAdd, New: object.File{Hash: fn, Mode: object.ModeFile}}})
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.Land(ctx, agent, service.LandInput{WorkspaceID: l.ws.Workspace.ID, Tree: tree})
		if err != nil || !res.Landed {
			t.Fatalf("land %s: %+v %v", l.path, res, err)
		}
		landed = append(landed, res)
	}
	if len(landed[1].Warnings) != 1 {
		t.Fatalf("second land warnings = %+v", landed[1].Warnings)
	}

	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if _, err := c.PostForm(srv.URL+"/ui/login", url.Values{"token": {token}, "next": {"/ui"}}); err != nil {
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

	const detail = "duplicate func clientIP in internal/httpapi (audit.go, limit.go)"
	page := get("/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug)
	if n := strings.Count(page, `class="pill warn" title="Found by the server`); n != 1 {
		t.Errorf("effort page has %d warning pills, want 1", n)
	}
	if !strings.Contains(page, "warning · "+detail) {
		t.Errorf("effort page does not show the warning %q", detail)
	}

	page = get("/ui/change?repo=acme%2Fshop&ref=" + landed[1].Changeset.ChangeID)
	if !strings.Contains(page, `<h2 class="sec">Warnings</h2>`) || !strings.Contains(page, detail) || !strings.Contains(page, `<span class="pill warn">duplicate</span>`) {
		t.Errorf("change page lacks the Warnings section")
	}
	if page := get("/ui/change?repo=acme%2Fshop&ref=" + landed[0].Changeset.ChangeID); strings.Contains(page, `<h2 class="sec">Warnings</h2>`) {
		t.Errorf("a change without warnings shows a Warnings section")
	}
}
