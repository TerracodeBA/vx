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

// TestSettingsAgentScope: the Settings page shows each agent's scope and
// budgets and edits them with a same-origin POST form; the agent itself
// sees its scope but has no form.
func TestSettingsAgentScope(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	alice, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	bob, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	aliceActor, _ := svc.ResolveActor(ctx, alice)
	agent, agentSecret, err := svc.CreateAgent(ctx, aliceActor, "backend", "")
	if err != nil {
		t.Fatal(err)
	}
	aliceToken, _ := st.CreateToken(ctx, alice.ID, "browser")
	bobToken, _ := st.CreateToken(ctx, bob.ID, "browser")

	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	signIn := func(token string) *http.Client {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		if _, err := postForm(c, srv.URL, "/ui/login", url.Values{"token": {token}, "next": {"/ui"}}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	get := func(c *http.Client, path string) string {
		t.Helper()
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d\n%s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	post := func(c *http.Client, form url.Values, origin string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/ui/settings/scope", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	scope := func() store.AgentScope {
		sc, err := st.AgentScope(ctx, agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sc
	}

	ac := signIn(aliceToken)
	page := get(ac, "/ui/settings")
	for _, want := range []string{`id="agent-backend"`, "Scope and budgets", `action="/ui/settings/scope"`, "every path", "(no limit)"} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	if strings.Contains(page, "<script>") || strings.Contains(page, " style=") {
		t.Error("settings page has inline script or style")
	}

	form := url.Values{"agent": {"alice/backend"}, "allow": {"internal/api/**  docs/api.md"}, "deny": {"internal/api/gen/**"},
		"max_lands_per_day": {"20"}, "max_lines": {"400"}, "max_files": {""}, "max_workspaces": {"2"}}
	if code, _ := post(ac, form, "http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin scope post = %d", code)
	}
	if !scope().Unlimited() {
		t.Fatal("a cross-origin post changed the scope")
	}
	code, done := post(ac, form, srv.URL)
	if code != 200 || !strings.Contains(done, "Saved the scope of") {
		t.Fatalf("scope post = %d\n%s", code, done)
	}
	sc := scope()
	if strings.Join(sc.Allow, ",") != "internal/api/**,docs/api.md" || strings.Join(sc.Deny, ",") != "internal/api/gen/**" ||
		sc.MaxLandsPerDay != 20 || sc.MaxLines != 400 || sc.MaxFiles != 0 || sc.MaxWorkspaces != 2 {
		t.Fatalf("stored scope = %+v", sc)
	}
	for _, want := range []string{`value="internal/api/** docs/api.md"`, "0 of 20", "0 of 2", "at most 400 changed lines", "Remove all limits"} {
		if !strings.Contains(done, want) {
			t.Errorf("page after saving misses %q", want)
		}
	}
	// A bad glob or number says what is wrong; nothing changes.
	if code, body := post(ac, url.Values{"agent": {"backend"}, "allow": {"/abs"}}, srv.URL); code != http.StatusBadRequest || !strings.Contains(body, "starts with /") {
		t.Fatalf("bad glob = %d\n%s", code, body)
	}
	if code, body := post(ac, url.Values{"agent": {"backend"}, "max_lines": {"lots"}}, srv.URL); code != http.StatusBadRequest || !strings.Contains(body, "not a whole number") {
		t.Fatalf("bad number = %d\n%s", code, body)
	}
	if scope().MaxLines != 400 {
		t.Fatal("a refused post changed the scope")
	}

	// Someone else cannot.
	bc := signIn(bobToken)
	if code, _ := post(bc, url.Values{"agent": {"alice/backend"}, "clear": {"yes"}}, srv.URL); code != http.StatusForbidden {
		t.Fatalf("bob clears alice's agent = %d", code)
	}
	// The agent sees its own scope, without a form, and cannot post.
	gc := signIn(agentSecret)
	own := get(gc, "/ui/settings")
	if !strings.Contains(own, "Your scope and budgets") || !strings.Contains(own, "internal/api/**") || strings.Contains(own, `action="/ui/settings/scope"`) {
		t.Errorf("agent's settings page:\n%s", own)
	}
	if code, _ := post(gc, url.Values{"agent": {"backend"}, "clear": {"yes"}}, srv.URL); code != http.StatusForbidden {
		t.Fatalf("agent clears its own scope = %d", code)
	}
	// Remove all limits.
	if code, _ := post(ac, url.Values{"agent": {"backend"}, "clear": {"yes"}}, srv.URL); code != 200 || !scope().Unlimited() {
		t.Fatalf("clear = %d, scope %+v", code, scope())
	}
}
