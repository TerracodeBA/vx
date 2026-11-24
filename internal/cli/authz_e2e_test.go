package cli_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/mcp"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// authzServer is a server with the API, web UI and MCP endpoint, and two
// people in two organizations: alice in acme, bob in globex.
type authzServer struct {
	*harness
	svc        *service.Service
	alice, bob service.Actor
	tokens     map[string]string // handle -> token
}

func newAuthzServer(t *testing.T) *authzServer {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	blobs, err := blob.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, blobs, nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	mcp.Mount(mux, svc, log)
	if err := web.Mount(mux, svc, log); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := &authzServer{harness: &harness{t: t, server: srv.URL, home: t.TempDir(), st: st}, svc: svc, tokens: map[string]string{}}
	for _, handle := range []string{"alice", "bob"} {
		p, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: handle, DisplayName: handle, Email: handle + "@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		tok, err := st.CreateToken(ctx, p.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		a, _ := svc.ResolveActor(ctx, p)
		s.tokens[handle] = tok
		if handle == "alice" {
			s.alice = a
		} else {
			s.bob = a
		}
	}
	if _, err := svc.CreateOrg(ctx, s.alice, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateOrg(ctx, s.bob, "globex", "Globex"); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *authzServer) grantBob(role service.RepoRole) {
	s.t.Helper()
	if _, err := s.svc.GrantRepo(context.Background(), s.alice, "acme/shop", "user:bob", role); err != nil {
		s.t.Fatal(err)
	}
}

// agentToken creates an agent for a person through the CLI.
func agentToken(t *testing.T, u *user, name string) string {
	t.Helper()
	var resp struct{ Token string }
	if err := json.Unmarshal([]byte(u.must("agent", "add", name, "--json")), &resp); err != nil || resp.Token == "" {
		t.Fatalf("agent add %s: %v", name, err)
	}
	return resp.Token
}

// hidden reports whether a CLI or tool error says "not found" without
// mentioning permissions, so a repository's existence does not leak.
func hidden(out string) bool {
	low := strings.ToLower(out)
	return (strings.Contains(low, "not found") || strings.Contains(low, "not_found") || strings.Contains(low, "no effort")) &&
		!strings.Contains(low, "permission")
}

func TestAuthzCLI(t *testing.T) {
	s := newAuthzServer(t)
	work := t.TempDir()
	alice := s.user("alice", s.tokens["alice"], work)
	bob := s.user("bob", s.tokens["bob"], work)
	aliceAgent := s.user("alice-agent", agentToken(t, alice, "claude"), work)
	bobAgent := s.user("bob-agent", agentToken(t, bob, "claude"), work)

	alice.must("repo", "create", "acme/shop")
	bob.must("repo", "create", "globex/site")
	alice.must("effort", "new", "Checkout", "--repo", "acme/shop")
	aliceAgent.must("clone", "acme/shop", "checkout", "a")
	aliceAgent.dir = filepath.Join(work, "a")
	aliceAgent.must("new", "Add cart")
	aliceAgent.write("cart.go", "package shop\n")
	aliceAgent.must("land")

	// No access: acme/shop is not found, through every command.
	for _, u := range []*user{bob, bobAgent} {
		if out := u.must("repo", "list"); strings.Contains(out, "acme/shop") || !strings.Contains(out, "globex/site") {
			t.Errorf("[%s] repo list without access:\n%s", u.label, out)
		}
		for _, args := range [][]string{
			{"effort", "list", "--repo", "acme/shop"},
			{"effort", "show", "checkout", "--repo", "acme/shop"},
			{"effort", "new", "Steal", "--repo", "acme/shop"},
			{"clone", "acme/shop", "checkout", "b-" + u.label},
			{"log", "checkout", "--repo", "acme/shop"},
			{"board", "--repo", "acme/shop", "--effort", "checkout"},
			{"ship", "plan", "checkout", "--repo", "acme/shop"},
		} {
			out, code := u.run(args...)
			if code == 0 || !hidden(out) {
				t.Errorf("[%s] vx %s without access: exit %d, want a not-found error:\n%s", u.label, strings.Join(args, " "), code, out)
			}
		}
	}

	// Read: bob's agent can clone and look, but not start work.
	s.grantBob(service.RoleRead)
	if out := bob.must("repo", "list"); !strings.Contains(out, "acme/shop") || !strings.Contains(out, "acme") || !strings.Contains(out, "read") {
		t.Errorf("repo list with a read grant should show acme/shop, its org and the role:\n%s", out)
	}
	bobAgent.must("clone", "acme/shop", "checkout", "b")
	bobAgent.dir = filepath.Join(work, "b")
	if got := bobAgent.read("cart.go"); got != "package shop\n" {
		t.Fatalf("cart.go = %q", got)
	}
	bobAgent.must("log")
	if out, code := bobAgent.run("new", "Change the cart"); code == 0 || !strings.Contains(out, "needs write") {
		t.Errorf("vx new with read only: exit %d, want a permission error:\n%s", code, out)
	}

	// Write: bob's agent lands, but cannot ship.
	s.grantBob(service.RoleWrite)
	bobAgent.must("new", "Change the cart")
	bobAgent.write("cart.go", "package shop\n\n// Cart holds items.\n")
	bobAgent.must("land")
	if out, code := bobAgent.run("ship", "plan"); code == 0 || !strings.Contains(out, "needs admin") {
		t.Errorf("ship plan with write: exit %d, want a permission error:\n%s", code, out)
	}

	// Admin: the ship plan gets past the access check (acme/shop has no
	// upstream to ship to).
	s.grantBob(service.RoleAdmin)
	if out, code := bobAgent.run("ship", "plan"); code == 0 || !strings.Contains(out, "no git upstream") {
		t.Errorf("ship plan with admin: exit %d, want the no-upstream error:\n%s", code, out)
	}
}

func TestAuthzWebAndMCP(t *testing.T) {
	s := newAuthzServer(t)
	ctx := context.Background()
	if _, err := s.svc.CreateRepo(ctx, s.alice, service.CreateRepoInput{Name: "acme/shop"}); err != nil {
		t.Fatal(err)
	}
	eff, err := s.svc.CreateEffort(ctx, s.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Checkout"})
	if err != nil {
		t.Fatal(err)
	}
	bobAgent := agentToken(t, s.user("bob", s.tokens["bob"], t.TempDir()), "claude")

	// Web UI, signed in as bob.
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	login := url.Values{"token": {s.tokens["bob"]}, "next": {"/ui"}}
	req, _ := http.NewRequest("POST", s.server+"/ui/login", strings.NewReader(login.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.server)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := c.Get(s.server + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// (The empty home page's example command mentions acme/shop-api.)
	if code, body := get("/ui"); code != 200 || strings.Contains(body, ">acme/shop<") || strings.Contains(body, "Checkout") {
		i := max(strings.Index(body, ">acme/shop<"), strings.Index(body, "Checkout"))
		t.Errorf("/ui for bob: status %d, shows acme's work: ...%s...", code, body[max(0, i-300):min(len(body), i+100)])
	}
	pages := []string{
		"/ui/repo?name=acme%2Fshop",
		"/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug,
		"/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug + "&view=board",
		"/ui/change?repo=acme%2Fshop&ref=" + eff.Base.String(),
		"/ui/ship?repo=acme%2Fshop&slug=" + eff.Slug,
		"/ui/effort/head?repo=acme%2Fshop&slug=" + eff.Slug,
	}
	for _, p := range pages {
		if code, body := get(p); code != http.StatusNotFound || strings.Contains(strings.ToLower(body), "permission") {
			t.Errorf("%s for bob: status %d, want 404 without mentioning permissions", p, code)
		}
	}

	// MCP, as bob's agent.
	client := sdk.NewClient(&sdk.Implementation{Name: "vx-test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             s.server + mcp.Path,
		HTTPClient:           &http.Client{Transport: bearerRT{token: bobAgent}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	tool := func(name string, args map[string]any) (bool, string) {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var parts []string
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				parts = append(parts, tc.Text)
			}
		}
		if res.StructuredContent != nil {
			b, _ := json.Marshal(res.StructuredContent)
			parts = append(parts, string(b))
		}
		return res.IsError, strings.Join(parts, "\n")
	}
	if isErr, out := tool("vx_efforts", map[string]any{}); isErr || strings.Contains(out, "acme/shop") {
		t.Errorf("vx_efforts for bob's agent lists acme/shop (error %v):\n%s", isErr, out)
	}
	repoEffort := map[string]any{"repo": "acme/shop", "effort": eff.Slug}
	for name, args := range map[string]map[string]any{
		"vx_efforts": {"repo": "acme/shop"},
		"vx_status":  repoEffort,
		"vx_log":     repoEffort,
		"vx_show":    {"repo": "acme/shop", "ref": eff.Base.String()},
		"vx_board":   repoEffort,
		"vx_context": {"repo": "acme/shop", "effort": eff.Slug, "targets": []string{"cart.go"}},
	} {
		if isErr, out := tool(name, args); !isErr || !hidden(out) {
			t.Errorf("%s for bob's agent on acme/shop: error %v, want not found:\n%s", name, isErr, out)
		}
	}

	// A read grant opens the same pages and tools.
	s.grantBob(service.RoleRead)
	if code, body := get(pages[1]); code != 200 || !strings.Contains(body, "Checkout") {
		t.Errorf("effort page with a read grant: status %d", code)
	}
	if _, body := get("/ui"); !strings.Contains(body, ">acme/shop<") || !strings.Contains(body, "Checkout") {
		t.Error("/ui with a read grant should list acme/shop and its effort")
	}
	if isErr, out := tool("vx_status", repoEffort); isErr || !strings.Contains(out, "Checkout") {
		t.Errorf("vx_status with a read grant: error %v:\n%s", isErr, out)
	}
	if isErr, out := tool("vx_efforts", map[string]any{}); isErr || !strings.Contains(out, `"my_role":"read"`) {
		t.Errorf("vx_efforts with a read grant should list acme/shop as read:\n%s", out)
	}
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
