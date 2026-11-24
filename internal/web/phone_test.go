package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
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
}

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
