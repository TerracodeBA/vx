package cli_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/oidc/oidctest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// ssoServer is a vx server with a public URL and a secret key, alice
// (owner of acme), bob (member), the repository acme/api, and a fake
// identity provider.
type ssoServer struct {
	t                    *testing.T
	url                  string
	svc                  *service.Service
	idp                  *oidctest.Provider
	aliceToken, bobToken string
	alice, bob           store.Principal
}

func newSSOServer(t *testing.T) *ssoServer {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc.GitHub.PublicURL, svc.GitHub.SecretKey = srv.URL, bytes.Repeat([]byte{9}, 32)
	s := &ssoServer{t: t, url: srv.URL, svc: svc, idp: oidctest.New(t)}
	var err error
	if s.alice, err = st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", Email: "alice@acme.test"}); err != nil {
		t.Fatal(err)
	}
	if s.bob, err = st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob", Email: "bob@acme.test"}); err != nil {
		t.Fatal(err)
	}
	s.aliceToken, _ = st.CreateToken(ctx, s.alice.ID, "test")
	s.bobToken, _ = st.CreateToken(ctx, s.bob.ID, "test")
	alice, _ := svc.ResolveActor(ctx, s.alice)
	if _, err := svc.CreateOrg(ctx, alice, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetMember(ctx, alice, "acme", "bob", service.OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRepo(ctx, alice, service.CreateRepoInput{Name: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	return s
}

// throughProvider follows a redirect to the fake provider and back to vx's
// callback, and returns the callback's answer.
func (s *ssoServer) throughProvider(b *browser, idpURL string) (int, string) {
	s.t.Helper()
	if !strings.HasPrefix(idpURL, s.idp.Issuer+"/authorize?") {
		s.t.Fatalf("not sent to the provider: %q", idpURL)
	}
	status, back, _ := b.get(idpURL)
	if status != http.StatusFound || !strings.HasPrefix(back, s.url+service.OIDCCallbackPath+"?") {
		s.t.Fatalf("provider answered %d %q", status, back)
	}
	status, _, body := b.get(back)
	return status, body
}

// ssoSignIn starts single sign-on from the browser's current state (GET,
// so a signed-in browser gets the confirmation page first and posts it).
func (s *ssoServer) ssoSignIn(b *browser, query string) (int, string) {
	s.t.Helper()
	status, loc, body := b.get(s.url + "/ui/login/sso?" + query)
	if status == http.StatusOK && strings.Contains(body, "Continue to acme single sign-on") {
		q, _ := url.ParseQuery(query)
		next := q.Get("next")
		if next == "" {
			next = "/ui"
		}
		req, _ := http.NewRequest(http.MethodPost, s.url+"/ui/login/sso", strings.NewReader(url.Values{"org": {"acme"}, "next": {next}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", s.url)
		resp, err := b.c.Do(req)
		if err != nil {
			s.t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			s.t.Fatalf("confirm = %d", resp.StatusCode)
		}
		loc = resp.Header.Get("Location")
	} else if status != http.StatusFound {
		s.t.Fatalf("start = %d %s", status, body)
	}
	return s.throughProvider(b, loc)
}

func idpUser(sub, email string) oidctest.User {
	return oidctest.User{Subject: sub, Email: email, EmailVerified: oidctest.Verified(true)}
}

// TestSSOWebAndCLI is single sign-on through the web UI and the CLI's
// browser login: an owner sets it up and requires it, a member signed in
// with a token gets the single sign-on page instead of the repository and
// a clear CLI error, connects, and vx login through single sign-on gives
// the CLI a token that works. Someone new whose email already has an
// account gets "this looks like you" instead of a second account.
func TestSSOWebAndCLI(t *testing.T) {
	s := newSSOServer(t)

	// The owner configures single sign-on on the organization page.
	alice := newBrowser(t, s.url)
	alice.signIn(s.aliceToken)
	if status, body := alice.post("/ui/orgs/acme/sso", url.Values{"issuer": {s.idp.Issuer}, "client_id": {s.idp.ClientID},
		"client_secret": {s.idp.ClientSecret}, "domains": {"acme.test"}}, s.url); status != http.StatusSeeOther {
		t.Fatalf("save single sign-on = %d %s", status, body)
	}
	_, _, page := alice.get(s.url + "/ui/orgs/acme")
	for _, want := range []string{"Single sign-on", s.idp.Issuer, s.url + "/auth/oidc/callback", "Sign in with single sign-on"} {
		if !strings.Contains(page, want) {
			t.Errorf("organization page lacks %q", want)
		}
	}
	if strings.Contains(page, s.idp.ClientSecret) {
		t.Fatal("the organization page shows the client secret")
	}
	// Requiring it before signing in through it is refused.
	if status, body := alice.post("/ui/orgs/acme/sso", url.Values{"issuer": {s.idp.Issuer}, "client_id": {s.idp.ClientID}, "domains": {"acme.test"}, "require": {"yes"}}, s.url); status != http.StatusBadRequest || !strings.Contains(body, "sign in through acme") {
		t.Fatalf("require first = %d", status)
	}
	s.idp.SetUser(idpUser("alice-sub", "alice@acme.test"))
	if status, body := s.ssoSignIn(alice, "org=acme&next=/ui/orgs/acme"); status != http.StatusOK || !strings.Contains(body, "url=/ui/orgs/acme") {
		t.Fatalf("alice connects = %d %s", status, body)
	}
	if status, body := alice.post("/ui/orgs/acme/sso", url.Values{"issuer": {s.idp.Issuer}, "client_id": {s.idp.ClientID}, "domains": {"acme.test"}, "require": {"yes"}}, s.url); status != http.StatusSeeOther {
		t.Fatalf("require = %d %s", status, body)
	}

	// Bob, signed in with a token, is asked to use single sign-on.
	bob := newBrowser(t, s.url)
	bob.signIn(s.bobToken)
	status, _, body := bob.get(s.url + "/ui/repo?name=acme/api")
	if status != http.StatusForbidden || !strings.Contains(body, "acme requires single sign-on") || !strings.Contains(body, "Sign in with acme single sign-on") ||
		!strings.Contains(body, `name="next" value="/ui/repo?name=acme/api"`) {
		t.Fatalf("bob's repository page = %d\n%s", status, body)
	}
	// The CLI says the same, and what to run.
	bobCLI := &user{h: &harness{t: t, server: s.url, home: t.TempDir()}, label: "bob", dir: t.TempDir(),
		env: map[string]string{"VX_SERVER": s.url, "VX_TOKEN": s.bobToken, "VX_CONFIG": t.TempDir() + "/c.json"}}
	if out, code := bobCLI.run("effort", "list", "--repo", "acme/api"); code != 1 || !strings.Contains(out, "organization acme requires single sign-on") || !strings.Contains(out, "vx login --server "+s.url) {
		t.Fatalf("bob's CLI = %d %s", code, out)
	}

	// Bob's first single sign-on without a session: his email has an
	// account, so vx asks him to sign in to it.
	fresh := newBrowser(t, s.url)
	s.idp.SetUser(idpUser("bob-sub", "bob@acme.test"))
	status, body = s.ssoSignIn(fresh, "who=bob@acme.test")
	if status != http.StatusConflict || !strings.Contains(body, "This looks like you") || !strings.Contains(body, "user:bob") ||
		!strings.Contains(body, "/ui/login?next="+url.QueryEscape("/ui/login/sso?org=acme&next=%2Fui")) {
		t.Fatalf("bob without a session = %d\n%s", status, body)
	}
	if people, _ := s.svc.Store.ListPeople(context.Background()); len(people) != 2 {
		t.Fatalf("a second account was made: %d people", len(people))
	}
	// He follows the link: signs in with his token, then connects.
	fresh.signIn(s.bobToken)
	if status, body := s.ssoSignIn(fresh, "org=acme&next=/ui"); status != http.StatusOK || !strings.Contains(body, "Signed in") {
		t.Fatalf("bob connects = %d %s", status, body)
	}
	if status, _, _ := fresh.get(s.url + "/ui/repo?name=acme/api"); status != http.StatusOK {
		t.Fatalf("bob's repository page after single sign-on = %d", status)
	}

	// vx login through single sign-on: the CLI's token works for acme.
	home := t.TempDir()
	open := func(link string) error {
		b := newBrowser(t, s.url)
		_, loc, _ := b.get(link)
		next, _ := url.ParseQuery(strings.TrimPrefix(loc, "/ui/login?"))
		if status, body := s.ssoSignIn(b, "who=bob@acme.test&next="+url.QueryEscape(next.Get("next"))); status != http.StatusOK || !strings.Contains(body, "url=/ui/cli/approve") {
			t.Fatalf("single sign-on from the approval page = %d %s", status, body)
		}
		code := strings.TrimPrefix(next.Get("next"), "/ui/cli/approve?code=")
		if status, body := b.post("/ui/cli/approve", url.Values{"code": {code}, "decision": {"approve"}}, s.url); status != http.StatusOK || !strings.Contains(body, "is authorized") {
			t.Fatalf("approve = %d %s", status, body)
		}
		return nil
	}
	var out, errb bytes.Buffer
	if code := loginApp(home, &out, &errb, open).Run(context.Background(), []string{"login", "--server", s.url}); code != 0 {
		t.Fatalf("vx login = %d %s %s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := loginApp(home, &out, &errb, nil).Run(context.Background(), []string{"effort", "list", "--repo", "acme/api"}); code != 0 {
		t.Fatalf("effort list after single sign-on login = %d %s %s", code, out.String(), errb.String())
	}

	// The login page offers single sign-on now, and a wrong domain says so.
	_, _, login := newBrowser(t, s.url).get(s.url + "/ui/login")
	if !strings.Contains(login, "Sign in with single sign-on") {
		t.Fatal("login page lacks single sign-on")
	}
	if status, _, body := newBrowser(t, s.url).get(s.url + "/ui/login/sso?who=x@other.test"); status != http.StatusNotFound || !strings.Contains(body, "no organization on this server signs in other.test addresses") {
		t.Fatalf("unknown domain = %d %s", status, body)
	}
	// A replayed callback finds no cookie.
	b := newBrowser(t, s.url)
	s.idp.SetUser(idpUser("carol-sub", "carol@acme.test"))
	_, loc, _ := b.get(s.url + "/ui/login/sso?org=acme")
	_, back, _ := b.get(loc)
	if status, _, _ := b.get(back); status != http.StatusOK {
		t.Fatalf("carol signs up = %d", status)
	}
	if status, _, body := b.get(back); status != http.StatusBadRequest || !strings.Contains(body, "Sign-in expired") {
		t.Fatalf("replayed callback = %d %s", status, body)
	}
}

// TestMergeAccountsWeb: a superadmin merges two accounts on Admin > People;
// the merged one is shown as disabled.
func TestMergeAccountsWeb(t *testing.T) {
	s := newSSOServer(t)
	ctx := context.Background()
	if _, _, err := s.svc.ConsoleSetSuperadmin(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	bob2, err := s.svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob2", Email: "bob@acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	bob2Token, _ := s.svc.Store.CreateToken(ctx, bob2.ID, "old laptop")
	b := newBrowser(t, s.url)
	b.signIn(s.aliceToken)
	if status, body := b.post("/ui/admin/people/merge", url.Values{"keep": {"bob"}, "merge": {"alice"}}, s.url); status != http.StatusForbidden || !strings.Contains(body, "cannot merge their own account") {
		t.Fatalf("merge self = %d %s", status, body)
	}
	if status, body := b.post("/ui/admin/people/merge", url.Values{"keep": {"bob"}, "merge": {"bob2"}}, s.url); status != http.StatusSeeOther {
		t.Fatalf("merge = %d %s", status, body)
	}
	_, _, page := b.get(s.url + "/ui/admin/people?ok=merged")
	if !strings.Contains(page, "Accounts merged") || !strings.Contains(page, "merged into bob, disabled") {
		t.Fatalf("people page:\n%s", page)
	}
	u := &user{h: &harness{t: t, server: s.url, home: t.TempDir()}, label: "bob2", dir: t.TempDir(),
		env: map[string]string{"VX_SERVER": s.url, "VX_TOKEN": bob2Token, "VX_CONFIG": t.TempDir() + "/c.json"}}
	if out := u.must("whoami"); out != "user:bob\n" {
		t.Fatalf("bob2's token after the merge = %q", out)
	}
	// Bob (not a superadmin) gets no merge page.
	other := newBrowser(t, s.url)
	other.signIn(s.bobToken)
	if status, _ := other.post("/ui/admin/people/merge", url.Values{"keep": {"bob"}, "merge": {"alice"}}, s.url); status != http.StatusNotFound {
		t.Fatalf("non-superadmin merge = %d", status)
	}
}
