package web_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// addPerson adds a signed-in browser for a new person.
func (e *adminEnv) addPerson(handle string, superadmin bool) service.Actor {
	e.t.Helper()
	p, err := e.st.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: handle, SiteAdmin: superadmin})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.svc.ResolveActor(e.ctx, p)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.st.CreateToken(e.ctx, p.ID, "web")
	if err != nil {
		e.t.Fatal(err)
	}
	e.browsers[handle] = e.signIn(tok)
	return a
}

// TestSuperadminWebNoRepoAccess: a superadmin who is not in acme runs the
// admin panel but gets 404 for acme/shop and its content in the web UI,
// until acme/shop's admin gives support access on the access page; ending
// it makes the repository 404 again.
func TestSuperadminWebNoRepoAccess(t *testing.T) {
	e := newAdminEnv(t)
	root := e.addPerson("root", true)
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: adminRepo, Name: "Checkout"})
	if err != nil {
		t.Fatal(err)
	}
	_, branches, err := e.svc.GetRepo(e.ctx, e.alice, adminRepo)
	if err != nil || len(branches) == 0 {
		t.Fatalf("branches: %v %v", branches, err)
	}
	head := branches[0].Head.String()
	repoPages := []string{
		"/ui/repo?name=" + adminRepo,
		"/ui/effort?repo=" + adminRepo + "&slug=" + eff.Slug,
		"/ui/change?repo=" + adminRepo + "&ref=" + head,
		"/ui/diff?repo=" + adminRepo + "&ref=" + head,
		"/ui/repos/" + adminRepo + "/access",
	}
	for _, p := range repoPages {
		if code, body := e.get("root", p); code != 404 || strings.Contains(body, "Checkout") {
			t.Errorf("superadmin GET %s = %d, want 404", p, code)
		}
	}
	if _, body := e.get("root", "/ui/repos"); !strings.Contains(body, "This account cannot read any repositories.") || strings.Contains(body, "name="+adminRepo) {
		t.Error("the superadmin's repository list shows acme/shop")
	}
	// The organization is visible, read-only, without its repositories.
	if code, body := e.get("root", "/ui/orgs/acme"); code != 200 || strings.Contains(body, "Add member") || strings.Contains(body, adminRepo) {
		t.Errorf("superadmin GET /ui/orgs/acme = %d: offers changes or lists acme/shop", code)
	}
	if resp, body := e.post("root", "/ui/orgs/acme/members", url.Values{"person": {"root"}, "role": {"owner"}}, ""); resp.StatusCode != 403 || !strings.Contains(body, "superadmin") {
		t.Errorf("superadmin adding themselves to acme = %d\n%.300s", resp.StatusCode, body)
	}
	// The admin panel works.
	for _, p := range []string{"/ui/admin/people", "/ui/admin/github"} {
		if code, _ := e.get("root", p); code != 200 {
			t.Errorf("superadmin GET %s = %d", p, code)
		}
	}

	// Support access from the access page (acme/shop's admin).
	resp, body := e.post("alice", "/ui/repos/"+adminRepo+"/access/support", url.Values{"person": {"root"}, "role": {"read"}, "for": {"72h"}, "reason": {"ticket 4711"}}, "")
	if resp.StatusCode != 200 || !strings.Contains(body, "Support access granted.") || !strings.Contains(body, "operator (support access)") || !strings.Contains(body, "ticket 4711") {
		t.Fatalf("grant support = %d\n%.600s", resp.StatusCode, body)
	}
	// A person who is not a superadmin gets a role instead.
	if resp, body := e.post("alice", "/ui/repos/"+adminRepo+"/access/support", url.Values{"person": {"carol"}, "role": {"read"}, "for": {"24h"}}, ""); resp.StatusCode != 400 || !strings.Contains(body, "not a superadmin") {
		t.Errorf("support for a non-superadmin = %d\n%.300s", resp.StatusCode, body)
	}
	if code, body := e.get("root", "/ui/repo?name="+adminRepo); code != 200 || !strings.Contains(body, "Checkout") {
		t.Errorf("superadmin with support access GET repo = %d", code)
	}
	if code, _ := e.get("root", "/ui/repos/"+adminRepo+"/access"); code != 403 {
		t.Errorf("support access is never admin: access page = %d, want 403", code)
	}
	_, entries, err := e.svc.RepoAccess(e.ctx, e.alice, adminRepo)
	if err != nil {
		t.Fatal(err)
	}
	var via string
	for _, en := range entries {
		if en.Person.ID == root.ID {
			via = string(en.Role) + " " + strings.Join(en.Via, ",")
		}
	}
	if !strings.HasPrefix(via, "read operator (support access) until ") {
		t.Errorf("root on the access list = %q", via)
	}
	resp, body = e.post("alice", "/ui/repos/"+adminRepo+"/access/support/revoke", url.Values{"person": {"root"}}, "")
	if resp.StatusCode != 200 || !strings.Contains(body, "Support access ended.") {
		t.Fatalf("end support = %d\n%.300s", resp.StatusCode, body)
	}
	if code, _ := e.get("root", "/ui/repo?name="+adminRepo); code != 404 {
		t.Errorf("after the support access ended GET repo = %d, want 404", code)
	}
}

// TestSuperadminReviewBanner: after an upgrade the admin panel shows the
// review banner to superadmins until one marks it reviewed.
func TestSuperadminReviewBanner(t *testing.T) {
	e := newAdminEnv(t)
	const banner = "site admins no longer have access to repositories; grant support access per repository if needed"
	if _, body := e.get("alice", "/ui/admin/people"); strings.Contains(body, banner) {
		t.Fatal("a new server shows the upgrade banner")
	}
	if err := e.st.AddServerNotice(e.ctx, service.NoticeSuperadminReview); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/ui/admin/people", "/ui/admin/github"} {
		if _, body := e.get("alice", p); !strings.Contains(body, banner) || !strings.Contains(body, "Mark as reviewed") {
			t.Errorf("GET %s lacks the review banner", p)
		}
	}
	if resp, _ := e.post("bob", "/ui/admin/review", url.Values{}, ""); resp.StatusCode != 404 {
		t.Errorf("non-superadmin marks reviewed = %d", resp.StatusCode)
	}
	if resp, _ := e.post("agent", "/ui/admin/review", url.Values{}, ""); resp.StatusCode != 403 {
		t.Errorf("agent marks reviewed = %d", resp.StatusCode)
	}
	resp, body := e.post("alice", "/ui/admin/review", url.Values{}, "")
	if resp.StatusCode != 200 || !strings.Contains(body, "Marked as reviewed.") || strings.Contains(body, banner) {
		t.Fatalf("mark reviewed = %d\n%.400s", resp.StatusCode, body)
	}
	ops, err := e.st.ListActorOps(e.ctx, e.alice.ID, 5)
	if err != nil || len(ops) == 0 || ops[0].Op != "server.notice.resolve" {
		t.Errorf("op log = %+v %v", ops, err)
	}
}
