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

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

func TestSettingsPage(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	alice, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice Liddell", Email: "alice@example.com"})
	bob, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	aliceActor, _ := svc.ResolveActor(ctx, alice)
	bobActor, _ := svc.ResolveActor(ctx, bob)
	_, agentSecret, err := svc.CreateAgent(ctx, aliceActor, "backend", "")
	if err != nil {
		t.Fatal(err)
	}
	_, bobAgentSecret, _ := svc.CreateAgent(ctx, bobActor, "helper", "")
	_, agentTokenID, _ := st.AuthenticateToken(ctx, agentSecret)
	_, bobTokenID, _ := st.AuthenticateToken(ctx, bobAgentSecret)
	webToken, _ := st.CreateToken(ctx, alice.ID, "browser <b>")

	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	read := func(resp *http.Response, err error) (int, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(path string, form url.Values, origin string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin == "" {
			origin = srv.URL
		}
		req.Header.Set("Origin", origin)
		return read(c.Do(req))
	}
	if _, err := postForm(c, srv.URL, "/ui/login", url.Values{"token": {webToken}, "next": {"/ui"}}); err != nil {
		t.Fatal(err)
	}

	// The sidebar links to settings from every page.
	if _, home := read(c.Get(srv.URL + "/ui")); !strings.Contains(home, `href="/ui/settings"`) {
		t.Fatal("sidebar has no Settings link")
	}
	resp, err := c.Get(srv.URL + "/ui/settings")
	if err != nil {
		t.Fatal(err)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	code, page := read(resp, nil)
	if code != 200 {
		t.Fatalf("settings = %d\n%s", code, page)
	}
	for _, want := range []string{"Alice Liddell", "alice@example.com", "alice/backend", "this browser", "browser &lt;b&gt;", agentTokenID.String()[:8], "Revoke"} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	for _, bad := range []string{"bob/helper", "<b>", "vxt_", " style=", "<script>"} {
		if strings.Contains(page, bad) {
			t.Errorf("settings page contains %q", bad)
		}
	}

	// Revoke is two steps: the first POST only asks.
	code, confirm := post("/ui/settings/revoke", url.Values{"id": {agentTokenID.String()}}, srv.URL)
	if code != 200 || !strings.Contains(confirm, "Revoke this token?") || !strings.Contains(confirm, `name="confirm" value="yes"`) {
		t.Fatalf("confirm page = %d\n%s", code, confirm)
	}
	if tok, _ := st.TokenByID(ctx, agentTokenID); !tok.Active() {
		t.Fatal("the first step revoked the token")
	}
	// Cross-origin posts are refused.
	if code, _ := post("/ui/settings/revoke", url.Values{"id": {agentTokenID.String()}, "confirm": {"yes"}}, "http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin revoke = %d", code)
	}
	if tok, _ := st.TokenByID(ctx, agentTokenID); !tok.Active() {
		t.Fatal("cross-origin post revoked the token")
	}
	code, done := post("/ui/settings/revoke", url.Values{"id": {agentTokenID.String()}, "confirm": {"yes"}}, srv.URL)
	if code != 200 || !strings.Contains(done, "Revoked token") {
		t.Fatalf("revoke = %d\n%s", code, done)
	}
	if tok, _ := st.TokenByID(ctx, agentTokenID); tok.Active() {
		t.Fatal("token not revoked")
	}
	if !regexp.MustCompile(`class="row-revoked"`).MatchString(done) {
		t.Error("revoked token not marked on the page")
	}

	// Someone else's agent: refused at both steps.
	for _, form := range []url.Values{{"id": {bobTokenID.String()}}, {"id": {bobTokenID.String()}, "confirm": {"yes"}}} {
		if code, _ := post("/ui/settings/revoke", form, ""); code != http.StatusForbidden {
			t.Fatalf("revoking bob's token = %d", code)
		}
	}
	if tok, _ := st.TokenByID(ctx, bobTokenID); !tok.Active() {
		t.Fatal("bob's token was revoked")
	}
	if code, _ := post("/ui/settings/revoke", url.Values{"id": {uuid.NewString()}}, ""); code != http.StatusNotFound {
		t.Fatalf("missing token = %d", code)
	}

	// The token this browser uses: the confirm page warns, a confirm without
	// the current flag is refused, and with it she is signed out.
	_, webTokenID, _ := st.AuthenticateToken(ctx, webToken)
	if _, confirm := post("/ui/settings/revoke", url.Values{"id": {webTokenID.String()}}, ""); !strings.Contains(confirm, "Revoke and sign out") {
		t.Fatalf("current-token confirm page:\n%s", confirm)
	}
	if code, _ := post("/ui/settings/revoke", url.Values{"id": {webTokenID.String()}, "confirm": {"yes"}}, ""); code != http.StatusBadRequest {
		t.Fatalf("revoking the current token without the flag = %d", code)
	}
	code, after := post("/ui/settings/revoke", url.Values{"id": {webTokenID.String()}, "confirm": {"yes"}, "current": {"yes"}}, "")
	if code != 200 || !strings.Contains(after, "Paste a vx token") {
		t.Fatalf("after revoking the current token = %d\n%s", code, after)
	}
	if _, page := read(c.Get(srv.URL + "/ui/settings")); !strings.Contains(page, "Paste a vx token") {
		t.Fatal("still signed in after revoking the session token")
	}
}
