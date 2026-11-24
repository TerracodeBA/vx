package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/gitbridge/hosttest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// newHostHarness is newHarnessWith(t, true) with a server key and public
// address, as GitLab, Gitea, Forgejo and Bitbucket repositories need.
func newHostHarness(t *testing.T) (*harness, string) {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	blobs, err := blob.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	br := gitbridge.New(st, blobs, t.TempDir(), nil)
	svc := service.New(st, blobs, br)
	svc.GitHub.SecretKey = bytes.Repeat([]byte{7}, 32)
	br.UseHostTokens(svc.HostRepoToken)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc.GitHub.PublicURL = srv.URL
	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateToken(ctx, alice.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, server: srv.URL, home: t.TempDir(), st: st}, tok
}

// postHook delivers a webhook to the vx server as the host would.
func postHook(t *testing.T, url string, headers map[string]string, payload any) (int, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(msg)
}

// The whole loop against a fake GitLab: register a self-managed project
// with its own token, ship an effort (push over HTTPS with the token, open
// the merge request), ship again (update it), then GitLab's webhooks: a
// push to main is imported and the merged merge request marks the effort
// merged. Deliveries without the secret are refused.
func TestShipToFakeGitLab(t *testing.T) {
	up := newGitUpstream(t)
	up.commit("Initial", map[string]string{"README.md": "# shop\n"})
	gl := hosttest.New(t, store.ProviderGitLab, "acme/platform/shop", "Bearer glpat-secret")
	gl.ServeGit(t, up.dir, "oauth2", "glpat-secret")

	h, aliceToken := newHostHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("glpat-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without a token the project cannot even be read.
	if out, code := alice.run("repo", "create", "acme/nope", "--gitlab", "acme/platform/shop", "--gitlab-url", gl.URL); code == 0 || !strings.Contains(out, "--token-file FILE") {
		t.Fatalf("created without a token, or no hint:\n%s", out)
	}
	anonymous := len(gl.GitRequests())
	out := alice.must("repo", "create", "acme/shop", "--gitlab", "acme/platform/shop", "--gitlab-url", gl.URL, "--token-file", tokenFile)
	for _, want := range []string{"following gitlab acme/platform/shop on " + gl.URL, "Register the webhook on GitLab", h.server + "/hooks/gitlab", "Secret token: ", "Push events, Merge request events"} {
		if !strings.Contains(out, want) {
			t.Errorf("repo create output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "glpat-secret") {
		t.Fatalf("repo create printed the token:\n%s", out)
	}
	secret := regexp.MustCompile(`Secret token: ([0-9a-f]+)`).FindStringSubmatch(out)
	if secret == nil {
		t.Fatalf("no webhook secret in:\n%s", out)
	}
	if out := alice.must("repo", "host", "acme/shop"); !strings.Contains(out, "the repository's webhook secret, shown when it was made") || strings.Contains(out, secret[1]) {
		t.Fatalf("repo host without flags:\n%s", out)
	}
	if out := alice.must("repo", "list"); !strings.Contains(out, "gitlab") || !strings.Contains(out, strings.TrimPrefix(gl.URL, "http://")+"/acme/platform/shop") {
		t.Fatalf("repo list:\n%s", out)
	}
	for _, g := range gl.GitRequests()[anonymous:] {
		if !strings.HasSuffix(g, " oauth2:glpat-secret") {
			t.Fatalf("git request without the token: %v", gl.GitRequests())
		}
	}

	alice.must("effort", "new", "Rate limiting", "--repo", "acme/shop", "--goal", "Limit login attempts.")
	alice.must("clone", "acme/shop", "rate-limiting", "shop")
	alice.dir = filepath.Join(work, "shop")
	alice.must("new", "Add limiter")
	alice.write("limit.go", "package shop\n")
	alice.must("land")

	out = alice.must("ship", "open")
	if !strings.Contains(out, "Opened pull request #101: "+gl.PRURL(101)) {
		t.Fatalf("ship open:\n%s", out)
	}
	mr := gl.PR(101)
	if mr == nil || mr.Head != "vx/rate-limiting" || mr.Base != "main" || mr.Title != "Rate limiting" || !strings.Contains(mr.Body, "Limit login attempts.") {
		t.Fatalf("merge request = %+v", mr)
	}
	r, err := git.PlainOpen(up.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reference(plumbing.NewBranchReferenceName("vx/rate-limiting"), true); err != nil {
		t.Fatalf("vx/rate-limiting was not pushed: %v", err)
	}

	alice.must("new", "Wire limiter")
	alice.write("limit.go", "package shop\n\nfunc Limit() {}\n")
	alice.must("land")
	if out := alice.must("ship", "open"); !strings.Contains(out, "Updated pull request #101") {
		t.Fatalf("second ship:\n%s", out)
	}
	if created, updated := gl.Counts(); created != 1 || updated != 1 {
		t.Fatalf("created %d, updated %d", created, updated)
	}

	// GitLab's webhooks.
	project := map[string]any{"path_with_namespace": "acme/platform/shop", "web_url": gl.URL + "/acme/platform/shop"}
	newMain := up.commit("Docs", map[string]string{"README.md": "# shop\n\nDocs.\n"})
	push := map[string]any{"object_kind": "push", "ref": "refs/heads/main", "after": newMain, "project": project}
	hooks := h.server + "/hooks/gitlab"
	if code, msg := postHook(t, hooks, map[string]string{"X-Gitlab-Event": "Push Hook"}, push); code != http.StatusUnauthorized {
		t.Fatalf("missing token: %d %s", code, msg)
	}
	if code, msg := postHook(t, hooks, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "guess"}, push); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d %s", code, msg)
	}
	if code, msg := postHook(t, hooks, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": secret[1], "X-Gitlab-Event-UUID": "e1"}, push); code != http.StatusOK {
		t.Fatalf("push: %d %s", code, msg)
	}
	main, err := h.st.GetBranch(context.Background(), mustRepo(t, h, "acme/shop").ID, "main")
	if err != nil || main.GitSHA != newMain {
		t.Fatalf("main after the push hook = %+v %v, want %s", main, err, newMain)
	}

	// The merge request is squash-merged on GitLab: the change ids are
	// gone, so the merge request event closes the effort.
	gl.SetState(101, "merged")
	up.commit("Rate limiting (!101)", map[string]string{"limit.go": "package shop\n\nfunc Limit() {}\n"})
	merged := map[string]any{"object_kind": "merge_request", "project": project,
		"object_attributes": map[string]any{"iid": 101, "action": "merge", "state": "merged", "source_branch": "vx/rate-limiting", "target_branch": "main"}}
	if code, msg := postHook(t, hooks, map[string]string{"X-Gitlab-Event": "Merge Request Hook", "X-Gitlab-Token": secret[1], "X-Gitlab-Event-UUID": "e2"}, merged); code != http.StatusOK {
		t.Fatalf("merge request hook: %d %s", code, msg)
	}
	if out := alice.must("effort", "list", "--repo", "acme/shop", "--all"); !strings.Contains(out, "rate-limiting") || !strings.Contains(out, "merged") {
		t.Fatalf("effort after the merge:\n%s", out)
	}
}

func mustRepo(t *testing.T, h *harness, name string) store.Repo {
	t.Helper()
	r, err := h.st.RepoByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
