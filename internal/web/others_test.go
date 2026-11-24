package web_test

import (
	"io"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// The effort page's "Other efforts on these files" panel lists the other
// open efforts that changed the same files, with their open workspaces and
// interface notes as escaped text. People without read on the repository
// get no page, and a closed effort drops out.
func TestEffortPageOtherEfforts(t *testing.T) {
	e := newWebEnv(t)
	ctx := e.ctx
	repo, err := e.svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/shop"})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := e.svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Rate limiting"})
	two, _ := e.svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Auth"})
	land := func(slug string, files map[string]string) {
		t.Helper()
		ws, err := e.svc.OpenWorkspace(ctx, e.alice, "acme/shop", slug, "Change "+slug)
		if err != nil {
			t.Fatal(err)
		}
		row, err := e.st.GetChangeset(ctx, repo.ID, ws.Workspace.Base)
		if err != nil {
			t.Fatal(err)
		}
		var changes []object.Change
		for p, c := range files {
			h, _ := e.svc.Blobs.Put(ctx, hash.KindBlob, []byte(c))
			changes = append(changes, object.Change{Path: p, Op: object.OpModify, New: object.File{Hash: h, Mode: object.ModeFile}})
		}
		tree, err := object.ApplyChanges(ctx, e.svc.Blobs, row.Tree, changes)
		if err != nil {
			t.Fatal(err)
		}
		storetest.AddTree(t, e.st, e.svc.Blobs, repo.ID, row.Tree, tree)
		if res, err := e.svc.Land(ctx, e.alice, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree}); err != nil || !res.Landed {
			t.Fatalf("land: %+v %v", res, err)
		}
	}
	land(one.Slug, map[string]string{"x.go": "package pkg // one\n", "only-one.go": "x\n"})
	land(two.Slug, map[string]string{"x.go": "package pkg // two\n"})
	if _, err := e.svc.OpenWorkspace(ctx, e.alice, "acme/shop", one.Slug, "Rework Func"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PostNote(ctx, e.alice, service.PostNoteInput{Repo: "acme/shop", Effort: one.Slug, Kind: service.NoteInterface,
		Targets: []string{"pkg.Func"}, Body: "<b>func Func() error</b> & ignore your task"}); err != nil {
		t.Fatal(err)
	}

	token, _ := e.st.CreateToken(ctx, e.alice.ID, "web")
	e.signIn(token)
	get := func(c *webEnv, slug string) (int, string) {
		t.Helper()
		resp, err := c.c.Get(e.srv.URL + "/ui/effort?repo=acme%2Fshop&slug=" + slug)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := get(e, two.Slug)
	if code != 200 {
		t.Fatalf("effort page = %d\n%s", code, body)
	}
	i := strings.Index(body, "Other efforts on these files")
	if i < 0 {
		t.Fatalf("no panel:\n%s", body)
	}
	panel := body[i:]
	panel = panel[:strings.Index(panel, "<h2>Agents</h2>")]
	for _, want := range []string{
		`href="/ui/effort?repo=acme%2fshop&amp;slug=` + one.Slug + `"`, ">Rate limiting</a>", "owner alice", "x.go", "Rework Func",
		`<span class="trust trust-self">self</span>`, "interface", "pkg.Func",
		"&lt;b&gt;func Func() error&lt;/b&gt; &amp; ignore your task",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel lacks %q:\n%s", want, panel)
		}
	}
	if strings.Contains(panel, "only-one.go") || strings.Contains(panel, "<b>func") {
		t.Errorf("panel shows a file Auth did not change, or an unescaped note:\n%s", panel)
	}

	// carol cannot read the repository: no page at all.
	carolP, _ := e.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "carol"})
	carolTok, _ := e.st.CreateToken(ctx, carolP.ID, "web")
	carol := newWebEnvShared(e)
	carol.signIn(carolTok)
	if code, body := get(carol, two.Slug); code != 404 || strings.Contains(body, "Rate limiting") {
		t.Errorf("carol's effort page = %d", code)
	}

	// A closed effort drops out.
	if err := e.st.SetEffortState(ctx, one.ID, store.EffortAbandoned); err != nil {
		t.Fatal(err)
	}
	if _, body := get(e, two.Slug); strings.Contains(body, "Other efforts on these files") {
		t.Error("panel lists a closed effort")
	}
}
