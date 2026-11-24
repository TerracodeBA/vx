package web_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// stubUpstream answers for a host without reaching it.
type stubUpstream struct{}

func (stubUpstream) DefaultBranch(context.Context, store.Repo) (string, error) { return "main", nil }
func (stubUpstream) SyncBranch(_ context.Context, r store.Repo, b string) (store.Branch, int, error) {
	return store.Branch{RepoID: r.ID, Name: b}, 0, nil
}

// The repository page names the host and links to the repository there;
// /hooks/gitlab refuses a delivery without the secret.
func TestRepoPageShowsHost(t *testing.T) {
	e := newWebEnv(t)
	e.svc.Upstream = stubUpstream{}
	if _, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "alice/shop", Provider: store.ProviderGitLab, Remote: "acme/platform/shop", BaseURL: "https://gitlab.example.com"}); err != nil {
		t.Fatal(err)
	}
	token, _ := e.st.CreateToken(e.ctx, e.alice.ID, "web")
	e.signIn(token)
	resp, err := e.c.Get(e.srv.URL + "/ui/repo?name=alice/shop")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	for _, want := range []string{"GitLab", `href="https://gitlab.example.com/acme/platform/shop"`, "https://gitlab.example.com</span>"} {
		if !strings.Contains(page, want) {
			t.Errorf("repo page lacks %q", want)
		}
	}
	resp, err = http.Post(e.srv.URL+"/hooks/gitlab", "application/json", strings.NewReader(`{"object_kind":"push","project":{"path_with_namespace":"acme/platform/shop"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned GitLab delivery: %d", resp.StatusCode)
	}
}
