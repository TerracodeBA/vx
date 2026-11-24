package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func (e *env) person(handle string) service.Actor {
	e.t.Helper()
	p, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: handle})
	if err != nil {
		e.t.Fatal(err)
	}
	a, _ := e.svc.ResolveActor(e.ctx, p)
	return a
}

func (e *env) agentOf(person service.Actor, name string) service.Actor {
	e.t.Helper()
	p, _, err := e.svc.CreateAgent(e.ctx, person, name, "")
	if err != nil {
		e.t.Fatal(err)
	}
	a, _ := e.svc.ResolveActor(e.ctx, p)
	return a
}

// roleOf is the actor's role on repo as Authorize sees it: RoleNone when
// it reports not found.
func (e *env) roleOf(a service.Actor, repo string) service.RepoRole {
	e.t.Helper()
	_, role, err := e.svc.Authorize(e.ctx, a, repo, service.RoleRead)
	if code(err) == service.CodeNotFound {
		return service.RoleNone
	}
	if err != nil {
		e.t.Fatalf("Authorize(%s, %s): %v", a.Ref(), repo, err)
	}
	return role
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// TestRepoRoleMatrix checks every way to get a role on a repository.
func TestRepoRoleMatrix(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	alice := e.alice
	bob, carol, dave, erin := e.person("bob"), e.person("carol"), e.person("dave"), e.person("erin")

	if _, err := svc.CreateOrg(ctx, alice, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	repo, err := svc.CreateRepo(ctx, alice, service.CreateRepoInput{Name: "acme/api"})
	if err != nil || repo.Org != "acme" {
		t.Fatalf("CreateRepo = %+v, %v", repo, err)
	}
	// Owner of the org: admin.
	if r := e.roleOf(alice, "acme/api"); r != service.RoleAdmin {
		t.Errorf("owner role = %q", r)
	}
	// Not a member: the repo is not found, with the same message as a
	// repo that does not exist.
	_, _, errHidden := svc.Authorize(ctx, bob, "acme/api", service.RoleRead)
	_, _, errMissing := svc.Authorize(ctx, bob, "acme/nope", service.RoleRead)
	if code(errHidden) != service.CodeNotFound || strings.ReplaceAll(errHidden.Error(), "acme/api", "X") != strings.ReplaceAll(errMissing.Error(), "acme/nope", "X") {
		t.Errorf("hidden %v vs missing %v", errHidden, errMissing)
	}
	if _, err := svc.AuthorizeID(ctx, bob, repo.ID, service.RoleRead); code(err) != service.CodeNotFound {
		t.Errorf("AuthorizeID hidden: %v", err)
	}

	// Member: the org default (read), and a permission error naming the
	// role needed and who can grant it.
	_, err = svc.SetMember(ctx, alice, "acme", "bob", service.OrgMember)
	e.must(err)
	if r := e.roleOf(bob, "acme/api"); r != service.RoleRead {
		t.Errorf("member role = %q", r)
	}
	_, _, err = svc.Authorize(ctx, bob, "acme/api", service.RoleWrite)
	if code(err) != service.CodePermission || !strings.Contains(err.Error(), "needs write") || !strings.Contains(err.Error(), "user:alice") ||
		!strings.Contains(err.Error(), "vx repo grant acme/api user:bob --role write") {
		t.Errorf("permission error = %v", err)
	}
	none := service.RoleNone
	_, err = svc.UpdateOrg(ctx, alice, "acme", "", &none)
	e.must(err)
	if r := e.roleOf(bob, "acme/api"); r != service.RoleNone {
		t.Errorf("member with default none = %q", r)
	}

	// Direct grant.
	_, err = svc.GrantRepo(ctx, alice, "acme/api", "bob", service.RoleWrite)
	e.must(err)
	if r := e.roleOf(bob, "acme/api"); r != service.RoleWrite {
		t.Errorf("granted role = %q", r)
	}

	// Team grant.
	_, err = svc.SetMember(ctx, alice, "acme", "user:carol", service.OrgMember)
	e.must(err)
	_, err = svc.CreateTeam(ctx, alice, "acme", "Backend")
	e.must(err)
	_, err = svc.SetTeamMember(ctx, alice, "acme", "backend", "carol")
	e.must(err)
	_, err = svc.GrantRepo(ctx, alice, "acme/api", "team:acme/backend", service.RoleWrite)
	e.must(err)
	if r := e.roleOf(carol, "acme/api"); r != service.RoleWrite {
		t.Errorf("team role = %q", r)
	}
	if _, err := svc.SetTeamMember(ctx, alice, "acme", "backend", "dave"); code(err) != service.CodeFailedPrecondition {
		t.Errorf("non-member into team: %v", err)
	}
	_, err = svc.RemoveTeamMember(ctx, alice, "acme", "backend", "carol")
	e.must(err)
	if r := e.roleOf(carol, "acme/api"); r != service.RoleNone {
		t.Errorf("after leaving the team = %q", r)
	}

	// Org admin: admin on every repo.
	_, err = svc.SetMember(ctx, alice, "acme", "dave", service.OrgAdmin)
	e.must(err)
	if r := e.roleOf(dave, "acme/api"); r != service.RoleAdmin {
		t.Errorf("org admin role = %q", r)
	}

	// Site admin: admin everywhere without being a member.
	e.must(svc.Store.SetSiteAdmin(ctx, erin.ID, true))
	if r := e.roleOf(erin, "acme/api"); r != service.RoleAdmin || !svc.IsSiteAdmin(ctx, erin) {
		t.Errorf("site admin role = %q", r)
	}
	if _, all, err := svc.ReadableRepoIDs(ctx, erin); err != nil || !all {
		t.Errorf("site admin readable: all=%v %v", all, err)
	}

	// Agents have their person's role and cannot manage anything.
	bobAgent := e.agentOf(bob, "helper")
	if r := e.roleOf(bobAgent, "acme/api"); r != service.RoleWrite {
		t.Errorf("agent role = %q", r)
	}
	if svc.IsSiteAdmin(ctx, e.agent) {
		t.Error("alice's agent is not a site admin")
	}
	_, _, err = svc.Authorize(ctx, bobAgent, "acme/api", service.RoleAdmin)
	if code(err) != service.CodePermission || !strings.Contains(err.Error(), "agent:bob/helper acts for user:bob") {
		t.Errorf("agent permission error = %v", err)
	}
	if _, err := svc.CreateOrg(ctx, e.agent, "agentorg", ""); code(err) != service.CodePermission {
		t.Errorf("agent CreateOrg: %v", err)
	}
	if _, err := svc.GrantRepo(ctx, e.agent, "acme/api", "carol", service.RoleRead); code(err) != service.CodePermission {
		t.Errorf("agent GrantRepo: %v", err)
	}
	if _, err := svc.SetMember(ctx, e.agent, "acme", "carol", service.OrgAdmin); code(err) != service.CodePermission {
		t.Errorf("agent SetMember: %v", err)
	}
	if _, err := svc.CreateTeam(ctx, e.agent, "acme", "x"); code(err) != service.CodePermission {
		t.Errorf("agent CreateTeam: %v", err)
	}
	if _, err := svc.GrantRepo(ctx, alice, "acme/api", "agent:bob/helper", service.RoleRead); code(err) != service.CodeInvalid {
		t.Errorf("grant to an agent: %v", err)
	}

	// Service principals: direct grants only.
	ciP, err := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindService, Handle: "ci"})
	e.must(err)
	ci, _ := svc.ResolveActor(ctx, ciP)
	if r := e.roleOf(ci, "acme/api"); r != service.RoleNone {
		t.Errorf("service without grant = %q", r)
	}
	_, err = svc.GrantRepo(ctx, alice, "acme/api", "service:ci", service.RoleRead)
	e.must(err)
	if r := e.roleOf(ci, "acme/api"); r != service.RoleRead {
		t.Errorf("service with grant = %q", r)
	}
	if ids, all, err := svc.ReadableRepoIDs(ctx, ci); err != nil || all || len(ids) != 1 || ids[0] != repo.ID {
		t.Errorf("service readable = %v %v %v", ids, all, err)
	}

	// Revoking a grant takes it away; non-admins cannot grant.
	e.must(svc.RevokeRepo(ctx, alice, "acme/api", "user:bob"))
	if r := e.roleOf(bob, "acme/api"); r != service.RoleNone {
		t.Errorf("after revoke = %q", r)
	}
	if err := svc.RevokeRepo(ctx, alice, "acme/api", "user:bob"); code(err) != service.CodeNotFound {
		t.Errorf("revoke twice: %v", err)
	}
	if _, err := svc.GrantRepo(ctx, bob, "acme/api", "bob", service.RoleAdmin); code(err) != service.CodeNotFound {
		t.Errorf("bob granting on a repo he cannot see: %v", err)
	}

	// Who has access, and why.
	grants, people, err := svc.RepoAccess(ctx, alice, "acme/api")
	e.must(err)
	if len(grants) != 2 { // service:ci read, team:acme/backend write
		t.Errorf("grants = %+v", grants)
	}
	via := map[string]string{}
	for _, p := range people {
		via[p.Person.Handle] = string(p.Role) + " " + strings.Join(p.Via, ",")
	}
	if via["alice"] != "admin org owner" || via["dave"] != "admin org admin" || via["erin"] != "admin site admin" || via["ci"] != "read service:ci" {
		t.Errorf("access = %v", via)
	}
	if _, _, err := svc.RepoAccess(ctx, bobAgent, "acme/api"); code(err) != service.CodeNotFound {
		t.Errorf("RepoAccess by a non-reader: %v", err)
	}

	// Every change is in the operation log.
	ops, err := svc.Store.ListOps(ctx, repo.ID, 50)
	e.must(err)
	seen := map[string]int{}
	for _, o := range ops {
		seen[o.Op]++
	}
	if seen["repo.grant"] != 3 || seen["repo.revoke"] != 1 {
		t.Errorf("ops = %v", seen)
	}
}

