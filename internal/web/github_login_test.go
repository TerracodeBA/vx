package web_test

import (
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

type ghWeb struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	svc *service.Service
	gh  *githubtest.Server
	srv *httptest.Server
}

func newGHWeb(t *testing.T, mode service.SignupMode) *ghWeb {
	t.Helper()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	gh := githubtest.New(t)
	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc.GitHub.PublicURL, svc.GitHub.Signup = srv.URL, mode
	gh.Configure(t, svc)
	return &ghWeb{t: t, ctx: context.Background(), st: st, svc: svc, gh: gh, srv: srv}
}

// browser is a client with its own cookies, like one browser.
func (e *ghWeb) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (e *ghWeb) get(c *http.Client, path string) (int, string) {
	e.t.Helper()
	if !strings.HasPrefix(path, "http") {
		path = e.srv.URL + path
	}
	resp, err := c.Get(path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *ghWeb) post(c *http.Client, path string, form url.Values, origin string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// signIn signs login in to GitHub (the fake) and clicks Sign in with GitHub
// in browser c; it returns the page the callback answers with.
func (e *ghWeb) signIn(c *http.Client, login, next string) (int, string) {
	e.t.Helper()
	e.gh.SignIn(login)
	path := "/ui/login/github"
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	return e.get(c, path)
}

// dest is where the continue page sends the browser.
func dest(t *testing.T, page string) string {
	t.Helper()
	const marker = `http-equiv="refresh" content="0;url=`
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("not a continue page:\n%s", page)
	}
	rest := page[i+len(marker):]
	return html.UnescapeString(rest[:strings.Index(rest, `"`)])
}

func TestGitHubWebSignInAndWelcome(t *testing.T) {
	e := newGHWeb(t, service.SignupOpen)
	e.gh.AddUser("Octocat")
	c := e.browser()

	_, login := e.get(c, "/ui/login?next=/ui/settings")
	if !strings.Contains(login, "Sign in with GitHub") || !strings.Contains(login, `href="/ui/login/github?next=%2fui%2fsettings"`) ||
		!strings.Contains(login, `name="token"`) || !strings.Contains(login, "vx ui") {
		t.Fatalf("login page:\n%s", login)
	}

	code, page := e.signIn(c, "Octocat", "")
	if code != 200 || dest(t, page) != "/ui/welcome" {
		t.Fatalf("first sign-in = %d\n%s", code, page)
	}
	p, err := e.st.PrincipalByHandle(e.ctx, "octocat")
	if err != nil {
		t.Fatalf("person not created: %v", err)
	}
	code, welcome := e.get(c, "/ui/welcome")
	if code != 200 {
		t.Fatalf("welcome = %d\n%s", code, welcome)
	}
	for _, want := range []string{
		"Install the vx GitHub App on your organization",
		"curl -fsSL " + e.srv.URL + "/install.sh | sh",
		"vx login --server " + e.srv.URL,
		"/plugin marketplace add TerracodeBA/vx",
		"/plugin install vx@vx",
		`data-copy="cmd-install"`,
		`href="/ui/welcome"`, // linked from the navigation
		"Octocat",
	} {
		if !strings.Contains(welcome, want) {
			t.Errorf("welcome page missing %q", want)
		}
	}
	for _, bad := range []string{" style=", "<script>", "—"} {
		if strings.Contains(welcome, bad) {
			t.Errorf("welcome page contains %q", bad)
		}
	}

	// Dismissing is a same-origin POST; afterwards the menu has no link.
	if code, _ := e.post(c, "/ui/welcome", url.Values{}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin dismiss = %d", code)
	}
	if !e.svc.ShowWelcome(e.ctx, service.Actor{Principal: p}) {
		t.Fatal("cross-origin post dismissed the welcome page")
	}
	if code, _ := e.post(c, "/ui/welcome", url.Values{}, e.srv.URL); code != 200 {
		t.Fatalf("dismiss = %d", code)
	}
	if _, home := e.get(c, "/ui"); strings.Contains(home, `href="/ui/welcome"`) {
		t.Fatal("welcome still in the menu after dismissing")
	}
	if _, welcome := e.get(c, "/ui/welcome"); !strings.Contains(welcome, "Show it again") {
		t.Fatal("a dismissed welcome page should offer to show it again")
	}

	// A second sign-in in a new browser goes to /ui, not the welcome page.
	c2 := e.browser()
	if code, page := e.signIn(c2, "Octocat", ""); code != 200 || dest(t, page) != "/ui" {
		t.Fatalf("second sign-in = %d\n%s", code, page)
	}
	if _, home := e.get(c2, "/ui"); !strings.Contains(home, "Sign out") {
		t.Fatal("not signed in after the continue page")
	}
}

func TestGitHubWebSignInNext(t *testing.T) {
	e := newGHWeb(t, service.SignupOpen)
	e.gh.AddUser("nina")
	for _, c := range []struct{ next, want string }{
		{"/ui/cli/approve?code=WDJB-MJHT", "/ui/cli/approve?code=WDJB-MJHT"}, // first sign-in keeps an explicit next
		{"//evil.example/ui", "/ui"},
		{"https://evil.example/ui", "/ui"},
		{"/ui/settings", "/ui/settings"},
	} {
		_, page := e.signIn(e.browser(), "nina", c.next)
		if got := dest(t, page); got != c.want {
			t.Errorf("next %q: went to %q, want %q", c.next, got, c.want)
		}
	}
	// An unsigned visitor of a page is sent through sign-in and back.
	c := e.browser()
	resp, err := (&http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(e.srv.URL + "/ui/settings")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	u, _ := url.Parse(loc)
	if _, page := e.signIn(c, "nina", u.Query().Get("next")); dest(t, page) != "/ui/settings" {
		t.Fatalf("round trip from %s lost next:\n%s", loc, page)
	}
}

func TestGitHubWebSignInErrors(t *testing.T) {
	e := newGHWeb(t, service.SignupInvite)
	e.gh.AddUser("ivan")
	noFollow := func(c *http.Client) *http.Client {
		return &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	// callbackURL starts a sign-in in c and approves it on GitHub, without
	// following GitHub's redirect back.
	callbackURL := func(c *http.Client) *url.URL {
		t.Helper()
		e.gh.SignIn("ivan")
		resp, err := noFollow(c).Get(e.srv.URL + "/ui/login/github")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		resp, err = noFollow(c).Get(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		u, _ := url.Parse(resp.Header.Get("Location"))
		if !strings.HasPrefix(u.String(), e.srv.URL+service.GitHubCallbackPath) {
			t.Fatalf("GitHub sent the browser to %s", u)
		}
		return u
	}

	// Not invited: a friendly page naming the login and what to do.
	code, page := e.signIn(e.browser(), "ivan", "")
	if code != http.StatusForbidden || !strings.Contains(page, "not invited yet") || !strings.Contains(page, "invite ivan") {
		t.Fatalf("not invited = %d\n%s", code, page)
	}
	if _, err := e.st.PrincipalByHandle(e.ctx, "ivan"); err == nil {
		t.Fatal("an uninvited person was created")
	}

	// Denied on GitHub.
	e.gh.DenyAuthorization(true)
	code, page = e.signIn(e.browser(), "ivan", "")
	if code != http.StatusForbidden || !strings.Contains(page, "You cancelled on GitHub") || !strings.Contains(page, "Try again") {
		t.Fatalf("denied = %d\n%s", code, page)
	}
	e.gh.DenyAuthorization(false)

	// Missing cookie: the callback opened in another browser.
	cb := callbackURL(e.browser())
	code, page = e.get(e.browser(), cb.String())
	if code != http.StatusBadRequest || !strings.Contains(page, "another browser") {
		t.Fatalf("missing cookie = %d\n%s", code, page)
	}

	// Tampered state.
	c := e.browser()
	cb = callbackURL(c)
	q := cb.Query()
	q.Set("state", q.Get("state")+"x")
	cb.RawQuery = q.Encode()
	code, page = e.get(c, cb.String())
	if code != http.StatusBadRequest || !strings.Contains(page, "did not match") {
		t.Fatalf("tampered state = %d\n%s", code, page)
	}

	// Tampered cookie: a browser cannot write its own flow (for example to
	// link its GitHub account to someone else).
	c = e.browser()
	cb = callbackURL(c)
	cu, _ := url.Parse(e.srv.URL + "/auth/github")
	for _, ck := range c.Jar.Cookies(cu) {
		if strings.HasPrefix(ck.Name, "vx_github") {
			ck.Value = "e30." + ck.Value[strings.Index(ck.Value, ".")+1:] // {} with the old signature
			ck.Path = "/auth/github"
			c.Jar.SetCookies(cu, []*http.Cookie{ck})
		}
	}
	if code, page = e.get(c, cb.String()); code != http.StatusBadRequest {
		t.Fatalf("tampered cookie = %d\n%s", code, page)
	}

	// Replayed callback: the cookie is used up by the first one.
	e.svc.InviteGitHubLogin(e.ctx, nil, "ivan", "", false)
	c = e.browser()
	cb = callbackURL(c)
	if code, page = e.get(c, cb.String()); code != 200 {
		t.Fatalf("invited sign-in = %d\n%s", code, page)
	}
	if code, page = e.get(c, cb.String()); code != http.StatusBadRequest || !strings.Contains(page, "already used") {
		t.Fatalf("replayed callback = %d\n%s", code, page)
	}
}

func TestGitHubWebSignupClosedAndNotConfigured(t *testing.T) {
	e := newGHWeb(t, service.SignupClosed)
	e.gh.AddUser("zed")
	if code, page := e.signIn(e.browser(), "zed", ""); code != http.StatusForbidden || !strings.Contains(page, "does not take new accounts") {
		t.Fatalf("closed = %d\n%s", code, page)
	}

	// A server without a GitHub App: token sign-in only.
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	e.gh.Prepare(t, svc)
	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(mux)
	defer e.srv.Close()
	c := e.browser()
	if _, login := e.get(c, "/ui/login"); strings.Contains(login, "Sign in with GitHub") {
		t.Fatal("login page offers GitHub without a GitHub App")
	}
	if code, page := e.get(c, "/ui/login/github"); code != http.StatusServiceUnavailable || !strings.Contains(page, "no GitHub App yet") {
		t.Fatalf("not configured = %d\n%s", code, page)
	}
}

func TestGitHubWebConnectExistingAccount(t *testing.T) {
	e := newGHWeb(t, service.SignupClosed)
	frank, _ := e.st.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "frank"})
	token, _ := e.st.CreateToken(e.ctx, frank.ID, "web")
	e.gh.AddUser("frank-gh")
	c := e.browser()
	if resp, err := postForm(c, e.srv.URL, "/ui/login", url.Values{"token": {token}, "next": {"/ui"}}); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if _, page := e.get(c, "/ui/settings"); !strings.Contains(page, "Connect GitHub") {
		t.Fatalf("settings has no Connect GitHub:\n%s", page)
	}
	e.gh.SignIn("frank-gh")
	if code, _ := e.post(c, "/ui/settings/github", url.Values{}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin connect = %d", code)
	}
	code, page := e.post(c, "/ui/settings/github", url.Values{}, e.srv.URL)
	if code != 200 || dest(t, page) != "/ui/settings?github=connected" {
		t.Fatalf("connect = %d\n%s", code, page)
	}
	if _, page := e.get(c, "/ui/settings?github=connected"); !strings.Contains(page, "GitHub connected") || !strings.Contains(page, "frank-gh") {
		t.Fatalf("settings after connect:\n%s", page)
	}
	// Signing in with GitHub now finds frank, though sign-up is closed.
	c2 := e.browser()
	if code, page := e.signIn(c2, "frank-gh", ""); code != 200 || dest(t, page) != "/ui" {
		t.Fatalf("sign-in after connect = %d\n%s", code, page)
	}
	if _, home := e.get(c2, "/ui"); !strings.Contains(home, "frank") {
		t.Fatal("not signed in as frank")
	}
}

func TestGitHubWebInvites(t *testing.T) {
	e := newGHWeb(t, service.SignupInvite)
	admin, _ := e.st.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "root", SiteAdmin: true})
	user, _ := e.st.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "user"})
	adminTok, _ := e.st.CreateToken(e.ctx, admin.ID, "web")
	userTok, _ := e.st.CreateToken(e.ctx, user.ID, "web")
	signedIn := func(tok string) *http.Client {
		c := e.browser()
		resp, err := postForm(c, e.srv.URL, "/ui/login", url.Values{"token": {tok}, "next": {"/ui"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return c
	}
	a, u := signedIn(adminTok), signedIn(userTok)
	if _, page := e.get(a, "/ui/admin/people"); !strings.Contains(page, "Invite by GitHub login") || !strings.Contains(page, "No open invites") {
		t.Fatalf("people page:\n%s", page)
	}
	if code, _ := e.post(a, "/ui/admin/invites", url.Values{"login": {"Mona"}, "note": {"waitlist"}}, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin invite = %d", code)
	}
	if code, _ := e.post(u, "/ui/admin/invites", url.Values{"login": {"mona"}}, e.srv.URL); code != http.StatusNotFound {
		t.Fatalf("invite by a non-admin = %d", code)
	}
	code, page := e.post(a, "/ui/admin/invites", url.Values{"login": {"Mona"}, "note": {"waitlist"}}, e.srv.URL)
	if code != 200 || !strings.Contains(page, "Invite saved") || !strings.Contains(page, ">mona") || !strings.Contains(page, "waitlist") {
		t.Fatalf("invite = %d\n%s", code, page)
	}
	if code, page := e.post(a, "/ui/admin/invites", url.Values{"login": {"not a login"}}, e.srv.URL); code != http.StatusBadRequest || !strings.Contains(page, "is not a GitHub login") {
		t.Fatalf("bad login = %d\n%s", code, page)
	}
	// The invited person signs up; the invite leaves the list.
	e.gh.AddUser("mona")
	if code, page := e.signIn(e.browser(), "mona", ""); code != 200 || dest(t, page) != "/ui/welcome" {
		t.Fatalf("invited sign-up = %d\n%s", code, page)
	}
	if _, page := e.get(a, "/ui/admin/people"); !strings.Contains(page, "No open invites") || !strings.Contains(page, "GitHub mona") {
		t.Fatalf("people page after sign-up:\n%s", page)
	}
	// Withdraw.
	e.post(a, "/ui/admin/invites", url.Values{"login": {"otto"}}, e.srv.URL)
	if code, page := e.post(a, "/ui/admin/invites/withdraw", url.Values{"login": {"otto"}}, e.srv.URL); code != 200 || !strings.Contains(page, "Invite withdrawn") {
		t.Fatalf("withdraw = %d\n%s", code, page)
	}
}
