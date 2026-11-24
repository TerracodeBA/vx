package service_test

// GitHub-backed repositories: roles come from GitHub, through the fake in
// internal/github/githubtest. On GitHub, alice is an admin of the
// organization acme, bob a member with read on acme/api and nothing on
// acme/secret, and carol is not in acme. acme/api is public, so GitHub
// gives everyone read on it; vx only counts that for acme's members.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

type ghEnv struct {
	*env
	gh              *githubtest.Server
	now             time.Time
	inst            int64
	acmeGH          githubtest.Account
	org             service.Org
	api, secret     store.Repo
	bob, carol      service.Actor
	bobAgent        service.Actor
	ghUsers         map[string]githubtest.Account
	apiGH, secretGH githubtest.Repo
}

// linkGitHub links a vx person to a GitHub user, as sign-in does.
func linkGitHub(e *env, a service.Actor, acct githubtest.Account) {
	e.t.Helper()
	if err := e.svc.Store.LinkGitHub(e.ctx, a.ID, acct.ID, acct.Login); err != nil {
		e.t.Fatal(err)
	}
}

// backWithGitHub links an existing vx repository to a GitHub repository of
// an installation, as installation sync does.
func backWithGitHub(e *env, r store.Repo, ghRepo githubtest.Repo, inst int64) store.Repo {
	e.t.Helper()
	if err := e.svc.Store.SetRepoGitHub(e.ctx, r.ID, &ghRepo.ID, &inst); err != nil {
		e.t.Fatal(err)
	}
	out, err := e.svc.Store.RepoByID(e.ctx, r.ID)
	if err != nil || !out.GitHubBacked() {
		e.t.Fatalf("repo %s not GitHub-backed: %v", r.Name, err)
	}
	return out
}

