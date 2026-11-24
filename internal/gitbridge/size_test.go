package gitbridge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/store"
)

// RepoSize asks each host's API for the size before an import, in the
// unit the host uses, and says "unknown" when the host does not tell.
func TestRepoSize(t *testing.T) {
	ctx := context.Background()
	answers := map[string]any{
		"/repos/octo/big":                 map[string]any{"size": 2048},                                             // GitHub: KB
		"/api/v4/projects/acme%2Fshop":    map[string]any{"statistics": map[string]any{"repository_size": 5000000}}, // GitLab: bytes
		"/api/v4/projects/acme%2Fnostats": map[string]any{"id": 1},                                                  // no statistics for this token
		"/api/v1/repos/acme/shop":         map[string]any{"size": 300},                                              // Gitea, Forgejo: KB
		"/2.0/repositories/acme/shop":     map[string]any{"size": 123456},                                           // Bitbucket: bytes
	}
	var lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := answers[r.URL.EscapedPath()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		lastQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(a)
	}))
	defer srv.Close()
	br := gitbridge.New(nil, blob.NewMem(), t.TempDir(), nil)
	br.GitHubAPI = srv.URL
	for _, c := range []struct {
		name   string
		repo   store.Repo
		want   int64
		known  bool
		params string
	}{
		{"github", store.Repo{Provider: store.ProviderGitHub, Remote: "octo/big"}, 2048 * 1024, true, ""},
		{"gitlab", hostedRepo(store.ProviderGitLab, "acme/shop", srv.URL), 5000000, true, "statistics=true"},
		{"gitlab without statistics", hostedRepo(store.ProviderGitLab, "acme/nostats", srv.URL), 0, false, "statistics=true"},
		{"gitea", hostedRepo(store.ProviderGitea, "acme/shop", srv.URL), 300 * 1024, true, ""},
		{"forgejo", hostedRepo(store.ProviderForgejo, "acme/shop", srv.URL), 300 * 1024, true, ""},
		{"bitbucket", hostedRepo(store.ProviderBitbucket, "acme/shop", srv.URL), 123456, true, ""},
		{"missing", store.Repo{Provider: store.ProviderGitHub, Remote: "octo/none"}, 0, false, ""},
		{"plain git", store.Repo{Provider: store.ProviderGit, Remote: "https://example.com/x.git"}, 0, false, ""},
	} {
		lastQuery = ""
		got, known, err := br.RepoSize(ctx, c.repo)
		if err != nil || got != c.want || known != c.known {
			t.Errorf("%s: RepoSize = %d, %v, %v; want %d, %v", c.name, got, known, err, c.want, c.known)
		}
		if c.params != "" && lastQuery != c.params {
			t.Errorf("%s: query %q, want %q", c.name, lastQuery, c.params)
		}
	}
}
