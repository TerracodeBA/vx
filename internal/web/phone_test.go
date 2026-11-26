package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// phoneEnv is a server with something on every page: efforts with lands
// and long paths, an escalated question, a pending review, an overlap,
// tasks, an organization, and a site admin signed in.
type phoneEnv struct {
	*webEnv
	token   string // alice's session token
	pages   []string
	effort  string
	review  string
	quest   string
	bob     service.Actor
	bobAg   service.Actor
	aliceAg service.Actor
	shift   time.Duration // how far the server's clock runs ahead
	// more are pages seen by others than alice: a newcomer whose account
	// reads nothing, and a browser asked to confirm a new account (#44).
	more []phoneSession
}

// phoneSession is a browser with its own cookies and the pages it shows.
type phoneSession struct {
	name    string
	cookies []phoneCookie
	pages   []string
	bare    bool // pages without the signed-in layout and menu
	gitHub  string
}

// phoneCookie is a cookie and the address it was set for.
type phoneCookie struct{ Name, Value, URL string }

func newPhoneEnv(t *testing.T) *phoneEnv {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice Liddell", SiteAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	aliceActor, _ := svc.ResolveActor(ctx, alice)
	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	e := &phoneEnv{webEnv: &webEnv{t: t, ctx: ctx, st: st, svc: svc, srv: srv, c: &http.Client{Jar: jar}, alice: aliceActor}}

	if _, err := svc.CreateOrg(ctx, aliceActor, "acme", "Acme Industries"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRepo(ctx, aliceActor, service.CreateRepoInput{Name: "acme/shop", Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	eff, err := svc.CreateEffort(ctx, aliceActor, service.CreateEffortInput{Repo: "acme/shop", Name: "Rate limiting for the login and signup endpoints", Goal: "Limit logins per account and per address"})
	if err != nil {
		t.Fatal(err)
	}
	e.effort = eff.Slug
	agP, _, err := svc.CreateAgent(ctx, aliceActor, "backend-with-a-long-name", "")
	if err != nil {
		t.Fatal(err)
	}
	e.aliceAg, _ = svc.ResolveActor(ctx, agP)
	long := "internal/httpx/middleware/ratelimit/token_bucket_with_a_rather_long_file_name.go"
	e.landFiles(e.aliceAg, eff.Slug, "Rules", map[string]string{"vx.toml": "[[policy]]\npaths = [\"db/**\"]\nmode = \"human\"\n"}, nil)
	e.landFiles(e.aliceAg, eff.Slug, "Add a token bucket limiter in front of login, with a long intent that wraps on a phone", map[string]string{
		long:          "package ratelimit\n\n// Allow reports whether the caller identified by key may make one more request right now, given the configured rate and burst; it never blocks.\nfunc Allow(key string) bool { return true }\n",
		"x/shared.go": "package x // one\n",
	}, &object.Evidence{Checks: []object.Check{{Name: "go test ./internal/httpx/middleware/ratelimit/...", Result: "pass", RanByVX: true, DurationMs: 4200}}})
	res := e.landFiles(e.aliceAg, eff.Slug, "Users table", map[string]string{"db/002_users.sql": "create table users (id uuid primary key);\n"}, nil)
	if res.Review == nil {
		t.Fatalf("not pending: %+v", res)
	}
	e.review = res.Review.ID.String()

	// Bob's agent asks alice's agent; nobody answers in time.
	bob, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	e.bob, _ = svc.ResolveActor(ctx, bob)
	if _, err := svc.SetMember(ctx, aliceActor, "acme", "bob", service.OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GrantRepo(ctx, aliceActor, "acme/shop", "bob", service.RoleWrite); err != nil {
		t.Fatal(err)
	}
	bobAg, _, err := svc.CreateAgent(ctx, e.bob, "audit", "")
	if err != nil {
		t.Fatal(err)
	}
	e.bobAg, _ = svc.ResolveActor(ctx, bobAg)
	q, err := svc.Ask(ctx, e.bobAg, service.AskInput{Repo: "acme/shop", Effort: eff.Slug, Targets: []string{long}, To: "alice/backend-with-a-long-name",
		Body: "Does the limiter key on the account id or the client address? Both have trade-offs <b>here</b>.", AnswerWithin: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	e.quest = q.ID.String()
	if _, err := svc.PostNote(ctx, e.bobAg, service.PostNoteInput{Repo: "acme/shop", Effort: eff.Slug, Kind: service.NotePlan, Targets: []string{long},
		Body: "Moving clientIP into internal/httpx so both limiters share it."}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTask(ctx, aliceActor, service.AddTaskInput{Repo: "acme/shop", Effort: eff.Slug, Title: "Wire the limiter into signup as well", Body: "Same bucket sizes."}); err != nil {
		t.Fatal(err)
	}
	// A second effort changes the same file: an overlap note on both.
	two, _ := svc.CreateEffort(ctx, e.bob, service.CreateEffortInput{Repo: "acme/shop", Name: "Shared helpers"})
	e.landFiles(e.bob, two.Slug, "Touch shared", map[string]string{"x/shared.go": "package x // two\n"}, nil)
	svc.WaitForecasts()
	svc.OpenWorkspace(ctx, e.aliceAg, "acme/shop", eff.Slug, "Wire limiter into signup")

	e.addNewcomers()

	// The question's deadline passes.
	e.shift = 2 * time.Minute
	svc.Now = func() time.Time { return time.Now().Add(e.shift) }

	tok, _ := st.CreateToken(ctx, alice.ID, "web")
	e.token = tok
	e.signIn(tok)
	ef := "/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug
	e.pages = []string{
		"/ui", "/ui/for-you", "/ui/repos", "/ui/repo?name=acme%2Fshop", "/ui/repos/acme/shop/access",
		ef, ef + "&view=board", ef + "&view=tasks", ef + "&view=changes",
		"/ui/change?repo=acme%2Fshop&review=" + e.review,
		"/ui/reviews", "/ui/ship?repo=acme%2Fshop&slug=" + eff.Slug, "/ui/settings",
		"/ui/orgs", "/ui/orgs/acme", "/ui/admin/people", "/ui/admin/github", "/ui/admin/audit",
	}
	// A landed change's page, found the way a person finds it.
	_, page := e.get(ef)
	if m := regexp.MustCompile(`href="(/ui/change\?repo=[^"]+&amp;ref=[^"]+)"`).FindStringSubmatch(page); m != nil {
		e.pages = append(e.pages, strings.ReplaceAll(m[1], "&amp;", "&"))
	} else {
		t.Fatal("effort page links no change")
	}
	return e
}

// addNewcomers signs up, with GitHub, a newcomer who reads no
// repositories (their empty pages explain why), and starts a second
// sign-up that waits on "create a new account?".
func (e *phoneEnv) addNewcomers() {
	t := e.t
	t.Helper()
	gh := githubtest.New(t)
	e.svc.GitHub.PublicURL, e.svc.GitHub.Signup = e.srv.URL, service.SignupOpen
	gh.Configure(t, e.svc)
	start := func(login string) (*http.Client, *cookiejar.Jar) {
		gh.AddUser(login)
		gh.SignIn(login)
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		resp, err := c.Get(e.srv.URL + "/ui/login/github")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Request.URL.Path != "/auth/github/signup" || !strings.Contains(string(b), "You are about to create a new vx account for GitHub") {
			t.Fatalf("sign-up of %s did not ask first: %s\n%s", login, resp.Request.URL, b)
		}
		return c, jar
	}
	const login = "newcomer-with-a-rather-long-github-login"
	c, jar := start(login)
	req, _ := http.NewRequest("POST", e.srv.URL+"/auth/github/signup", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, _ := url.Parse(e.srv.URL + "/ui")
	var newcomer phoneSession
	newcomer.name, newcomer.gitHub = "newcomer", login
	for _, ck := range jar.Cookies(u) {
		newcomer.cookies = append(newcomer.cookies, phoneCookie{ck.Name, ck.Value, e.srv.URL + "/ui"})
	}
	if len(newcomer.cookies) == 0 {
		t.Fatal("newcomer has no session")
	}
	newcomer.pages = []string{"/ui", "/ui/repos", "/ui/for-you"}

	_, jar = start("second-account")
	su, _ := url.Parse(e.srv.URL + "/auth/github/signup")
	asking := phoneSession{name: "signup", pages: []string{"/auth/github/signup"}, bare: true}
	for _, ck := range jar.Cookies(su) {
		asking.cookies = append(asking.cookies, phoneCookie{ck.Name, ck.Value, e.srv.URL + "/auth/github/signup"})
	}
	e.more = append(e.more, newcomer, asking)
}

// client is a browser with s's cookies.
func (e *phoneEnv) client(s phoneSession) *http.Client {
	jar, _ := cookiejar.New(nil)
	for _, ck := range s.cookies {
		u, _ := url.Parse(ck.URL)
		path := u.Path
		if i := strings.LastIndex(path, "/"); i > 0 {
			path = path[:i]
		}
		jar.SetCookies(u, []*http.Cookie{{Name: ck.Name, Value: ck.Value, Path: path}})
	}
	return &http.Client{Jar: jar}
}

func (e *phoneEnv) get(path string) (int, string) {
	e.t.Helper()
	resp, err := e.c.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Every page carries the phone hooks: the viewport, the manifest, the
// phone stylesheet and script, the menu button with For you first in the
// menu; and no inline styles or scripts (the CSP refuses them).
func TestPhonePagesMarkup(t *testing.T) {
	e := newPhoneEnv(t)
	inlineStyle := regexp.MustCompile(`(?i)\sstyle\s*=`)
	inlineScript := regexp.MustCompile(`(?i)<script(\s[^>]*)?>[^<]`)
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*"`)
	for _, path := range e.pages {
		code, body := e.get(path)
		if code != 200 {
			t.Errorf("%s: status %d", path, code)
			continue
		}
		for _, want := range []string{
			`name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"`,
			`<link rel="manifest" href="/ui/manifest.webmanifest">`,
			`<meta name="theme-color"`, `rel="apple-touch-icon"`,
			`href="/ui/static/phone.css"`, `src="/ui/static/phone.js"`,
			`class="menu-btn"`, `aria-controls="sidebar"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", path, want)
			}
		}
		nav := body[strings.Index(body, `<nav class="side-nav"`):]
		if i := strings.Index(nav, `class="side-link`); i < 0 || !strings.HasPrefix(nav[i:], `class="side-link side-for-you`) {
			t.Errorf("%s: For you is not first in the menu", path)
		}
		if m := inlineStyle.FindString(body); m != "" {
			t.Errorf("%s: inline style attribute %q", path, m)
		}
		if m := inlineScript.FindString(body); m != "" {
			t.Errorf("%s: inline script %q", path, m)
		}
		if m := handler.FindString(body); m != "" {
			t.Errorf("%s: inline event handler %q", path, m)
		}
		if strings.Contains(body, "<b>here</b>") {
			t.Errorf("%s: unescaped note text", path)
		}
		if strings.Contains(body, `id="no-access"`) {
			t.Errorf("%s: alice reads repositories, yet the page says she reads none", path)
		}
	}
	// The newcomer's empty pages say who is signed in and why they are
	// empty; the sign-up page asks before creating an account. Both have
	// the phone hooks and no inline styles or scripts.
	for _, s := range e.more {
		c := e.client(s)
		for _, path := range s.pages {
			resp, err := c.Get(e.srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := string(b)
			if resp.StatusCode != 200 {
				t.Errorf("%s %s: status %d\n%s", s.name, path, resp.StatusCode, body)
				continue
			}
			want := []string{`name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"`, `href="/ui/static/phone.css"`}
			if s.bare {
				want = append(want, "You are about to create a new vx account for GitHub", "Not you? Switch GitHub account")
			} else {
				want = append(want, `class="menu-btn"`, `id="no-access"`, "You are signed in as", "(GitHub: <strong class=\"mono\">"+s.gitHub+"</strong>)",
					"This account cannot read any repositories.", "GitHub: <span class=\"mono\">"+s.gitHub+"</span>")
			}
			for _, w := range want {
				if !strings.Contains(body, w) {
					t.Errorf("%s %s: missing %q", s.name, path, w)
				}
			}
			for _, re := range []*regexp.Regexp{inlineStyle, inlineScript, handler} {
				if m := re.FindString(body); m != "" {
					t.Errorf("%s %s: inline %q", s.name, path, m)
				}
			}
		}
	}
	// The phone stylesheet has the breakpoints and hooks the pages rely on.
	_, css := e.get("/ui/static/phone.css")
	for _, want := range []string{"@media (max-width: 860px)", "@media (max-width: 520px)", ".menu-btn", ".fy-list", ".diff-scroll", "min-height: 40px", "overflow-x: auto"} {
		if !strings.Contains(css, want) {
			t.Errorf("phone.css lacks %q", want)
		}
	}
	// The offline page needs no session and works on its own.
	resp, err := http.Get(e.srv.URL + "/ui/offline")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "You are offline") || !strings.Contains(string(b), "/ui/static/phone.css") {
		t.Fatalf("offline page = %d\n%s", resp.StatusCode, b)
	}
}
