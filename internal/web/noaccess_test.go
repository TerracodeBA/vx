package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// TestGitHubSignupAsksFirst: a GitHub sign-in without a vx account asks
// "create a new vx account for GitHub LOGIN?" and creates nothing until
// Continue, which must come from this site and this browser (#44).
func TestGitHubSignupAsksFirst(t *testing.T) {
	e := newGHWeb(t, service.SignupOpen)
	e.gh.AddUser("dora")
	c := e.browser()
	code, page := e.signIn(c, "dora", "/ui/settings")
	for _, want := range []string{
		"You are about to create a new vx account for GitHub <strong class=\"mono\">dora</strong>",
		`action="/auth/github/signup"`, ">Continue<", "Not you? Switch GitHub account",
		`href="` + e.gh.URL + `/logout"`, "GitHub keeps its own session",
	} {
		if code != 200 || !strings.Contains(page, want) {
			t.Fatalf("sign-up page = %d lacks %q:\n%s", code, want, page)
		}
	}
	for _, bad := range []string{" style=", "<script>", "\u2014"} {
		if strings.Contains(page, bad) {
			t.Errorf("sign-up page contains %q", bad)
		}
	}
	noAccount := func(when string) {
		t.Helper()
		if _, err := e.st.PrincipalByHandle(e.ctx, "dora"); err == nil {
			t.Fatalf("%s: an account was created before Continue", when)
		}
	}
	noAccount("asking")
	// Reloading asks again.
	if code, page := e.get(c, "/auth/github/signup"); code != 200 || !strings.Contains(page, "You are about to create a new vx account") {
		t.Fatalf("reload = %d\n%s", code, page)
	}
	// A cross-origin Continue is refused.
	if code, _ := e.post(c, "/auth/github/signup", url.Values{}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin Continue = %d", code)
	}
	noAccount("cross-origin Continue")
	// Another browser has no sign-up to continue, nor one with a forged cookie.
	if code, page := e.post(e.browser(), "/auth/github/signup", url.Values{}, e.srv.URL); code != http.StatusBadRequest || !strings.Contains(page, "No account was created") {
		t.Fatalf("Continue in another browser = %d\n%s", code, page)
	}
	forged := e.browser()
	u, _ := url.Parse(e.srv.URL + "/auth/github/signup")
	for _, ck := range c.Jar.Cookies(u) {
		forged.Jar.SetCookies(u, []*http.Cookie{{Name: ck.Name, Value: strings.Replace(ck.Value, ".", "x.", 1), Path: "/auth/github"}})
	}
	if code, _ := e.post(forged, "/auth/github/signup", url.Values{}, e.srv.URL); code != http.StatusBadRequest {
		t.Fatalf("Continue with a forged cookie = %d", code)
	}
	noAccount("other browsers")

	// Continue creates the account and keeps where the sign-in was going.
	code, page = e.post(c, "/auth/github/signup", url.Values{}, e.srv.URL)
	if code != 200 || dest(t, page) != "/ui/settings" {
		t.Fatalf("Continue = %d\n%s", code, page)
	}
	if _, err := e.st.PrincipalByHandle(e.ctx, "dora"); err != nil {
		t.Fatalf("no account after Continue: %v", err)
	}
	// Once only.
	if code, _ := e.post(c, "/auth/github/signup", url.Values{}, e.srv.URL); code != http.StatusBadRequest {
		t.Fatalf("second Continue = %d", code)
	}
	// Signing in again finds the account without asking.
	if code, page := e.signIn(e.browser(), "dora", ""); code != 200 || dest(t, page) != "/ui" {
		t.Fatalf("second sign-in = %d\n%s", code, page)
	}

	// An invited person is expected: no question, even with open sign-up.
	if _, err := e.svc.InviteGitHubLogin(e.ctx, nil, "ines", "", false); err != nil {
		t.Fatal(err)
	}
	e.gh.AddUser("ines")
	if code, page := e.signIn(e.browser(), "ines", ""); code != 200 || dest(t, page) != "/ui/welcome" {
		t.Fatalf("invited sign-up = %d\n%s", code, page)
	}
}

// TestEmptyPagesExplainAccess: someone who reads no repository sees, on
// Repositories, All efforts and For you, which account is signed in, why
// it reads nothing and what to do; once they read one, the pages are
// back to normal. The sidebar names the account and its GitHub login.
func TestEmptyPagesExplainAccess(t *testing.T) {
	e := newGHWeb(t, service.SignupOpen)
	e.gh.AddUser("Dora-Two")
	c := e.browser()
	if code, page := e.signUp(c, "Dora-Two", ""); code != 200 {
		t.Fatalf("sign-up = %d\n%s", code, page)
	}
	for _, path := range []string{"/ui", "/ui/repos", "/ui/for-you"} {
		code, page := e.get(c, path)
		for _, want := range []string{
			`id="no-access"`,
			"You are signed in as <strong class=\"mono\">dora-two</strong> (GitHub: <strong class=\"mono\">Dora-Two</strong>). This account cannot read any repositories.",
			"This account was created just now", "another GitHub account than last time",
			"It belongs to no organization in vx besides its own personal one.",
			"The vx GitHub App is not installed on any GitHub organization Dora-Two belongs to",
			`action="/ui/logout"`, "switch accounts on GitHub first", `href="` + e.gh.URL + `/logout"`,
			"vx org member ORG dora-two --role member", "vx repo grant REPO user:dora-two --role read",
			"Install the vx GitHub App",
			"GitHub: <span class=\"mono\">Dora-Two</span>", // the sidebar
		} {
			if code != 200 || !strings.Contains(page, want) {
				t.Errorf("%s = %d lacks %q", path, code, want)
			}
		}
		if strings.Contains(page, "No repositories yet") || strings.Contains(page, "Nothing waits for you") {
			t.Errorf("%s shows the plain empty state too", path)
		}
	}
	// The sidebar names the account on every page.
	if _, page := e.get(c, "/ui/settings"); !strings.Contains(page, "GitHub: <span class=\"mono\">Dora-Two</span>") || !strings.Contains(page, ">dora-two</span>") {
		t.Error("settings sidebar does not name the account and its GitHub login")
	}

	// Given a repository, the explanation goes away.
	adminP, _ := e.st.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "admin"})
	admin, _ := e.svc.ResolveActor(e.ctx, adminP)
	if _, err := e.svc.CreateRepo(e.ctx, admin, service.CreateRepoInput{Name: "shop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GrantRepo(e.ctx, admin, "shop", "dora-two", service.RoleRead); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/ui", "/ui/repos", "/ui/for-you"} {
		if code, page := e.get(c, path); code != 200 || strings.Contains(page, `id="no-access"`) || strings.Contains(page, "cannot read any repositories") {
			t.Errorf("%s = %d still explains an empty account", path, code)
		}
	}
	if _, page := e.get(c, "/ui/for-you"); !strings.Contains(page, "Nothing waits for you") {
		t.Error("For you with a repository and nothing waiting lost its empty state")
	}
	if _, page := e.get(c, "/ui/repos"); !strings.Contains(page, "name=shop") {
		t.Error("the granted repository is not listed")
	}
}