func TestOrgMembershipRules(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	alice := e.alice
	bob, carol := e.person("bob"), e.person("carol")
	_, err := svc.CreateOrg(ctx, alice, "acme", "")
	e.must(err)
	if _, err := svc.CreateOrg(ctx, bob, "carol", ""); code(err) != service.CodeExists {
		t.Errorf("org named like a person: %v", err)
	}

	// The last owner cannot be demoted or removed.
	if _, err := svc.SetMember(ctx, alice, "acme", "alice", service.OrgAdmin); code(err) != service.CodeFailedPrecondition {
		t.Errorf("demote last owner: %v", err)
	}
	if err := svc.RemoveMember(ctx, alice, "acme", "alice"); code(err) != service.CodeFailedPrecondition {
		t.Errorf("remove last owner: %v", err)
	}
	// Admins manage members but not owners.
	_, err = svc.SetMember(ctx, alice, "acme", "bob", service.OrgAdmin)
	e.must(err)
	if _, err := svc.SetMember(ctx, bob, "acme", "carol", service.OrgOwner); code(err) != service.CodePermission {
		t.Errorf("admin making an owner: %v", err)
	}
	if _, err := svc.SetMember(ctx, bob, "acme", "alice", service.OrgMember); code(err) != service.CodePermission {
		t.Errorf("admin demoting an owner: %v", err)
	}
	if err := svc.RemoveMember(ctx, bob, "acme", "alice"); code(err) != service.CodePermission {
		t.Errorf("admin removing an owner: %v", err)
	}
	m, err := svc.SetMember(ctx, bob, "acme", "carol", service.OrgMember)
	if err != nil || m.Role != service.OrgMember || m.Person.Handle != "carol" {
		t.Fatalf("admin adds a member = %+v, %v", m, err)
	}
	// Members cannot manage; non-members do not see the org.
	if _, err := svc.SetMember(ctx, carol, "acme", "carol", service.OrgAdmin); code(err) != service.CodePermission {
		t.Errorf("member promoting herself: %v", err)
	}
	dave := e.person("dave")
	if _, err := svc.GetOrg(ctx, dave, "acme"); code(err) != service.CodeNotFound {
		t.Errorf("non-member GetOrg: %v", err)
	}
	if _, _, err := svc.ListOrgs(ctx, dave, true); code(err) != service.CodePermission {
		t.Errorf("non-admin ListOrgs all: %v", err)
	}
	// Owners make owners; then the first owner may step down.
	_, err = svc.SetMember(ctx, alice, "acme", "bob", service.OrgOwner)
	e.must(err)
	_, err = svc.SetMember(ctx, alice, "acme", "alice", service.OrgMember)
	e.must(err)
	// Anyone may leave.
	e.must(svc.RemoveMember(ctx, carol, "acme", "carol"))

	d, err := svc.GetOrg(ctx, bob, "acme")
	e.must(err)
	roles := map[string]service.OrgRole{}
	for _, m := range d.Members {
		roles[m.Person.Handle] = m.Role
	}
	if d.MyRole != service.OrgOwner || roles["alice"] != service.OrgMember || roles["bob"] != service.OrgOwner || len(roles) != 2 {
		t.Errorf("org = %+v %v", d.MyRole, roles)
	}
	orgs, myRoles, err := svc.ListOrgs(ctx, alice, false)
	e.must(err)
	if len(orgs) != 2 || orgs[0].Handle != "acme" || myRoles[0] != service.OrgMember || orgs[1].Handle != "alice" || !orgs[1].Personal {
		t.Errorf("alice's orgs = %+v %v", orgs, myRoles)
	}

	// Personal orgs have only their person.
	if _, err := svc.SetMember(ctx, alice, "alice", "bob", service.OrgMember); code(err) != service.CodeFailedPrecondition {
		t.Errorf("member in a personal org: %v", err)
	}
}

