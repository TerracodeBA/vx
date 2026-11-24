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

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// adminEnv is a server with an organization acme and a repository in it,
// and one signed-in browser per person: alice (site admin, owner of
// acme), bob (member of acme), carol (in no organization) and alice's
// agent.
type adminEnv struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	svc *service.Service
	srv *httptest.Server

	alice, bob, carol service.Actor
	browsers          map[string]*http.Client // alice, bob, carol, agent
}

const adminRepo = "acme/shop"

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e := &adminEnv{t: t, ctx: ctx, st: st, svc: svc, srv: srv, browsers: map[string]*http.Client{}}

	person := func(handle, name string, siteAdmin bool) service.Actor {
		t.Helper()
		p, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: handle, DisplayName: name, SiteAdmin: siteAdmin})
		if err != nil {
			t.Fatal(err)
		}
		a, err := svc.ResolveActor(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := st.CreateToken(ctx, p.ID, "web")
		if err != nil {
			t.Fatal(err)
		}
		e.browsers[handle] = e.signIn(tok)
		return a
	}
	e.alice = person("alice", "Alice Liddell", true)
	e.bob = person("bob", "Bob <b>Builder</b>", false)
	e.carol = person("carol", "Carol", false)
	_, agentTok, err := svc.CreateAgent(ctx, e.alice, "backend", "")
	if err != nil {
		t.Fatal(err)
	}
	e.browsers["agent"] = e.signIn(agentTok)

	if _, err := svc.CreateOrg(ctx, e.alice, "acme", "Acme <script>alert(1)</script>"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetMember(ctx, e.alice, "acme", "bob", service.OrgMember); err != nil {
		t.Fatal(err)
	}
	read := service.RoleRead
	if _, err := svc.UpdateOrg(ctx, e.alice, "acme", "", &read); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: adminRepo, Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *adminEnv) signIn(token string) *http.Client {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := postForm(c, e.srv.URL, "/ui/login", url.Values{"token": {token}, "next": {"/ui"}})
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(b), "Paste a vx token") {
		e.t.Fatalf("sign in = %d\n%s", resp.StatusCode, b)
	}
	return c
}

func (e *adminEnv) get(who, path string) (int, string) {
	e.t.Helper()
	resp, err := e.browsers[who].Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// post sends a form as who's browser. origin "" means this site.
func (e *adminEnv) post(who, path string, form url.Values, origin string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin == "" {
		origin = e.srv.URL
	}
	req.Header.Set("Origin", origin)
	resp, err := e.browsers[who].Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *adminEnv) members() map[string]service.OrgRole {
	e.t.Helper()
	d, err := e.svc.GetOrg(e.ctx, e.alice, "acme")
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]service.OrgRole{}
	for _, m := range d.Members {
		out[m.Person.Handle] = m.Role
	}
	return out
}

