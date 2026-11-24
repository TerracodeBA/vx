package cli_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// TestGitHubRolesCLI runs GitHub-backed roles end to end through the CLI
// and the web UI, against the fake GitHub: alice is an admin of acme on
// GitHub, bob a member with read on acme/api and nothing on acme/secret,
// carol is not in acme.
func TestGitHubRolesCLI(t *testing.T) {
	s := newAuthzServer(t)
	ctx := context.Background()
	st := s.harness.st
	gh := githubtest.New(t)
	ghUsers := map[string]githubtest.Account{}
	for _, l := range []string{"alice", "bob", "carol"} {
		ghUsers[l] = gh.AddUser(l)
	}
	acme := gh.AddOrg("acme")
	gh.SetOrgMember("acme", "alice", "admin")
	gh.SetOrgMember("acme", "bob", "member")
	apiGH, secretGH := gh.AddRepo("acme/api", true), gh.AddRepo("acme/secret", true)
	gh.SetPermission("acme/api", "bob", "read")
	inst := gh.Install("acme")
	s.svc.GitHub.API = gh.Client(t)

	carol, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	carolTok, err := st.CreateToken(ctx, carol.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []store.Principal{s.alice.Principal, s.bob.Principal, carol} {
		if err := st.LinkGitHub(ctx, p.ID, ghUsers[p.Handle].ID, p.Handle); err != nil {
			t.Fatal(err)
		}
	}
	org, err := st.OrgByHandle(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertGitHubInstallation(ctx, store.GitHubInstallationRow{ID: inst, AccountID: acme.ID, AccountLogin: "acme", AccountType: "Organization", OrgID: &org.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetOrgGitHub(ctx, org.ID, &acme.ID, "acme"); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	alice := s.user("alice", s.tokens["alice"], work)
	bob := s.user("bob", s.tokens["bob"], work)
	carolU := s.user("carol", carolTok, work)
	aliceAgent := s.user("alice-agent", agentToken(t, alice, "claude"), work)
	bobAgent := s.user("bob-agent", agentToken(t, bob, "claude"), work)

	for name, ghRepo := range map[string]githubtest.Repo{"acme/api": apiGH, "acme/secret": secretGH} {
		alice.must("repo", "create", name)
		r, err := st.RepoByName(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetRepoGitHub(ctx, r.ID, &ghRepo.ID, &inst); err != nil {
			t.Fatal(err)
		}
	}
	alice.must("effort", "new", "Rate limits", "--repo", "acme/api")
	aliceAgent.must("clone", "acme/api", "rate-limits", "a")
	aliceAgent.dir = filepath.Join(work, "a")
	aliceAgent.must("new", "Add limiter")
	aliceAgent.write("limit.go", "package api\n")
	aliceAgent.must("land")

	// Who sees what.
	if out := alice.must("repo", "list"); !strings.Contains(out, "acme/api") || !strings.Contains(out, "acme/secret") || !strings.Contains(out, "roles from GitHub") {
		t.Errorf("alice's repo list:\n%s", out)
	}
	for _, u := range []*user{bob, bobAgent} {
		out := u.must("repo", "list")
		if !strings.Contains(out, "acme/api") || strings.Contains(out, "acme/secret") || !strings.Contains(out, "roles from GitHub") {
			t.Errorf("[%s] repo list:\n%s", u.label, out)
		}
		if js := u.must("repo", "list", "--json"); !strings.Contains(js, `"github_backed":true`) && !strings.Contains(js, `"githubBacked":true`) {
			t.Errorf("[%s] repo list --json lacks github_backed:\n%s", u.label, js)
		}
		if out, code := u.run("effort", "list", "--repo", "acme/secret"); code == 0 || !hidden(out) {
			t.Errorf("[%s] effort list on acme/secret: exit %d\n%s", u.label, code, out)
		}
	}
	if out := carolU.must("repo", "list"); strings.Contains(out, "acme/") {
		t.Errorf("carol's repo list:\n%s", out)
	}

	// Read: bob's agent clones but cannot start work; vx grants are refused.
	bobAgent.must("clone", "acme/api", "rate-limits", "b")
	bobAgent.dir = filepath.Join(work, "b")
	if out, code := bobAgent.run("new", "Tune limits"); code == 0 || !strings.Contains(out, "needs write") || !strings.Contains(out, "come from GitHub") {
		t.Errorf("vx new with read from GitHub: exit %d\n%s", code, out)
	}
	if out, code := alice.run("repo", "grant", "acme/api", "bob", "--role", "write"); code == 0 || !strings.Contains(out, "github.com/acme/api/settings/access") {
		t.Errorf("vx repo grant on a GitHub-backed repository: exit %d\n%s", code, out)
	}

	// GitHub gives bob write; the webhook's invalidation applies it at once.
	gh.SetPermission("acme/api", "bob", "write")
	if err := s.svc.DropGitHubRoles(ctx, service.GitHubRoleScope{GitHubRepoID: apiGH.ID, Reason: "member edited"}); err != nil {
		t.Fatal(err)
	}
	bobAgent.must("new", "Tune limits")
	bobAgent.write("limit.go", "package api\n\n// Limit is per client.\n")
	bobAgent.must("land")
	if out, code := bobAgent.run("ship", "plan"); code == 0 || !strings.Contains(out, "needs admin") {
		t.Errorf("ship plan with write from GitHub: exit %d\n%s", code, out)
	}

	// Web UI: the repository page says where roles come from.
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	form := url.Values{"token": {s.tokens["bob"]}, "next": {"/ui"}}
	req, _ := http.NewRequest("POST", s.server+"/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.server)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for path, want := range map[string]int{"/ui/repo?name=acme/api": 200, "/ui/repo?name=acme/secret": 404} {
		resp, err := c.Get(s.server + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want || (want == 200 && !strings.Contains(string(body), "roles from GitHub")) {
			t.Errorf("GET %s as bob: %d (want %d)\n%.300s", path, resp.StatusCode, want, body)
		}
	}
}
