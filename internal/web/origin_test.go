package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// postForm posts a form the way a browser does from a page of base: with
// an Origin header naming it.
func postForm(c *http.Client, base, path string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", base)
	return c.Do(req)
}

type webEnv struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	svc   *service.Service
	srv   *httptest.Server
	c     *http.Client
	alice service.Actor
}

func newWebEnv(t *testing.T) *webEnv {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	alice, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	aliceActor, _ := svc.ResolveActor(ctx, alice)
	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &webEnv{t: t, ctx: ctx, st: st, svc: svc, srv: srv, c: &http.Client{Jar: jar}, alice: aliceActor}
}

// post sends a form with the given headers (Origin, Referer) and returns the
// status and body.
func (e *webEnv) post(path string, form url.Values, headers map[string]string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *webEnv) signIn(token string) {
	e.t.Helper()
	if code, body := e.post("/ui/login", url.Values{"token": {token}, "next": {"/ui/settings"}}, map[string]string{"Origin": e.srv.URL}); code != 200 || strings.Contains(body, "Paste a vx token") {
		e.t.Fatalf("sign in = %d\n%s", code, body)
	}
}

// Every state-changing form refuses posts that do not come from this site,
// whether they carry a foreign Origin, a foreign Referer, or neither.
func TestFormsRefuseCrossOrigin(t *testing.T) {
	e := newWebEnv(t)
	token, _ := e.st.CreateToken(e.ctx, e.alice.ID, "web")
	tokID := func() string {
		_, toks, _ := e.svc.ListTokens(e.ctx, e.alice, "")
		return toks[len(toks)-1].ID.String()
	}
	e.signIn(token)

	refused := []map[string]string{
		{},
		{"Origin": "http://evil.example"},
		{"Origin": "null"},
		{"Referer": "http://evil.example/ui/settings"},
		{"Origin": "http://evil.example", "Referer": e.srv.URL + "/ui/settings"}, // Origin wins
	}
	forms := map[string]url.Values{
		"/ui/login":           {"token": {token}},
		"/ui/logout":          {},
		"/ui/ship":            {"repo": {"nope"}, "slug": {"nope"}},
		"/ui/settings/revoke": {"id": {tokID()}, "confirm": {"yes"}},
		"/ui/settings/token":  {"confirm": {"yes"}, "name": {"csrf"}},
	}
	for path, form := range forms {
		for _, h := range refused {
			if code, _ := e.post(path, form, h); code != http.StatusForbidden {
				t.Errorf("POST %s with %v = %d, want 403", path, h, code)
			}
		}
	}
	_, toks, _ := e.svc.ListTokens(e.ctx, e.alice, "")
	for _, tk := range toks {
		if !tk.Active() || tk.Name == "csrf" {
			t.Fatalf("a refused post changed tokens: %+v", tk)
		}
	}
	// Still signed in: the refused logouts did nothing.
	if code, body := e.post("/ui/settings/token", url.Values{}, map[string]string{"Referer": e.srv.URL + "/ui/settings"}); code != 200 || !strings.Contains(body, "Create a token for") {
		t.Fatalf("same-site Referer without Origin = %d\n%s", code, body)
	}
	// A same-site logout signs out.
	if code, body := e.post("/ui/logout", url.Values{}, map[string]string{"Origin": e.srv.URL}); code != 200 || !strings.Contains(body, "Paste a vx token") {
		t.Fatalf("logout = %d", code)
	}
}

