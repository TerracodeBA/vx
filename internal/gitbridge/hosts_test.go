package gitbridge_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/gitbridge/hosttest"
	"github.com/TerracodeBA/vx/internal/provider"
	"github.com/TerracodeBA/vx/internal/store"
)

func hostedRepo(p store.Provider, remote, base string) store.Repo {
	return store.Repo{ID: uuid.New(), Name: "acme/shop", Provider: p, Remote: remote, BaseURL: base, DefaultBranch: "main"}
}

func repoToken(tok string) func(context.Context, store.Repo) (string, error) {
	return func(context.Context, store.Repo) (string, error) { return tok, nil }
}

// Each host: open, find the open one and update it (title, body, base:
// a stacked PR retargeted onto another vx/ branch), read its state.
func TestHostPullRequests(t *testing.T) {
	ctx := context.Background()
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:app-pass"))
	for _, c := range []struct {
		name   string
		kind   store.Provider
		remote string
		token  string
		auth   string
		draft  func(*hosttest.PR) bool
	}{
		{"gitlab", store.ProviderGitLab, "acme/platform/shop", "gl-token", "Bearer gl-token", func(p *hosttest.PR) bool { return strings.HasPrefix(p.Title, "Draft: ") }},
		{"gitea", store.ProviderGitea, "acme/shop", "gt-token", "token gt-token", func(p *hosttest.PR) bool { return strings.HasPrefix(p.Title, "WIP: ") }},
		{"forgejo", store.ProviderForgejo, "acme/shop", "fj-token", "token fj-token", func(p *hosttest.PR) bool { return strings.HasPrefix(p.Title, "WIP: ") }},
		{"bitbucket access token", store.ProviderBitbucket, "acme/shop", "bb-token", "Bearer bb-token", func(p *hosttest.PR) bool { return p.Draft }},
		{"bitbucket app password", store.ProviderBitbucket, "acme/shop", "alice:app-pass", basic, func(p *hosttest.PR) bool { return p.Draft }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := hosttest.New(t, c.kind, c.remote, c.auth)
			br := gitbridge.New(nil, blob.NewMem(), t.TempDir(), nil)
			br.UseHostTokens(repoToken(c.token))
			repo := hostedRepo(c.kind, c.remote, f.URL)

			// A PR from a fork with the same branch name is not vx's.
			if c.kind != store.ProviderGitLab {
				f.Add(hosttest.PR{Number: 7, Head: "vx/limits", Base: "main", State: "open", Fork: true})
			}
			opened, err := br.OpenPullRequest(ctx, repo, provider.PullRequest{Head: "vx/limits", Base: "main", Title: "Limits", Body: "one", Draft: true})
			if err != nil || opened.Updated || opened.Number == 0 || opened.URL != f.PRURL(opened.Number) {
				t.Fatalf("open: %+v %v", opened, err)
			}
			p := f.PR(opened.Number)
			if p.Head != "vx/limits" || p.Base != "main" || p.Body != "one" || !c.draft(p) {
				t.Fatalf("created %+v", p)
			}
			// Shipping again updates the same one; a stacked part
			// targets another vx/ branch.
			again, err := br.OpenPullRequest(ctx, repo, provider.PullRequest{Head: "vx/limits", Base: "vx/limits-part-1", Title: "Limits", Body: "two"})
			if err != nil || !again.Updated || again.Number != opened.Number {
				t.Fatalf("update: %+v %v", again, err)
			}
			p = f.PR(opened.Number)
			if created, updated := f.Counts(); p.Base != "vx/limits-part-1" || p.Body != "two" || created != 1 || updated != 1 {
				t.Fatalf("after update %+v created %d updated %d", p, created, updated)
			}
			found, err := br.FindPullRequest(ctx, repo, "vx/limits")
			if err != nil || found.Number != opened.Number || found.State != provider.PRStateOpen || found.Base != "vx/limits-part-1" {
				t.Fatalf("find: %+v %v", found, err)
			}
			if _, err := br.FindPullRequest(ctx, repo, "vx/other"); !errors.Is(err, provider.ErrNotFound) {
				t.Fatalf("find missing: %v", err)
			}
			for state, want := range map[string]provider.PRState{"merged": provider.PRStateMerged, "closed": provider.PRStateClosed, "open": provider.PRStateOpen} {
				f.SetState(opened.Number, state)
				got, err := br.PullRequest(ctx, repo, opened.Number)
				if err != nil || got.State != want || got.Head != "vx/limits" {
					t.Fatalf("state %s: %+v %v", state, got, err)
				}
			}
			if _, err := br.PullRequest(ctx, repo, 9999); !errors.Is(err, provider.ErrNotFound) {
				t.Fatalf("missing PR: %v", err)
			}
			// vx never opens a PR from a branch outside vx/.
			if _, err := br.OpenPullRequest(ctx, repo, provider.PullRequest{Head: "main", Base: "vx/x", Title: "no"}); err == nil {
				t.Fatal("opened a PR from main")
			}
			// A refused token says what to check, without the token.
			br.UseHostTokens(repoToken("wrong-secret-token"))
			_, err = br.OpenPullRequest(ctx, repo, provider.PullRequest{Head: "vx/y", Base: "main", Title: "y"})
			if err == nil || !strings.Contains(err.Error(), "token was refused") || strings.Contains(err.Error(), "wrong-secret-token") {
				t.Fatalf("bad token: %v", err)
			}
		})
	}
}

