package github_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/github"
	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
)

func TestGitHubClientManifestConversion(t *testing.T) {
	gh := githubtest.New(t)
	ctx := context.Background()
	code := gh.ManifestCode([]byte(`{"name":"vx (app.example.com)","url":"https://app.example.com"}`))
	creds, err := gh.SetupClient(t).CreateAppFromManifest(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if creds.AppID == 0 || creds.Slug != "vx-app-example-com" || creds.PrivateKeyPEM == "" || creds.ClientSecret == "" || creds.WebhookSecret == "" {
		t.Fatalf("credentials incomplete: %+v", creds)
	}
	if creds.HTMLURL != gh.URL+"/apps/vx-app-example-com" {
		t.Fatalf("html url %q", creds.HTMLURL)
	}
	// Codes work once, and the error never repeats the code.
	_, err = gh.SetupClient(t).CreateAppFromManifest(ctx, code)
	if err == nil || strings.Contains(err.Error(), code) {
		t.Fatalf("second conversion: %v", err)
	}
}

func TestGitHubClientInstallationsAndTokens(t *testing.T) {
	gh := githubtest.New(t)
	ctx := context.Background()
	now := time.Now()
	gh.Now = func() time.Time { return now }
	gh.PageSize = 2
	gh.AddOrg("Acme")
	for i := range 5 {
		gh.AddRepo(fmt.Sprintf("Acme/r%d", i), i%2 == 0)
	}
	id := gh.Install("Acme")
	c := gh.Client(t)

	in, err := c.Installation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != id || in.AccountLogin != "Acme" || in.AccountType != "Organization" || in.RepositorySelection != "all" || in.SuspendedAt != nil {
		t.Fatalf("installation %+v", in)
	}

	repos, err := c.InstallationRepos(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 5 || repos[0].FullName != "Acme/r0" || !repos[0].Private || repos[1].Private || repos[4].DefaultBranch != "main" {
		t.Fatalf("repos %+v", repos)
	}
	if n := gh.Calls("GET /installation/repositories"); n != 3 {
		t.Fatalf("pages fetched: %d, want 3", n)
	}

	// The token is cached until shortly before it expires.
	tokenCalls := fmt.Sprintf("POST /app/installations/%d/access_tokens", id)
	if n := gh.Calls(tokenCalls); n != 1 {
		t.Fatalf("token requests %d, want 1", n)
	}
	tok1, exp, err := c.InstallationToken(ctx, id)
	if err != nil || !exp.After(now) {
		t.Fatal(tok1, exp, err)
	}
	if n := gh.Calls(tokenCalls); n != 1 {
		t.Fatalf("token not cached: %d requests", n)
	}
	now = now.Add(56 * time.Minute) // within five minutes of expiry
	tok2, _, err := c.InstallationToken(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if tok2 == tok1 || gh.Calls(tokenCalls) != 2 {
		t.Fatalf("token not refreshed near expiry")
	}

	// A token GitHub stopped accepting is replaced once.
	gh.ExpireTokens()
	if _, err := c.InstallationRepos(ctx, id); err != nil {
		t.Fatalf("after revoked token: %v", err)
	}
	if gh.Calls(tokenCalls) != 3 {
		t.Fatalf("revoked token not replaced")
	}

	// Suspended and deleted installations fail clearly.
	gh.SuspendInstallation(id, true)
	in, err = c.Installation(ctx, id)
	if err != nil || in.SuspendedAt == nil {
		t.Fatalf("suspended: %+v %v", in, err)
	}
	gh.DeleteInstallation(id)
	_, err = c.Installation(ctx, id)
	if !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("deleted installation: %v", err)
	}
}

func TestGitHubClientAppJWT(t *testing.T) {
	gh := githubtest.New(t)
	ctx := context.Background()
	gh.AddUser("bob")
	id := gh.Install("bob")
	// A client whose clock is 20 minutes behind makes expired tokens.
	creds, _ := gh.App()
	c, err := github.New(github.Config{
		APIURL: gh.APIURL, WebURL: gh.URL, AppID: creds.AppID, PrivateKeyPEM: creds.PrivateKeyPEM,
		Now: func() time.Time { return time.Now().Add(-20 * time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Installation(ctx, id)
	if !errors.Is(err, github.ErrUnauthorized) {
		t.Fatalf("stale JWT: %v", err)
	}
	if strings.Contains(err.Error(), "eyJ") {
		t.Fatalf("error leaks the JWT: %v", err)
	}
	// Wrong key.
	other := githubtest.New(t)
	ocreds := other.NewApp()
	c, _ = github.New(github.Config{APIURL: gh.APIURL, AppID: creds.AppID, PrivateKeyPEM: ocreds.PrivateKeyPEM})
	if _, err := c.Installation(ctx, id); !errors.Is(err, github.ErrUnauthorized) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := github.New(github.Config{PrivateKeyPEM: "not a key"}); err == nil {
		t.Fatal("bad key accepted")
	}
	if _, err := github.New(github.Config{APIURL: "ftp://x"}); err == nil {
		t.Fatal("bad base URL accepted")
	}
}

func TestGitHubClientOAuthPKCE(t *testing.T) {
	gh := githubtest.New(t)
	ctx := context.Background()
	gh.AddUser("Alice")
	gh.SetProfile("Alice", "Alice Liddell", "alice@example.org")
	c := gh.Client(t)
	verifier := "a-long-random-verifier-0123456789-abcdefghijklmnop"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirect := "https://vx.example.com/auth/github/callback"

	// Through the browser side: authorize redirects back with a code.
	creds, _ := gh.App()
	gh.SignIn("Alice")
	q := url.Values{"client_id": {creds.ClientID}, "redirect_uri": {redirect}, "state": {"st"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(gh.URL + "/login/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Query().Get("state") != "st" || !strings.HasPrefix(loc.String(), redirect) {
		t.Fatalf("authorize: %d %s", resp.StatusCode, loc)
	}
	code := loc.Query().Get("code")

	if _, err := c.ExchangeOAuthCode(ctx, code, redirect, "wrong-verifier"); err == nil {
		t.Fatal("wrong verifier accepted")
	}
	code = gh.OAuthCode("Alice", redirect, challenge)
	tok, err := c.ExchangeOAuthCode(ctx, code, redirect, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExchangeOAuthCode(ctx, code, redirect, verifier); err == nil {
		t.Fatal("code reused")
	}
	u, err := c.User(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := gh.Account("Alice"); u.ID != a.ID || u.Login != "Alice" || u.Name != "Alice Liddell" || u.Email != "alice@example.org" {
		t.Fatalf("user %+v", u)
	}
	gh.SetProfile("Alice", "", "")
	if u, err := c.User(ctx, tok); err != nil || u.Email != "" {
		t.Fatalf("no email: %+v %v", u, err)
	}
	if _, err := c.User(ctx, "ghu_bogus"); !errors.Is(err, github.ErrUnauthorized) || strings.Contains(err.Error(), "ghu_bogus") {
		t.Fatalf("bad token: %v", err)
	}
}

func TestGitHubClientPermissionsAndMembership(t *testing.T) {
	gh := githubtest.New(t)
	ctx := context.Background()
	gh.AddOrg("acme")
	for _, u := range []string{"ann", "bo", "cy", "di", "ed"} {
		gh.AddUser(u)
	}
	gh.AddRepo("acme/private", true)
	gh.AddRepo("acme/public", false)
	gh.AddRepo("acme/other", true)
	id := gh.Install("acme", "acme/private", "acme/public")
	gh.SetPermission("acme/private", "ann", "maintain")
	gh.SetPermission("acme/private", "bo", "triage")
	gh.SetPermission("acme/other", "cy", "admin")
	gh.SetOrgMember("acme", "di", "admin")
	gh.SetOrgMember("acme", "ann", "member")
	c := gh.Client(t)

	for _, tc := range []struct{ repo, login, want string }{
		{"private", "ann", "maintain"},
		{"private", "bo", "triage"},
		{"private", "cy", "none"},
		{"private", "di", "admin"}, // org owner
		{"public", "ed", "read"},
		{"private", "nobody", "none"}, // unknown user
		{"other", "cy", "none"},       // not in the installation
	} {
		got, err := c.RepoPermission(ctx, id, "acme", tc.repo, tc.login)
		if err != nil || got != tc.want {
			t.Errorf("%s on %s: %q %v, want %q", tc.login, tc.repo, got, err, tc.want)
		}
	}
	for login, want := range map[string]string{"di": "admin", "ann": "member", "ed": "none"} {
		got, err := c.OrgMembership(ctx, id, "acme", login)
		if err != nil || got != want {
			t.Errorf("membership of %s: %q %v, want %q", login, got, err, want)
		}
	}

	// Rate limits say when to try again.
	gh.Fail("GET /repos/*", http.StatusForbidden, 1)
	_, err := c.RepoPermission(ctx, id, "acme", "private", "ann")
	if !errors.Is(err, github.ErrRateLimited) || !strings.Contains(err.Error(), "resets at") {
		t.Fatalf("rate limit: %v", err)
	}
	gh.Fail("GET /orgs/*", http.StatusInternalServerError, 1)
	if _, err := c.OrgMembership(ctx, id, "acme", "ann"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("server error: %v", err)
	}
	// Suspended installations cannot mint tokens.
	c2 := gh.Client(t)
	gh.SuspendInstallation(id, true)
	if _, err := c2.RepoPermission(ctx, id, "acme", "private", "ann"); err == nil {
		t.Fatal("suspended installation answered")
	}
}

func TestGitHubClientTimeout(t *testing.T) {
	block := make(chan struct{})
	hang := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block })
	ts := newServer(t, hang)
	defer close(block)
	c, err := github.New(github.Config{APIURL: ts, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.User(context.Background(), "ghu_secret"); err == nil || strings.Contains(err.Error(), "ghu_secret") {
		t.Fatalf("timeout: %v", err)
	}
}

func newServer(t *testing.T, h http.Handler) string {
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

// A check run vx opens stays queued until it is updated with a result;
// without the checks: write permission GitHub refuses both calls with the
// error the service recognizes.
func TestGitHubClientCheckRunsStartAndUpdate(t *testing.T) {
	gh := githubtest.New(t)
	ctx := context.Background()
	gh.AddOrg("acme")
	gh.AddRepo("acme/shop", true)
	id := gh.Install("acme", "acme/shop")
	c := gh.Client(t)

	runID, err := c.StartCheckRun(ctx, id, "acme", "shop", service.GitHubCheckRun{
		Name: "vx: e2e", HeadSHA: "abc123", Status: "queued", Title: "Waiting for unit", Summary: "waits for unit", DetailsURL: "https://vx.example/ui/x",
	})
	if err != nil || runID == 0 {
		t.Fatalf("StartCheckRun = %d, %v", runID, err)
	}
	run, ok := gh.CheckRun(runID)
	if !ok || run.Status != "queued" || run.Conclusion != "" || run.HeadSHA != "abc123" || run.DetailsURL != "https://vx.example/ui/x" {
		t.Fatalf("started run: %+v", run)
	}
	if err := c.UpdateCheckRun(ctx, id, "acme", "shop", runID, service.GitHubCheckRun{Name: "vx: e2e", Conclusion: "failure", Title: "e2e failed", Summary: "2 of 9 steps failed"}); err != nil {
		t.Fatal(err)
	}
	run, _ = gh.CheckRun(runID)
	if run.Status != "completed" || run.Conclusion != "failure" || run.Summary != "2 of 9 steps failed" || run.Updates != 1 {
		t.Fatalf("updated run: %+v", run)
	}
	if n := len(gh.CheckRuns("acme/shop")); n != 1 {
		t.Errorf("an update made a new run: %d runs", n)
	}

	gh.SetAppPermission("checks", "read")
	if _, err := c.StartCheckRun(ctx, id, "acme", "shop", service.GitHubCheckRun{Name: "vx: unit", HeadSHA: "abc123", Status: "queued"}); !errors.Is(err, service.ErrGitHubForbidden) {
		t.Errorf("start without checks: write: %v", err)
	}
	if err := c.UpdateCheckRun(ctx, id, "acme", "shop", runID, service.GitHubCheckRun{Name: "vx: e2e", Conclusion: "success"}); !errors.Is(err, service.ErrGitHubForbidden) {
		t.Errorf("update without checks: write: %v", err)
	}
}
