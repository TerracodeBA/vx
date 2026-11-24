package gitbridge_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/provider"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// A repository of a GitHub App installation opens pull requests with its
// installation token; without one (and no VX_GITHUB_TOKEN) vx hands back
// a link instead.
func TestPullRequestWithInstallationToken(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	gh := &fakeGitHub{open: map[string]int{}, bodies: map[int]string{}}
	api := httptest.NewServer(gh)
	defer api.Close()
	br := gitbridge.New(st, blob.NewMem(), t.TempDir(), func() string { return "" })
	br.GitHubAPI = api.URL
	repo, err := st.CreateRepo(ctx, store.Repo{Name: "acme/shop", Provider: store.ProviderGitHub, Remote: "acme/shop"})
	if err != nil {
		t.Fatal(err)
	}
	pr := provider.PullRequest{Title: "Rate limiting", Head: "vx/rate-limiting", Base: "main", Body: "body"}
	if _, err := br.OpenPullRequest(ctx, repo, pr); !errors.Is(err, provider.ErrNoPullRequests) {
		t.Fatalf("without a token: %v", err)
	}
	var asked []string
	br.UseRepoTokens(func(ctx context.Context, r store.Repo) (string, error) {
		asked = append(asked, r.Name)
		return "gh-test-token", nil
	})
	opened, err := br.OpenPullRequest(ctx, repo, pr)
	if err != nil || opened.Number == 0 {
		t.Fatalf("with the installation token: %+v %v", opened, err)
	}
	if len(asked) == 0 || asked[0] != "acme/shop" {
		t.Fatalf("token asked for %v", asked)
	}
	// A failing token source falls back to VX_GITHUB_TOKEN.
	br.GitHubToken = func() string { return "gh-test-token" }
	br.UseRepoTokens(func(context.Context, store.Repo) (string, error) { return "", errors.New("GitHub is down") })
	if _, err := br.OpenPullRequest(ctx, repo, provider.PullRequest{Title: "Two", Head: "vx/two", Base: "main"}); err != nil {
		t.Fatalf("fallback: %v", err)
	}
}