// Each admin page renders for the people allowed to see it, read-only for
// those who may see but not change, and is 404 (or 403) for others.
func TestAdminPagesByRole(t *testing.T) {
	e := newAdminEnv(t)
	type want struct {
		status   int
		has, not []string
	}
	cases := []struct {
		who, path string
		want      want
	}{
		// Everyone gets the Organizations entry; only site admins get Admin.
		{"alice", "/ui", want{200, []string{`href="/ui/orgs"`, `href="/ui/admin/people"`}, nil}},
		{"bob", "/ui", want{200, []string{`href="/ui/orgs"`}, []string{`href="/ui/admin/people"`}}},
		{"carol", "/ui", want{200, []string{`href="/ui/orgs"`}, []string{`href="/ui/admin/people"`}}},

		{"alice", "/ui/orgs", want{200, []string{`href="/ui/orgs/acme"`, "owner", "New organization"}, nil}},
		{"bob", "/ui/orgs", want{200, []string{`href="/ui/orgs/acme"`, "member"}, nil}},
		{"carol", "/ui/orgs", want{200, []string{`href="/ui/orgs/carol"`, "personal"}, []string{`href="/ui/orgs/acme"`}}},
		{"agent", "/ui/orgs", want{200, []string{`href="/ui/orgs/acme"`}, []string{"New organization"}}},

		{"alice", "/ui/orgs/acme", want{200, []string{"Change role", "Add member", "Create team", "Save settings", `href="/ui/repos/acme/shop/access"`}, nil}},
		{"bob", "/ui/orgs/acme", want{200, []string{"Bob", "acme/shop", "read-only"}, []string{"Change role", "Add member", "Create team", "Save settings", "/access"}}},
		{"agent", "/ui/orgs/acme", want{200, []string{"read-only"}, []string{"Change role", "Add member", "<form method=\"post\" action=\"/ui/orgs/acme"}}},
		{"carol", "/ui/orgs/acme", want{404, nil, []string{"Acme"}}},
		{"alice", "/ui/orgs/nope", want{404, nil, nil}},

		{"alice", "/ui/repos/acme/shop/access", want{200, []string{"Direct grants", "Everyone with access", "Grant", "alice", "bob"}, nil}},
		{"bob", "/ui/repos/acme/shop/access", want{403, nil, []string{"Direct grants"}}},
		{"carol", "/ui/repos/acme/shop/access", want{404, nil, []string{"Direct grants"}}},
		{"alice", "/ui/repos/acme/nope/access", want{404, nil, nil}},
		{"alice", "/ui/repos/acme/shop/other", want{404, nil, nil}},

		// The repository page links to access for repo admins only.
		{"alice", "/ui/repo?name=acme/shop", want{200, []string{`href="/ui/repos/acme/shop/access"`}, nil}},
		{"bob", "/ui/repo?name=acme/shop", want{200, nil, []string{"/access"}}},

		{"alice", "/ui/admin/people", want{200, []string{"Add person", "alice", "bob", "carol", "site admin", "acme (owner)", "acme (member)"}, nil}},
		{"bob", "/ui/admin/people", want{404, nil, []string{"Add person"}}},
		{"carol", "/ui/admin/people", want{404, nil, []string{"Add person"}}},
	}
	for _, c := range cases {
		code, body := e.get(c.who, c.path)
		if code != c.want.status {
			t.Errorf("%s GET %s = %d, want %d\n%.400s", c.who, c.path, code, c.want.status, body)
			continue
		}
		for _, s := range c.want.has {
			if !strings.Contains(body, s) {
				t.Errorf("%s GET %s: missing %q", c.who, c.path, s)
			}
		}
		for _, s := range c.want.not {
			if strings.Contains(body, s) {
				t.Errorf("%s GET %s: should not contain %q", c.who, c.path, s)
			}
		}
		if strings.Contains(body, "<script>alert") || strings.Contains(body, "<b>Builder") || strings.Contains(body, " style=") {
			t.Errorf("%s GET %s: unescaped HTML or inline style on the page", c.who, c.path)
		}
	}
}

