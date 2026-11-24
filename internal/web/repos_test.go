package web_test

import (
	"io"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// The sidebar has one Repositories entry instead of every repository and
// its efforts; /ui/repos lists exactly the repositories the viewer can read.
func TestSidebarRepositoriesEntryAndPage(t *testing.T) {
	e := newWebEnv(t)
	ctx := e.ctx
	for _, name := range []string{"alice/shop", "alice/api"} {
		if _, err := e.svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "alice/shop", Name: "Rate limiting"}); err != nil {
		t.Fatal(err)
	}
	bob, _ := e.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	bobActor, _ := e.svc.ResolveActor(ctx, bob)
	if _, err := e.svc.CreateRepo(ctx, bobActor, service.CreateRepoInput{Name: "bob/secret"}); err != nil {
		t.Fatal(err)
	}
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

	// Settings has no repository content of its own: whatever names a
	// repository there comes from the sidebar.
	page := get("/ui/settings")
	if !strings.Contains(page, `href="/ui/repos"`) || !strings.Contains(page, ">Repositories</a>") {
		t.Fatalf("sidebar lacks the Repositories entry:\n%s", page)
	}
	for _, name := range []string{"alice/shop", "alice/api", "Rate limiting"} {
		if strings.Contains(page, name) {
			t.Errorf("sidebar still lists %q", name)
		}
	}

	page = get("/ui/repos")
	for _, want := range []string{"alice/shop", "alice/api", `data-filter="repo-table"`, `id="repo-table"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/ui/repos lacks %q", want)
		}
	}
	if strings.Contains(page, "bob/secret") {
		t.Error("/ui/repos shows a repository the viewer cannot read")
	}
	if strings.Contains(page, "<script>") || strings.Contains(page, "style=") {
		t.Error("/ui/repos has inline script or style")
	}
	// The entry is marked current on the page and inside a repository.
	if !strings.Contains(get("/ui/repo?name=alice%2Fshop"), `class="side-link on" href="/ui/repos"`) {
		t.Error("Repositories entry not marked current inside a repository")
	}
}