func TestCreateTokenPage(t *testing.T) {
	e := newWebEnv(t)
	_, agentSecret, err := e.svc.CreateAgent(e.ctx, e.alice, "backend", "")
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := e.st.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	bobActor, _ := e.svc.ResolveActor(e.ctx, bob)
	if _, _, err := e.svc.CreateAgent(e.ctx, bobActor, "helper", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := e.st.CreateToken(e.ctx, e.alice.ID, "web")
	e.signIn(token)
	same := map[string]string{"Origin": e.srv.URL}

	resp, err := e.c.Get(e.srv.URL + "/ui/settings")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	if n := strings.Count(page, `action="/ui/settings/token"`); n != 2 {
		t.Fatalf("settings page has %d Create token forms, want 2 (hers and the agent's)", n)
	}
	if !strings.Contains(page, `name="agent" value="alice/backend"`) {
		t.Fatal("no Create token form for the agent")
	}

	// Step one asks for a label and creates nothing.
	code, ask := e.post("/ui/settings/token", url.Values{"agent": {"alice/backend"}}, same)
	if code != 200 || !strings.Contains(ask, "Create a token for") || !strings.Contains(ask, `name="confirm" value="yes"`) || strings.Contains(ask, "vxt_") {
		t.Fatalf("step one = %d\n%s", code, ask)
	}
	if _, toks, _ := e.svc.ListTokens(e.ctx, e.alice, "backend"); len(toks) != 1 {
		t.Fatalf("step one created a token: %d", len(toks))
	}

	// Step two creates it and shows the secret once, uncached.
	req, _ := http.NewRequest("POST", e.srv.URL+"/ui/settings/token",
		strings.NewReader(url.Values{"agent": {"alice/backend"}, "name": {"ci <x>"}, "confirm": {"yes"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	resp, err = e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	done := string(b)
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("step two = %d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	i := strings.Index(done, "vxt_")
	if i < 0 || !strings.Contains(done, "only time vx shows it") || !strings.Contains(done, "ci &lt;x&gt;") {
		t.Fatalf("result page:\n%s", done)
	}
	secret := done[i : i+strings.IndexAny(done[i:], "<\n ")]
	if p, err := e.st.Authenticate(e.ctx, secret); err != nil || p.Handle != "alice/backend" {
		t.Fatalf("new secret authenticates as %v, %v", p.Handle, err)
	}
	for _, bad := range []string{" style=", "<script>"} {
		if strings.Contains(done, bad) {
			t.Errorf("result page contains %q", bad)
		}
	}
	// The secret is not on the settings page afterwards.
	resp, _ = e.c.Get(e.srv.URL + "/ui/settings")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), secret) {
		t.Fatal("settings page shows the secret again")
	}
	// For herself, with the default label.
	if code, done := e.post("/ui/settings/token", url.Values{"confirm": {"yes"}}, same); code != 200 || !strings.Contains(done, "created by user:alice") {
		t.Fatalf("own token = %d\n%s", code, done)
	}

	// Not for Bob's agent, at either step.
	for _, form := range []url.Values{{"agent": {"bob/helper"}}, {"agent": {"bob/helper"}, "confirm": {"yes"}}} {
		if code, _ := e.post("/ui/settings/token", form, same); code != http.StatusForbidden {
			t.Fatalf("token for bob's agent = %d", code)
		}
	}
	// An agent signed in to the web UI sees no Create token button and is refused.
	agentWeb := newWebEnvShared(e)
	agentWeb.signIn(agentSecret)
	resp, _ = agentWeb.c.Get(e.srv.URL + "/ui/settings")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), `action="/ui/settings/token"`) {
		t.Fatal("agent sees a Create token button")
	}
	if code, body := agentWeb.post("/ui/settings/token", url.Values{"confirm": {"yes"}}, same); code != http.StatusForbidden || !strings.Contains(body, "only people can create tokens") {
		t.Fatalf("agent creating a token = %d\n%s", code, body)
	}
}

// newWebEnvShared is another browser on the same server.
func newWebEnvShared(e *webEnv) *webEnv {
	jar, _ := cookiejar.New(nil)
	other := *e
	other.c = &http.Client{Jar: jar}
	return &other
}

// The session cookie is named after the server's port, so two servers on
// one host keep separate sessions, and it keeps its flags.
func TestSessionCookiePerServer(t *testing.T) {
	e := newWebEnv(t)
	token, _ := e.st.CreateToken(e.ctx, e.alice.ID, "web")
	resp, err := postForm(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		e.srv.URL, "/ui/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, _ := url.Parse(e.srv.URL)
	want := "vx_token_" + u.Port()
	var got *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == want {
			got = c
		}
		if c.Name == "vx_token" {
			t.Error("the shared cookie name vx_token is still set")
		}
	}
	if got == nil || !got.HttpOnly || got.SameSite != http.SameSiteStrictMode || got.Secure {
		t.Fatalf("cookie = %+v, want %s HttpOnly SameSite=Strict", got, want)
	}
	// A cookie under the old name, or another port's, does not sign in.
	for _, name := range []string{"vx_token", "vx_token_1"} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/ui/settings", nil)
		req.AddCookie(&http.Cookie{Name: name, Value: token})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), "Paste a vx token") {
			t.Errorf("cookie %s signed in", name)
		}
	}
	// Behind an HTTPS proxy the cookie is Secure.
	req, _ := http.NewRequest("POST", e.srv.URL+"/ui/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err = (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cs := resp.Cookies(); len(cs) != 1 || !cs[0].Secure {
		t.Fatalf("cookies behind https = %+v", cs)
	}
}