// installOn records an installation of the App on a GitHub organization,
// linked to the vx organization org.
func installOn(e *env, gh *githubtest.Server, acct githubtest.Account, org service.Org) int64 {
	e.t.Helper()
	id := gh.Install(acct.Login)
	if err := e.svc.Store.UpsertGitHubInstallation(e.ctx, store.GitHubInstallationRow{
		ID: id, AccountID: acct.ID, AccountLogin: acct.Login, AccountType: "Organization", OrgID: &org.ID, RepositorySelection: "all",
	}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.svc.Store.SetOrgGitHub(e.ctx, org.ID, &acct.ID, acct.Login); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func newGHEnv(t *testing.T) *ghEnv {
	t.Helper()
	e := &ghEnv{env: newEnv(t), gh: githubtest.New(t), now: time.Now()}
	e.svc.Now = func() time.Time { return e.now }
	ctx := e.ctx
	e.ghUsers = map[string]githubtest.Account{}
	for _, l := range []string{"alice", "bob", "carol"} {
		e.ghUsers[l] = e.gh.AddUser(l)
	}
	e.acmeGH = e.gh.AddOrg("acme")
	e.gh.SetOrgMember("acme", "alice", "admin")
	e.gh.SetOrgMember("acme", "bob", "member")
	e.apiGH = e.gh.AddRepo("acme/api", false)
	e.secretGH = e.gh.AddRepo("acme/secret", true)
	e.gh.SetPermission("acme/api", "bob", "read")

	e.bob, e.carol = e.person("bob"), e.person("carol")
	e.bobAgent = e.agentOf(e.bob, "claude")
	var err error
	if e.org, err = e.svc.CreateOrg(ctx, e.alice, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	e.inst = installOn(e.env, e.gh, e.acmeGH, e.org)
	e.svc.GitHub.API = e.gh.Client(t)
	for _, name := range []string{"acme/api", "acme/secret"} {
		r, err := e.svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if name == "acme/api" {
			e.api = backWithGitHub(e.env, r, e.apiGH, e.inst)
		} else {
			e.secret = backWithGitHub(e.env, r, e.secretGH, e.inst)
		}
	}
	linkGitHub(e.env, e.alice, e.ghUsers["alice"])
	linkGitHub(e.env, e.bob, e.ghUsers["bob"])
	linkGitHub(e.env, e.carol, e.ghUsers["carol"])
	return e
}

func (e *ghEnv) listed(a service.Actor) map[string]service.RepoRole {
	e.t.Helper()
	repos, err := e.svc.ListRepos(e.ctx, a)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]service.RepoRole{}
	for _, r := range repos {
		out[r.Name] = r.Role
	}
	ids, all, err := e.svc.ReadableRepoIDs(e.ctx, a)
	if err != nil {
		e.t.Fatal(err)
	}
	for name, id := range map[string]store.Repo{"acme/api": e.api, "acme/secret": e.secret} {
		_, inList := out[name]
		if inIDs := all || slices.Contains(ids, id.ID); inIDs != inList {
			e.t.Errorf("%s: ListRepos has %s = %v but ReadableRepoIDs = %v", a.Ref(), name, inList, inIDs)
		}
	}
	return out
}

func (e *ghEnv) want(a service.Actor, repo string, want service.RepoRole) {
	e.t.Helper()
	if got := e.roleOf(a, repo); got != want {
		e.t.Errorf("%s on %s: role %q, want %q", a.Ref(), repo, got, want)
	}
}

func (e *ghEnv) drop(scope service.GitHubRoleScope) {
	e.t.Helper()
	if err := e.svc.DropGitHubRoles(e.ctx, scope); err != nil {
		e.t.Fatal(err)
	}
}

// TestGitHubRolesWhoSeesWhat checks roles and lists for everyone.
func TestGitHubRolesWhoSeesWhat(t *testing.T) {
	e := newGHEnv(t)
	cases := []struct {
		a           service.Actor
		api, secret service.RepoRole
	}{
		{e.alice, service.RoleAdmin, service.RoleAdmin},
		{e.agent, service.RoleAdmin, service.RoleAdmin},
		{e.bob, service.RoleRead, service.RoleNone},
		{e.bobAgent, service.RoleRead, service.RoleNone},
		{e.carol, service.RoleNone, service.RoleNone}, // acme/api is public, but carol is not in acme
	}
	for _, c := range cases {
		e.want(c.a, "acme/api", c.api)
		e.want(c.a, "acme/secret", c.secret)
		got := e.listed(c.a)
		if got["acme/api"] != c.api || got["acme/secret"] != c.secret {
			t.Errorf("%s lists %v, want acme/api %q and acme/secret %q", c.a.Ref(), got, c.api, c.secret)
		}
	}
	// A person without a GitHub link has no role, even as owner of the vx
	// organization; a site admin keeps admin without one.
	dave := e.person("dave")
	root := e.person("root")
	if err := e.svc.Store.SetSiteAdmin(e.ctx, root.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Store.SetOrgMember(e.ctx, e.org.ID, dave.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	e.want(dave, "acme/api", service.RoleNone)
	e.want(root, "acme/secret", service.RoleAdmin)
	if got := e.listed(dave); len(got) != 0 {
		t.Errorf("dave (no GitHub link) lists %v", got)
	}
	// Inbox and efforts follow the same rule.
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/secret", Name: "Rotate keys"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := e.svc.Ask(e.ctx, e.agent, service.AskInput{Repo: "acme/secret", Effort: eff.Slug, Targets: []string{"keys.go"}, Body: "Who rotates?", To: "user:bob"})
	if err != nil {
		t.Fatal(err)
	}
	in, err := e.svc.GetInbox(e.ctx, e.bob)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(in.Questions, func(n service.Note) bool { return n.ID == q.ID }) {
		t.Error("bob's inbox shows a question in acme/secret, where GitHub gives him nothing")
	}
	if _, _, err := e.svc.ListEfforts(e.ctx, e.bob, "acme/secret", ""); code(err) != service.CodeNotFound {
		t.Errorf("bob lists efforts of acme/secret: %v", err)
	}
}

// TestGitHubRolesLandAndShip checks who may land and ship.
func TestGitHubRolesLandAndShip(t *testing.T) {
	e := newGHEnv(t)
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/api", Name: "Rate limits"})
	if err != nil {
		t.Fatal(err)
	}
	land := func(a service.Actor) error {
		ws, err := e.svc.OpenWorkspace(e.ctx, a, "acme/api", eff.Slug, "Try")
		if err != nil {
			return err
		}
		cur, _ := e.svc.Store.EffortByID(e.ctx, eff.ID)
		tree := e.snapshot(e.api, cur.Head, map[string]*string{fmt.Sprintf("%s.go", a.Handle[:3]): s("package api\n")})
		_, err = e.svc.Land(e.ctx, a, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
		return err
	}
	ship := func(a service.Actor) error {
		_, err := e.svc.PlanShipment(e.ctx, a, "acme/api", eff.Slug, "")
		if code(err) == service.CodeFailedPrecondition {
			return nil // past the access check
		}
		return err
	}
	if err := land(e.agent); err != nil {
		t.Fatalf("alice's agent lands: %v", err)
	}
	if err := land(e.bobAgent); code(err) != service.CodePermission {
		t.Errorf("bob's agent with read lands: %v", err)
	}
	if err := land(e.carol); code(err) != service.CodeNotFound {
		t.Errorf("carol lands: %v", err)
	}
	if err := ship(e.alice); err != nil {
		t.Errorf("alice ships: %v", err)
	}
	if err := ship(e.bob); code(err) != service.CodePermission {
		t.Errorf("bob ships with read: %v", err)
	}

	// bob gets write on GitHub; the webhook drops the cache.
	e.gh.SetPermission("acme/api", "bob", "write")
	e.drop(service.GitHubRoleScope{GitHubRepoID: e.apiGH.ID, Reason: "member edited"})
	if err := land(e.bobAgent); err != nil {
		t.Errorf("bob's agent with write on GitHub lands: %v", err)
	}
	if err := ship(e.bob); code(err) != service.CodePermission {
		t.Errorf("bob ships with write: %v", err)
	}
	// The permission error says where roles come from.
	var se *service.Error
	if err := ship(e.bob); !errors.As(err, &se) || !strings.Contains(se.Msg, "come from GitHub") || !strings.Contains(se.Msg, "github.com/acme/api/settings/access") {
		t.Errorf("permission error does not point to GitHub: %v", err)
	}
}

// TestGitHubRolesFollowChanges checks that GitHub changes apply at once
// after a webhook and after GitHubRoleTTL without one, and that the cache
// saves GitHub calls in between.
func TestGitHubRolesFollowChanges(t *testing.T) {
	e := newGHEnv(t)
	e.want(e.bob, "acme/api", service.RoleRead)
	e.listed(e.bob) // asks about acme/secret once
	calls := e.gh.Calls("GET /repos/*") + e.gh.Calls("GET /orgs/*")
	for range 3 {
		e.want(e.bob, "acme/api", service.RoleRead)
		e.want(e.bobAgent, "acme/secret", service.RoleNone)
		e.listed(e.bob)
	}
	if n := e.gh.Calls("GET /repos/*") + e.gh.Calls("GET /orgs/*") - calls; n != 0 {
		t.Errorf("cached roles still asked GitHub %d times", n)
	}

	// Without a webhook: the old role until the TTL passes.
	e.gh.SetPermission("acme/api", "bob", "write")
	e.want(e.bob, "acme/api", service.RoleRead)
	e.now = e.now.Add(service.GitHubRoleTTL + time.Second)
	e.want(e.bob, "acme/api", service.RoleWrite)
	e.want(e.bobAgent, "acme/api", service.RoleWrite)

	// bob is removed from acme: the organization webhook applies it at once.
	// His write grant stays (outside collaborator), but read from a public
	// repository no longer counts.
	e.gh.SetOrgMember("acme", "bob", "")
	e.gh.SetPermission("acme/api", "bob", "none")
	if err := e.svc.ApplyGitHubOrgMembership(e.ctx, e.acmeGH.ID, e.ghUsers["bob"].ID, ""); err != nil {
		t.Fatal(err)
	}
	e.want(e.bob, "acme/api", service.RoleNone)
	if got := e.listed(e.bobAgent); len(got) != 0 {
		t.Errorf("bob's agent after leaving acme lists %v", got)
	}
	if r, _ := e.svc.Store.OrgRole(e.ctx, e.org.ID, e.bob.ID); r != "" {
		t.Errorf("bob is still %q in the vx organization acme", r)
	}

	// Back in acme, then acme/api goes private to a team bob is not in.
	e.gh.SetOrgMember("acme", "bob", "member")
	if err := e.svc.ApplyGitHubOrgMembership(e.ctx, e.acmeGH.ID, e.ghUsers["bob"].ID, "member"); err != nil {
		t.Fatal(err)
	}
	e.want(e.bob, "acme/api", service.RoleRead) // public: members read
	if r, _ := e.svc.Store.OrgRole(e.ctx, e.org.ID, e.bob.ID); r != "member" {
		t.Errorf("bob back in acme is %q in the vx organization", r)
	}
	e.gh.SetPrivate("acme/api", true)
	e.gh.SetPermission("acme/api", "carol", "none")
	e.drop(service.GitHubRoleScope{GitHubRepoID: e.apiGH.ID, Reason: "repository privatized"})
	e.want(e.bob, "acme/api", service.RoleNone)
	e.want(e.alice, "acme/api", service.RoleAdmin)
}

// TestGitHubRolesIgnoreVXGrants checks that vx grants, teams and org
// defaults give nothing on GitHub-backed repositories and cannot be made.
func TestGitHubRolesIgnoreVXGrants(t *testing.T) {
	e := newGHEnv(t)
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, "acme/secret", "user:bob", service.RoleWrite); code(err) != service.CodeFailedPrecondition || !strings.Contains(err.Error(), "github.com/acme/secret/settings/access") {
		t.Errorf("granting bob on a GitHub-backed repository: %v", err)
	}
	// A grant written before the repository was linked has no effect.
	if _, err := e.svc.Store.SetRepoGrant(e.ctx, e.secret.ID, &e.bob.ID, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	read := service.RoleRead
	if _, err := e.svc.UpdateOrg(e.ctx, e.alice, "acme", "", &read); err != nil {
		t.Fatal(err)
	}
	e.want(e.bob, "acme/secret", service.RoleNone)
	e.want(e.carol, "acme/secret", service.RoleNone)
	if got := e.listed(e.bob); got["acme/secret"] != "" {
		t.Errorf("bob lists acme/secret through a vx grant: %v", got)
	}
	if err := e.svc.RevokeRepo(e.ctx, e.alice, "acme/secret", "user:bob"); err != nil {
		t.Errorf("revoking an old grant: %v", err)
	}
	// Members and teams of an organization linked to GitHub are GitHub's.
	if _, err := e.svc.SetMember(e.ctx, e.alice, "acme", "carol", service.OrgMember); code(err) != service.CodeFailedPrecondition || !strings.Contains(err.Error(), "github.com/orgs/acme/people") {
		t.Errorf("adding carol to a GitHub-linked organization: %v", err)
	}
	if _, err := e.svc.CreateTeam(e.ctx, e.alice, "acme", "backend"); code(err) != service.CodeFailedPrecondition {
		t.Errorf("creating a team in a GitHub-linked organization: %v", err)
	}
	// Service principals keep direct grants (GitHub does not know them).
	ci, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindService, Handle: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	ciActor, _ := e.svc.ResolveActor(e.ctx, ci)
	e.want(ciActor, "acme/secret", service.RoleNone)
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, "acme/secret", "service:ci", service.RoleRead); err != nil {
		t.Fatalf("granting a service principal: %v", err)
	}
	e.want(ciActor, "acme/secret", service.RoleRead)
	if got := e.listed(ciActor); got["acme/secret"] != service.RoleRead || got["acme/api"] != "" {
		t.Errorf("service:ci lists %v", got)
	}
	// Access shows GitHub's roles.
	_, entries, err := e.svc.RepoAccess(e.ctx, e.alice, "acme/secret")
	if err != nil {
		t.Fatal(err)
	}
	via := map[string]string{}
	for _, en := range entries {
		via[en.Person.Handle] = fmt.Sprint(en.Role, en.Via)
	}
	if via["alice"] != "admin[GitHub]" || via["ci"] != "read[service:ci]" || via["bob"] != "" {
		t.Errorf("access entries = %v", via)
	}
}

// TestGitHubRolesWhenGitHubIsDown checks the fallbacks: a recent cached
// answer works, an old one or none fails closed with a retryable error.
func TestGitHubRolesWhenGitHubIsDown(t *testing.T) {
	e := newGHEnv(t)
	e.want(e.bob, "acme/api", service.RoleRead)
	e.listed(e.bob)
	e.gh.Fail("GET /repos/*", 502, 1000)
	e.gh.Fail("GET /orgs/*", 502, 1000)

	e.now = e.now.Add(service.GitHubRoleTTL + time.Minute)
	e.want(e.bob, "acme/api", service.RoleRead) // within the hour
	if got := e.listed(e.bob); got["acme/api"] != service.RoleRead {
		t.Errorf("bob's list with GitHub down and a recent cache: %v", got)
	}
	// Never checked: no access, and a clear error.
	_, _, err := e.svc.Authorize(e.ctx, e.carol, "acme/api", service.RoleRead)
	if code(err) != service.CodeUnavailable || !strings.Contains(err.Error(), "try again") {
		t.Errorf("carol with GitHub down: %v", err)
	}
	// Too old: no access.
	e.now = e.now.Add(time.Hour)
	if _, _, err := e.svc.Authorize(e.ctx, e.bob, "acme/api", service.RoleRead); code(err) != service.CodeUnavailable {
		t.Errorf("bob with GitHub down and a cache over an hour old: %v", err)
	}
	if got := e.listed(e.bobAgent); len(got) != 0 {
		t.Errorf("bob's agent lists %v with GitHub down and an old cache", got)
	}
	// Site admins do not need GitHub.
	root := e.person("root")
	if err := e.svc.Store.SetSiteAdmin(e.ctx, root.ID, true); err != nil {
		t.Fatal(err)
	}
	e.want(root, "acme/api", service.RoleAdmin)
}

// TestGitHubRolesInstallation checks suspension, removal and invalidation
// by installation and person, and SyncGitHubOrgs on sign-in.
func TestGitHubRolesInstallation(t *testing.T) {
	e := newGHEnv(t)
	e.want(e.bob, "acme/api", service.RoleRead)
	if err := e.svc.DropGitHubRoles(e.ctx, service.GitHubRoleScope{}); code(err) != service.CodeInvalid {
		t.Errorf("empty scope: %v", err)
	}
	now := e.now
	if err := e.svc.Store.UpsertGitHubInstallation(e.ctx, store.GitHubInstallationRow{
		ID: e.inst, AccountID: e.acmeGH.ID, AccountLogin: "acme", AccountType: "Organization", OrgID: &e.org.ID, SuspendedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	e.drop(service.GitHubRoleScope{InstallationID: e.inst, Reason: "installation suspended"})
	e.want(e.bob, "acme/api", service.RoleNone)
	e.want(e.alice, "acme/api", service.RoleNone)
	if err := e.svc.Store.UpsertGitHubInstallation(e.ctx, store.GitHubInstallationRow{
		ID: e.inst, AccountID: e.acmeGH.ID, AccountLogin: "acme", AccountType: "Organization", OrgID: &e.org.ID,
	}); err != nil {
		t.Fatal(err)
	}
	e.want(e.bob, "acme/api", service.RoleRead)
	if err := e.svc.Store.DeleteGitHubInstallation(e.ctx, e.inst); err != nil {
		t.Fatal(err)
	}
	e.drop(service.GitHubRoleScope{InstallationID: e.inst, Reason: "installation deleted"})
	e.want(e.alice, "acme/secret", service.RoleNone)
	if got := e.listed(e.alice); len(got) != 0 {
		t.Errorf("alice lists %v after the installation was removed", got)
	}
	e.inst = installOn(e.env, e.gh, e.acmeGH, e.org)
	for _, r := range []store.Repo{e.api, e.secret} {
		gid := *r.GitHubRepoID
		if err := e.svc.Store.SetRepoGitHub(e.ctx, r.ID, &gid, &e.inst); err != nil {
			t.Fatal(err)
		}
	}

	// Sign-in: membership in the linked vx organization follows GitHub.
	if err := e.svc.SyncGitHubOrgs(e.ctx, e.bob.Principal); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SyncGitHubOrgs(e.ctx, e.carol.Principal); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SyncGitHubOrgs(e.ctx, e.alice.Principal); err != nil {
		t.Fatal(err)
	}
	for who, want := range map[service.Actor]string{e.bob: "member", e.carol: "", e.alice: "owner"} {
		if r, _ := e.svc.Store.OrgRole(e.ctx, e.org.ID, who.ID); r != want {
			t.Errorf("%s in vx acme after sign-in: %q, want %q", who.Ref(), r, want)
		}
	}
	mine, err := e.svc.MyGitHubInstallations(e.ctx, e.bobAgent)
	if err != nil || len(mine) != 1 || mine[0].ID != e.inst {
		t.Errorf("bob's installations = %+v %v", mine, err)
	}
	if mine, err := e.svc.MyGitHubInstallations(e.ctx, e.carol); err != nil || len(mine) != 0 {
		t.Errorf("carol's installations = %+v %v", mine, err)
	}

	// Person removed: everything cached for them goes.
	e.gh.SetPermission("acme/api", "bob", "write")
	e.drop(service.GitHubRoleScope{GitHubUserID: e.ghUsers["bob"].ID, Reason: "person removed"})
	e.want(e.bob, "acme/api", service.RoleWrite)
	// Organization scope (team changes).
	e.gh.SetPermission("acme/secret", "bob", "triage")
	e.drop(service.GitHubRoleScope{GitHubOrgID: e.acmeGH.ID, Reason: "team changed"})
	e.want(e.bob, "acme/secret", service.RoleRead)
}