// Every admin form refuses posts from other sites and from agents' sessions,
// and neither changes anything.
func TestAdminFormsRefuseCrossOriginAndAgents(t *testing.T) {
	e := newAdminEnv(t)
	forms := map[string]url.Values{
		"/ui/orgs":                           {"handle": {"evil"}},
		"/ui/orgs/acme/settings":             {"display_name": {"pwned"}, "default_role": {"write"}},
		"/ui/orgs/acme/members":              {"person": {"carol"}, "role": {"owner"}},
		"/ui/orgs/acme/members/remove":       {"person": {"bob"}},
		"/ui/orgs/acme/teams":                {"name": {"evil"}},
		"/ui/orgs/acme/teams/delete":         {"team": {"core"}},
		"/ui/orgs/acme/teams/members":        {"team": {"core"}, "person": {"bob"}},
		"/ui/orgs/acme/teams/members/remove": {"team": {"core"}, "person": {"alice"}},
		"/ui/repos/acme/shop/access/grant":   {"kind": {"user"}, "subject": {"carol"}, "role": {"admin"}},
		"/ui/repos/acme/shop/access/revoke":  {"subject": {"user:bob"}},
		"/ui/admin/people":                   {"handle": {"mallory"}, "site_admin": {"on"}},
	}
	if _, err := e.svc.CreateTeam(e.ctx, e.alice, "acme", "core"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetTeamMember(e.ctx, e.alice, "acme", "core", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, adminRepo, "user:bob", service.RoleWrite); err != nil {
		t.Fatal(err)
	}
	for path, form := range forms {
		if resp, _ := e.post("alice", path, form, "http://evil.example"); resp.StatusCode != http.StatusForbidden {
			t.Errorf("cross-origin POST %s = %d, want 403", path, resp.StatusCode)
		}
		if resp, body := e.post("agent", path, form, ""); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "agents cannot") {
			t.Errorf("agent POST %s = %d, want 403\n%.300s", path, resp.StatusCode, body)
		}
	}
	d, err := e.svc.GetOrg(e.ctx, e.alice, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if d.Org.DisplayName == "pwned" || d.Org.DefaultRepoRole != service.RoleRead || len(d.Teams) != 1 || len(d.Teams[0].Members) != 1 {
		t.Errorf("a refused post changed the org: %+v teams %+v", d.Org, d.Teams)
	}
	if m := e.members(); len(m) != 2 || m["bob"] != service.OrgMember {
		t.Errorf("a refused post changed members: %v", m)
	}
	grants, _, err := e.svc.RepoAccess(e.ctx, e.alice, adminRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Subject != "user:bob" {
		t.Errorf("a refused post changed grants: %+v", grants)
	}
	people, _ := e.svc.ListPeople(e.ctx, e.alice)
	for _, p := range people {
		if p.Handle == "mallory" {
			t.Error("a refused post created a person")
		}
	}
	orgs, _, err := e.svc.ListOrgs(e.ctx, e.alice, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orgs {
		if o.Handle == "evil" {
			t.Error("a refused post created an org")
		}
	}

	// Members (not owners or admins) are refused by the service, and see why.
	resp, body := e.post("bob", "/ui/orgs/acme/members", url.Values{"person": {"carol"}, "role": {"member"}}, "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `class="note bad"`) {
		t.Errorf("member adding a member = %d\n%.500s", resp.StatusCode, body)
	}
	// Strangers get 404, not a hint that acme exists.
	if resp, _ := e.post("carol", "/ui/orgs/acme/teams", url.Values{"name": {"x"}}, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("stranger creating a team = %d, want 404", resp.StatusCode)
	}
	if resp, _ := e.post("carol", "/ui/admin/people", url.Values{"handle": {"mallory"}}, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-admin adding a person = %d, want 404", resp.StatusCode)
	}
}

// Adding a person shows their first token once: on the response to the
// form, never in a URL, and never again.
func TestAdminAddPersonShowsTokenOnce(t *testing.T) {
	e := newAdminEnv(t)
	resp, body := e.post("alice", "/ui/admin/people", url.Values{
		"handle": {"dave"}, "display_name": {"Dave <i>D</i>"}, "email": {"dave@example.com"}, "org": {"acme"},
	}, "")
	if resp.StatusCode != 200 {
		t.Fatalf("add person = %d\n%s", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	m := regexp.MustCompile(`<pre class="code token-secret">([^<]+)</pre>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no token on the page:\n%s", body)
	}
	secret := m[1]
	for _, want := range []string{"vx login --server " + e.srv.URL + " --token " + secret, "vx ui", "privately", "Dave &lt;i&gt;D&lt;/i&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("new person page missing %q", want)
		}
	}
	p, err := e.st.Authenticate(e.ctx, secret)
	if err != nil || p.Handle != "dave" {
		t.Fatalf("the shown token does not sign in as dave: %v %+v", err, p)
	}
	if m := e.members(); m["dave"] != service.OrgMember {
		t.Errorf("dave not added to acme: %v", m)
	}
	// Never again: not on the people page, not on a reload of the list.
	code, list := e.get("alice", "/ui/admin/people")
	if code != 200 || !strings.Contains(list, "dave") || strings.Contains(list, secret) {
		t.Errorf("people page after adding = %d, has dave %v, has secret %v", code, strings.Contains(list, "dave"), strings.Contains(list, secret))
	}
	if strings.Contains(resp.Request.URL.String(), secret) {
		t.Error("the secret is in a URL")
	}
	// Adding the same handle again is a message on the page, not a 500.
	resp, body = e.post("alice", "/ui/admin/people", url.Values{"handle": {"dave"}}, "")
	if resp.StatusCode == 200 || resp.StatusCode >= 500 || !strings.Contains(body, `class="note bad"`) || strings.Contains(body, "token-secret") {
		t.Errorf("adding dave twice = %d\n%.500s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `value="dave"`) {
		t.Error("the form lost what was typed")
	}
}

// Changes made through the forms show up on the pages.
func TestAdminMembershipChangesShow(t *testing.T) {
	e := newAdminEnv(t)
	step := func(path string, form url.Values) string {
		t.Helper()
		resp, body := e.post("alice", path, form, "")
		if resp.StatusCode != 200 || strings.Contains(body, `class="note bad"`) {
			t.Fatalf("POST %s %v = %d\n%s", path, form, resp.StatusCode, body)
		}
		return body
	}
	// Add carol, then make bob an admin.
	page := step("/ui/orgs/acme/members", url.Values{"person": {"carol"}, "role": {"member"}})
	if !strings.Contains(page, "Membership saved.") || !strings.Contains(page, "Carol") {
		t.Errorf("after adding carol:\n%s", page)
	}
	step("/ui/orgs/acme/members", url.Values{"person": {"bob"}, "role": {"admin"}})
	if m := e.members(); m["bob"] != service.OrgAdmin || m["carol"] != service.OrgMember {
		t.Fatalf("members = %v", m)
	}
	// Bob now manages the org.
	if _, page := e.get("bob", "/ui/orgs/acme"); !strings.Contains(page, "Change role") {
		t.Error("bob, now an admin, does not get the member forms")
	}
	// Carol now sees acme.
	if code, _ := e.get("carol", "/ui/orgs/acme"); code != 200 {
		t.Errorf("carol, now a member, gets %d", code)
	}

	// Teams: create, add carol, see her in it, remove her, delete it.
	step("/ui/orgs/acme/teams", url.Values{"name": {"backend"}})
	page = step("/ui/orgs/acme/teams/members", url.Values{"team": {"backend"}, "person": {"carol"}})
	if !regexp.MustCompile(`(?s)team:acme/backend.*carol`).MatchString(page) {
		t.Errorf("carol not shown in team backend:\n%s", page)
	}
	// Grant the team write; carol's effective role says why.
	page = step("/ui/repos/acme/shop/access/grant", url.Values{"kind": {"team"}, "subject": {"acme/backend"}, "role": {"write"}})
	if !strings.Contains(page, "Access granted.") || !strings.Contains(page, "acme/backend") {
		t.Errorf("after granting:\n%s", page)
	}
	_, entries, err := e.svc.RepoAccess(e.ctx, e.alice, adminRepo)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if en.Person.Handle == "carol" && en.Role != service.RoleWrite {
			t.Errorf("carol's role = %s via %v", en.Role, en.Via)
		}
	}
	page = step("/ui/repos/acme/shop/access/revoke", url.Values{"subject": {"team:acme/backend"}})
	if !strings.Contains(page, "Access revoked.") {
		t.Error("no note after revoking")
	}
	step("/ui/orgs/acme/teams/members/remove", url.Values{"team": {"backend"}, "person": {"carol"}})
	page = step("/ui/orgs/acme/teams/delete", url.Values{"team": {"backend"}})
	if strings.Contains(page, "team:acme/backend") {
		t.Error("deleted team still shown")
	}
	// Settings.
	page = step("/ui/orgs/acme/settings", url.Values{"display_name": {"Acme & Co"}, "default_role": {""}})
	if !strings.Contains(page, "Acme &amp; Co") {
		t.Error("new display name not shown")
	}
	if d, _ := e.svc.GetOrg(e.ctx, e.alice, "acme"); d.Org.DefaultRepoRole != service.RoleNone {
		t.Errorf("default role = %q, want none", d.Org.DefaultRepoRole)
	}
	// Remove carol: she no longer sees acme.
	step("/ui/orgs/acme/members/remove", url.Values{"person": {"carol"}})
	if code, _ := e.get("carol", "/ui/orgs/acme"); code != 404 {
		t.Errorf("carol after removal gets %d, want 404", code)
	}
	// A new organization from the list page lands on its page, owned by bob.
	resp, page := e.post("bob", "/ui/orgs", url.Values{"handle": {"bobco"}, "display_name": {"Bob Co"}}, "")
	if resp.StatusCode != 200 || !strings.Contains(page, "Organization created.") || !strings.Contains(page, "you are owner") {
		t.Errorf("new org = %d\n%.600s", resp.StatusCode, page)
	}
	// Service errors are messages on the page, not 500s.
	resp, page = e.post("alice", "/ui/orgs/acme/members", url.Values{"person": {"nobody"}, "role": {"member"}}, "")
	if resp.StatusCode >= 500 || resp.StatusCode == 200 || !strings.Contains(page, `class="note bad"`) || !strings.Contains(page, "Members") {
		t.Errorf("adding an unknown person = %d\n%.600s", resp.StatusCode, page)
	}
}