// Without a token vx hands back the host's page for opening the PR.
func TestHostPullRequestWithoutToken(t *testing.T) {
	ctx := context.Background()
	br := gitbridge.New(nil, blob.NewMem(), t.TempDir(), nil)
	pr := provider.PullRequest{Head: "vx/limits", Base: "main", Title: "Limits"}
	for _, c := range []struct {
		repo store.Repo
		want string
	}{
		{hostedRepo(store.ProviderGitLab, "acme/shop", ""), "https://gitlab.com/acme/shop/-/merge_requests/new?merge_request%5Bsource_branch%5D=vx%2Flimits&merge_request%5Btarget_branch%5D=main"},
		{hostedRepo(store.ProviderGitea, "acme/shop", "https://gitea.example.com"), "https://gitea.example.com/acme/shop/compare/main...vx/limits"},
		{hostedRepo(store.ProviderBitbucket, "acme/shop", ""), "https://bitbucket.org/acme/shop/pull-requests/new?dest=main&source=vx%2Flimits"},
	} {
		got, err := br.OpenPullRequest(ctx, c.repo, pr)
		if !errors.Is(err, provider.ErrNoPullRequests) || got.URL != c.want {
			t.Errorf("%s: %+v %v, want %s", c.repo.Provider, got, err, c.want)
		}
	}
}

// The server's token for a host goes only to repositories on that host: a
// repository naming another address needs its own token.
func TestHostServerTokenStaysOnItsHost(t *testing.T) {
	ctx := context.Background()
	f := hosttest.New(t, store.ProviderGitLab, "acme/platform/shop", "Bearer server-token")
	br := gitbridge.New(nil, blob.NewMem(), t.TempDir(), nil)
	br.SetHost(store.ProviderGitLab, gitbridge.HostConfig{URL: f.URL, Token: func() string { return "server-token" }})
	pr := provider.PullRequest{Head: "vx/limits", Base: "main", Title: "Limits"}
	if _, err := br.OpenPullRequest(ctx, hostedRepo(store.ProviderGitLab, "acme/platform/shop", f.URL), pr); err != nil {
		t.Fatalf("on the server's host: %v", err)
	}
	n := len(f.Requests())
	// Same fake under another address: no token goes there, so no PR.
	other := strings.Replace(f.URL, "127.0.0.1", "localhost", 1)
	if _, err := br.OpenPullRequest(ctx, hostedRepo(store.ProviderGitLab, "acme/platform/shop", other), pr); !errors.Is(err, provider.ErrNoPullRequests) {
		t.Fatalf("another host got the server's token: %v", err)
	}
	if len(f.Requests()) != n {
		t.Fatalf("asked the other host: %v", f.Requests()[n:])
	}
}

// git over HTTPS carries the token as each host expects.
func TestHostGitAuth(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		kind       store.Provider
		token      string
		user, pass string
	}{
		{store.ProviderGitLab, "gl-token", "oauth2", "gl-token"},
		{store.ProviderGitea, "gt-token", "oauth2", "gt-token"},
		{store.ProviderForgejo, "fj-token", "oauth2", "fj-token"},
		{store.ProviderBitbucket, "bb-token", "x-token-auth", "bb-token"},
		{store.ProviderBitbucket, "alice:app-pass", "alice", "app-pass"},
	} {
		var mu sync.Mutex
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			u, p, _ := r.BasicAuth()
			seen = append(seen, r.URL.Path+" "+u+":"+p)
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
		}))
		br := gitbridge.New(nil, blob.NewMem(), t.TempDir(), nil)
		br.UseHostTokens(repoToken(c.token))
		_, err := br.DefaultBranch(ctx, hostedRepo(c.kind, "acme/shop", srv.URL))
		srv.Close()
		if err == nil {
			t.Fatalf("%s: the fake refuses everything", c.kind)
		}
		if strings.Contains(err.Error(), c.pass) {
			t.Fatalf("%s: the error shows the token: %v", c.kind, err)
		}
		want := "/acme/shop.git/info/refs " + c.user + ":" + c.pass
		if len(seen) == 0 || seen[0] != want {
			t.Fatalf("%s: git asked %v, want %q", c.kind, seen, want)
		}
	}
}
