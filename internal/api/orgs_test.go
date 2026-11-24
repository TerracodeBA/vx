package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

func TestOrgServiceOverConnect(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	token := func(handle string, admin bool) string {
		p, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: handle, SiteAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		tok, err := st.CreateToken(ctx, p.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	orgsAs := func(tok string) vxv1connect.OrgServiceClient {
		return vxv1connect.NewOrgServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(bearer(tok)))
	}
	aliceTok, bobTok := token("alice", true), token("bob", false)
	alice, bob := orgsAs(aliceTok), orgsAs(bobTok)
	aliceRepos := vxv1connect.NewRepoServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(bearer(aliceTok)))

	created, err := alice.CreateOrg(ctx, connect.NewRequest(&vxv1.CreateOrgRequest{Handle: "acme", DisplayName: "Acme"}))
	if err != nil || created.Msg.Org.MyRole != "owner" || created.Msg.Org.DefaultRepoRole != "read" {
		t.Fatalf("CreateOrg = %v, %v", created, err)
	}
	if _, err := aliceRepos.CreateRepo(ctx, connect.NewRequest(&vxv1.CreateRepoRequest{Name: "api", Provider: "none", Org: "acme"})); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.GetOrg(ctx, connect.NewRequest(&vxv1.GetOrgRequest{Handle: "acme"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("bob GetOrg: %v", err)
	}
	if _, err := bob.RepoAccess(ctx, connect.NewRequest(&vxv1.RepoAccessRequest{Repo: "api"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("bob RepoAccess: %v", err)
	}
	if _, err := alice.SetMember(ctx, connect.NewRequest(&vxv1.SetMemberRequest{Org: "acme", Person: "bob", Role: "member"})); err != nil {
		t.Fatal(err)
	}
	none := "none"
	upd, err := alice.UpdateOrg(ctx, connect.NewRequest(&vxv1.UpdateOrgRequest{Handle: "acme", DefaultRepoRole: none}))
	if err != nil || upd.Msg.Org.DefaultRepoRole != "none" || upd.Msg.Org.MyRole != "owner" {
		t.Fatalf("UpdateOrg = %v, %v", upd, err)
	}
	if _, err := alice.CreateTeam(ctx, connect.NewRequest(&vxv1.CreateTeamRequest{Org: "acme", Name: "backend"})); err != nil {
		t.Fatal(err)
	}
	team, err := alice.SetTeamMember(ctx, connect.NewRequest(&vxv1.SetTeamMemberRequest{Org: "acme", Team: "backend", Person: "bob"}))
	if err != nil || len(team.Msg.Team.Members) != 1 || team.Msg.Team.Members[0].Ref != "user:bob" {
		t.Fatalf("SetTeamMember = %v, %v", team, err)
	}
	g, err := alice.GrantRepo(ctx, connect.NewRequest(&vxv1.GrantRepoRequest{Repo: "api", Subject: "team:acme/backend", Role: "write"}))
	if err != nil || g.Msg.Grant.Subject != "team:acme/backend" {
		t.Fatalf("GrantRepo = %v, %v", g, err)
	}
	org, err := bob.GetOrg(ctx, connect.NewRequest(&vxv1.GetOrgRequest{Handle: "acme"}))
	if err != nil || len(org.Msg.Repos) != 1 || org.Msg.Repos[0].MyRole != "write" || org.Msg.Repos[0].Org != "acme" || org.Msg.Org.MyRole != "member" {
		t.Fatalf("bob GetOrg = %v, %v", org, err)
	}
	if len(org.Msg.Members) != 2 || len(org.Msg.Teams) != 1 {
		t.Errorf("members %d teams %d", len(org.Msg.Members), len(org.Msg.Teams))
	}
	if _, err := bob.GrantRepo(ctx, connect.NewRequest(&vxv1.GrantRepoRequest{Repo: "api", Subject: "bob", Role: "admin"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("bob granting himself admin: %v", err)
	}
	acc, err := alice.RepoAccess(ctx, connect.NewRequest(&vxv1.RepoAccessRequest{Repo: "api"}))
	if err != nil || len(acc.Msg.Grants) != 1 || len(acc.Msg.People) != 2 {
		t.Fatalf("RepoAccess = %v, %v", acc, err)
	}

	// People.
	cp, err := alice.CreatePerson(ctx, connect.NewRequest(&vxv1.CreatePersonRequest{Handle: "carol", SiteAdmin: true, Org: "acme"}))
	if err != nil || cp.Msg.Token == "" || !cp.Msg.Person.SiteAdmin {
		t.Fatalf("CreatePerson = %v, %v", cp, err)
	}
	if _, err := bob.CreatePerson(ctx, connect.NewRequest(&vxv1.CreatePersonRequest{Handle: "dave"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("bob CreatePerson: %v", err)
	}
	people, err := alice.ListPeople(ctx, connect.NewRequest(&vxv1.ListPeopleRequest{}))
	if err != nil || len(people.Msg.People) != 3 {
		t.Fatalf("ListPeople = %v, %v", people, err)
	}
	all, err := alice.ListOrgs(ctx, connect.NewRequest(&vxv1.ListOrgsRequest{All: true}))
	if err != nil || len(all.Msg.Orgs) != 4 { // acme and three personal orgs
		t.Fatalf("ListOrgs all = %v, %v", all, err)
	}

	// Removing, revoking, deleting.
	if _, err := alice.RemoveTeamMember(ctx, connect.NewRequest(&vxv1.RemoveTeamMemberRequest{Org: "acme", Team: "backend", Person: "bob"})); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.RevokeRepo(ctx, connect.NewRequest(&vxv1.RevokeRepoRequest{Repo: "api", Subject: "team:acme/backend"})); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.DeleteTeam(ctx, connect.NewRequest(&vxv1.DeleteTeamRequest{Org: "acme", Name: "backend"})); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.RemoveMember(ctx, connect.NewRequest(&vxv1.RemoveMemberRequest{Org: "acme", Person: "bob"})); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.GetOrg(ctx, connect.NewRequest(&vxv1.GetOrgRequest{Handle: "acme"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("bob after removal: %v", err)
	}
}
