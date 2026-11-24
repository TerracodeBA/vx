package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// TestOrgsMigrationKeepsAccess runs migration 008 on a database with
// people and repositories and checks that nobody loses access: every
// person owns every organization the repositories went to and is a site
// admin, and everyone has a personal organization.
func TestOrgsMigrationKeepsAccess(t *testing.T) {
	ctx := context.Background()
	url := storetest.NewDatabase(t)
	s, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.MigrateTo(ctx, 5); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`insert into principals (kind, handle, display_name) values ('human', 'alice', 'Alice'), ('human', 'acme', 'Acme Person'), ('service', 'ci', 'CI')`,
		`insert into principals (kind, handle, parent_id, display_name) select 'agent', 'alice/claude', id, 'claude' from principals where handle = 'alice'`,
		`insert into repos (name) values ('Acme/api'), ('acme/web'), ('solo'), ('bob/tool')`,
		`insert into upstreams (repo_id, provider, default_branch) select id, 'none', 'main' from repos`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	wantOrg := map[string]string{"Acme/api": "acme", "acme/web": "acme", "solo": "default", "bob/tool": "bob"}
	repos, err := s.ListRepos(ctx)
	if err != nil || len(repos) != 4 {
		t.Fatalf("repos = %d, %v", len(repos), err)
	}
	for _, r := range repos {
		if r.Org != wantOrg[r.Name] {
			t.Errorf("%s is in %q, want %q", r.Name, r.Org, wantOrg[r.Name])
		}
	}
	alice, _ := s.PrincipalByHandle(ctx, "alice")
	acmePerson, _ := s.PrincipalByHandle(ctx, "acme")
	ci, _ := s.PrincipalByHandle(ctx, "ci")
	agent, _ := s.PrincipalByHandle(ctx, "alice/claude")
	if !alice.SiteAdmin || !acmePerson.SiteAdmin || ci.SiteAdmin || agent.SiteAdmin {
		t.Errorf("site admins: alice %v acme %v ci %v agent %v", alice.SiteAdmin, acmePerson.SiteAdmin, ci.SiteAdmin, agent.SiteAdmin)
	}
	for _, p := range []store.Principal{alice, acmePerson} {
		for _, h := range []string{"acme", "default", "bob"} {
			o, err := s.OrgByHandle(ctx, h)
			if err != nil {
				t.Fatal(err)
			}
			if role, _ := s.OrgRole(ctx, o.ID, p.ID); role != "owner" {
				t.Errorf("%s in %s: %q, want owner", p.Handle, h, role)
			}
		}
	}
	// Personal organizations: alice gets a new one; the person named acme
	// takes over the acme organization made from repository names.
	if o, err := s.OrgByHandle(ctx, "alice"); err != nil || !o.Personal {
		t.Errorf("alice's personal org = %+v, %v", o, err)
	} else if role, _ := s.OrgRole(ctx, o.ID, acmePerson.ID); role != "" {
		t.Errorf("acme person in alice's personal org: %q", role)
	}
	if o, _ := s.OrgByHandle(ctx, "acme"); !o.Personal {
		t.Errorf("acme should be the acme person's personal org")
	}
	if o, _ := s.OrgByHandle(ctx, "default"); o.Personal || o.DefaultRepoRole != "read" {
		t.Errorf("default org = %+v", o)
	}
	// After the migration new repos still need an organization.
	if _, err := conn.Exec(ctx, `insert into repos (name) values ('x')`); err == nil {
		t.Error("repos.org_id should be not null")
	}
}