func TestCreateRepoOrgAndPeople(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	alice := e.alice
	bob := e.person("bob")
	_, err := svc.CreateOrg(ctx, alice, "acme", "")
	e.must(err)
	_, err = svc.SetMember(ctx, alice, "acme", "bob", service.OrgMember)
	e.must(err)

	// A member cannot create repos in the org: an empty org falls back to
	// the personal org, an explicit one is refused.
	r, err := svc.CreateRepo(ctx, bob, service.CreateRepoInput{Name: "acme/bobs"})
	if err != nil || r.Org != "bob" {
		t.Errorf("member's acme/bobs = %+v, %v", r, err)
	}
	if _, err := svc.CreateRepo(ctx, bob, service.CreateRepoInput{Name: "x", Org: "acme"}); code(err) != service.CodePermission {
		t.Errorf("member creating in acme: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, bob, service.CreateRepoInput{Name: "x", Org: "nope"}); code(err) != service.CodeNotFound {
		t.Errorf("unknown org: %v", err)
	}
	// Alice's agent creates in acme for her.
	if r, err := svc.CreateRepo(ctx, e.agent, service.CreateRepoInput{Name: "acme/web"}); err != nil || r.Org != "acme" {
		t.Errorf("agent's acme/web = %+v, %v", r, err)
	}
	if r, err := svc.CreateRepo(ctx, alice, service.CreateRepoInput{Name: "tools", Org: "acme"}); err != nil || r.Org != "acme" {
		t.Errorf("tools = %+v, %v", r, err)
	}
	d, err := svc.GetOrg(ctx, bob, "acme")
	e.must(err)
	if len(d.Repos) != 2 { // acme/web and tools, by the org default
		t.Errorf("bob sees %d acme repos", len(d.Repos))
	}

	// People: site admins only.
	if _, _, err := svc.CreatePerson(ctx, alice, "zed", "", "", false, ""); code(err) != service.CodePermission {
		t.Errorf("non-admin CreatePerson: %v", err)
	}
	e.must(svc.Store.SetSiteAdmin(ctx, alice.ID, true))
	alice, _ = svc.ResolveActor(ctx, alice.Principal)
	if _, _, err := svc.CreatePerson(ctx, e.agent, "zed", "", "", false, ""); code(err) != service.CodePermission {
		t.Errorf("agent CreatePerson: %v", err)
	}
	zed, token, err := svc.CreatePerson(ctx, alice, "zed", "Zed", "zed@example.com", true, "acme")
	if err != nil || token == "" || !zed.SiteAdmin {
		t.Fatalf("CreatePerson = %+v, %v", zed, err)
	}
	if p, err := svc.Store.Authenticate(ctx, token); err != nil || p.ID != zed.ID {
		t.Errorf("token: %v", err)
	}
	zedActor, _ := svc.ResolveActor(ctx, zed)
	if orgs, roles, _ := svc.ListOrgs(ctx, zedActor, false); len(orgs) != 2 || roles[0] != service.OrgMember || !orgs[1].Personal {
		t.Errorf("zed's orgs = %+v %v", orgs, roles)
	}
	people, err := svc.ListPeople(ctx, alice)
	if err != nil || len(people) != 3 {
		t.Errorf("people = %d, %v", len(people), err)
	}
	if _, err := svc.ListPeople(ctx, bob); code(err) != service.CodePermission {
		t.Errorf("non-admin ListPeople: %v", err)
	}
}
