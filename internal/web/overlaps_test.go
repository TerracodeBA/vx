package web_test

import (
	"io"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// The effort page shows "Overlaps with other efforts" and the efforts
// lists a badge, from the stored forecast.
func TestEffortPageOverlaps(t *testing.T) {
	e := newWebEnv(t)
	ctx := e.ctx
	repo, err := e.svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/shop"})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := e.svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Rate limiting"})
	two, _ := e.svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Auth <b>v2</b>"})
	land := func(slug, content string) {
		t.Helper()
		ws, err := e.svc.OpenWorkspace(ctx, e.alice, "acme/shop", slug, "Change "+slug)
		if err != nil {
			t.Fatal(err)
		}
		row, err := e.st.GetChangeset(ctx, repo.ID, ws.Workspace.Base)
		if err != nil {
			t.Fatal(err)
		}
		h, _ := e.svc.Blobs.Put(ctx, hash.KindBlob, []byte(content))
		tree, err := object.ApplyChanges(ctx, e.svc.Blobs, row.Tree, []object.Change{{Path: "x.go", Op: object.OpModify, New: object.File{Hash: h, Mode: object.ModeFile}}})
		if err != nil {
			t.Fatal(err)
		}
		storetest.AddTree(t, e.st, e.svc.Blobs, repo.ID, row.Tree, tree)
		if res, err := e.svc.Land(ctx, e.alice, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree}); err != nil || !res.Landed {
			t.Fatalf("land: %+v %v", res, err)
		}
	}
	land(one.Slug, "package pkg // one\n")
	land(two.Slug, "package pkg // two\n")
	e.svc.WaitForecasts()

	token, _ := e.st.CreateToken(ctx, e.alice.ID, "web")
	e.signIn(token)
	get := func(path string) string {
		t.Helper()
		resp, err := e.c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d\n%s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	body := get("/ui/effort?repo=acme%2Fshop&slug=" + one.Slug)
	i := strings.Index(body, "Overlaps with other efforts")
	if i < 0 {
		t.Fatalf("no overlaps panel:\n%s", body)
	}
	panel := body[i:]
	for _, want := range []string{`id="overlaps"`, "conflict", "Auth &lt;b&gt;v2&lt;/b&gt;", "Conflicts in <span class=\"mono\">x.go</span>"} {
		if !strings.Contains(body, want) {
			t.Errorf("effort page lacks %q:\n%s", want, panel)
		}
	}
	for _, path := range []string{"/ui", "/ui/repo?name=acme%2Fshop"} {
		if b := get(path); !strings.Contains(b, `class="badge badge-conflict"`) || !strings.Contains(b, "conflicts with 1") {
			t.Errorf("%s lacks the overlap badge", path)
		}
	}
}