func TestOrgStore(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	alice, err := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob", SiteAdmin: true})
	if !bob.SiteAdmin {
		t.Error("bob should be a site admin")
	}
	// Creating a person creates their personal org; agents get none.
	personal, err := s.OrgByHandle(ctx, "alice")
	if err != nil || !personal.Personal {
		t.Fatalf("personal org = %+v, %v", personal, err)
	}
	if _, err := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/claude", ParentID: &alice.ID, SiteAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if ag, _ := s.PrincipalByHandle(ctx, "alice/claude"); ag.SiteAdmin {
		t.Error("agents are never site admins")
	}

	acme, err := s.CreateOrg(ctx, "acme", "Acme Inc", alice.ID)
	if err != nil || acme.DefaultRepoRole != "read" || acme.Personal {
		t.Fatalf("CreateOrg = %+v, %v", acme, err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "", bob.ID); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate org: %v", err)
	}
	if _, err := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "acme"}); !errors.Is(err, store.ErrExists) {
		t.Errorf("person named like an org: %v", err)
	}
	if role, _ := s.OrgRole(ctx, acme.ID, alice.ID); role != "owner" {
		t.Errorf("alice in acme = %q", role)
	}
	if err := s.SetOrgMember(ctx, acme.ID, bob.ID, "member"); err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, acme, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMember(ctx, team, bob.ID); err != nil {
		t.Fatal(err)
	}
	carol, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "carol"})
	if err := s.AddTeamMember(ctx, team, carol.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("non-member into team: %v", err)
	}
	members, err := s.ListOrgMembers(ctx, acme.ID)
	if err != nil || len(members) != 2 || members[1].Person.Handle != "bob" || len(members[1].Teams) != 1 {
		t.Fatalf("members = %+v, %v", members, err)
	}

	repo, err := s.CreateRepo(ctx, store.Repo{Name: "acme/api", OrgID: acme.ID})
	if err != nil || repo.Org != "acme" {
		t.Fatalf("CreateRepo = %+v, %v", repo, err)
	}
	if _, f, err := s.RepoRoleFacts(ctx, "acme/api", bob.ID, false); err != nil || f.OrgRole != "member" || f.DefaultRole != "read" || f.Grant != "" || !f.SiteAdmin {
		t.Errorf("bob facts = %+v, %v", f, err)
	}
	tid := team.ID
	if _, err := s.SetRepoGrant(ctx, repo.ID, nil, &tid, "write"); err != nil {
		t.Fatal(err)
	}
	if _, f, _ := s.RepoRoleFacts(ctx, "acme/api", bob.ID, false); f.Grant != "write" {
		t.Errorf("team grant: %+v", f)
	}
	if _, f, _ := s.RepoRoleFacts(ctx, "acme/api", bob.ID, true); f.Grant != "" || f.OrgRole != "" {
		t.Errorf("direct only: %+v", f)
	}
	cid := carol.ID
	if prev, err := s.SetRepoGrant(ctx, repo.ID, &cid, nil, "read"); err != nil || prev != "" {
		t.Fatal(prev, err)
	}
	if prev, err := s.SetRepoGrant(ctx, repo.ID, &cid, nil, "admin"); err != nil || prev != "read" {
		t.Fatal(prev, err)
	}
	grants, err := s.ListRepoGrants(ctx, repo.ID)
	if err != nil || len(grants) != 2 || grants[0].Principal.Handle != "carol" || grants[0].Role != "admin" || grants[1].Team.Name != "backend" {
		t.Fatalf("grants = %+v, %v", grants, err)
	}
	ids, err := s.ReadableRepoIDs(ctx, carol.ID, false)
	if err != nil || len(ids) != 1 || ids[0] != repo.ID {
		t.Errorf("carol readable = %v, %v", ids, err)
	}
	// Removing bob from the org takes him out of its teams too.
	if ok, err := s.RemoveOrgMember(ctx, acme.ID, bob.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got, _ := s.TeamByName(ctx, acme, "backend"); len(got.Members) != 0 {
		t.Errorf("team members after removal = %+v", got.Members)
	}
	if prev, _ := s.DeleteRepoGrant(ctx, repo.ID, &cid, nil); prev != "admin" {
		t.Errorf("deleted grant role = %q", prev)
	}
	// A repo created without an org goes to "default".
	if r, err := s.CreateRepo(ctx, store.Repo{Name: "loose"}); err != nil || r.Org != store.DefaultOrg {
		t.Errorf("loose repo = %+v, %v", r, err)
	}
}
