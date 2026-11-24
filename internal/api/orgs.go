package api

import (
	"context"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// ---------- Orgs ----------

func orgPB(o service.Org, myRole service.OrgRole) *vxv1.Org {
	def := string(o.DefaultRepoRole)
	if def == "" {
		def = "none"
	}
	return &vxv1.Org{
		Handle: o.Handle, DisplayName: o.DisplayName, DefaultRepoRole: def,
		CreatedAt: ts(o.CreatedAt), MyRole: string(myRole), Personal: o.Personal,
	}
}

// personPB converts a person (or service principal) that is not the
// caller, with whether they are a superadmin.
func personPB(p store.Principal) *vxv1.Principal {
	out := principalPB(service.Actor{Principal: p})
	out.SiteAdmin = p.SiteAdmin
	return out
}

func peoplePB(ps []store.Principal) []*vxv1.Principal {
	out := make([]*vxv1.Principal, 0, len(ps))
	for _, p := range ps {
		out = append(out, personPB(p))
	}
	return out
}

func teamPB(t service.Team) *vxv1.Team {
	return &vxv1.Team{Org: t.Org, Name: t.Name, Members: peoplePB(t.Members)}
}

func memberPB(m service.Membership) *vxv1.OrgMember {
	return &vxv1.OrgMember{Person: personPB(m.Person), Role: string(m.Role), Teams: m.Teams}
}

func grantPB(g service.RepoGrant) *vxv1.RepoGrant {
	return &vxv1.RepoGrant{Repo: g.Repo, Subject: g.Subject, Role: string(g.Role)}
}

func (h *Handlers) CreateOrg(ctx context.Context, req *connect.Request[vxv1.CreateOrgRequest]) (*connect.Response[vxv1.CreateOrgResponse], error) {
	o, err := h.svc.CreateOrg(ctx, actorFrom(ctx), req.Msg.Handle, req.Msg.DisplayName)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.CreateOrgResponse{Org: orgPB(o, service.OrgOwner)}), nil
}

func (h *Handlers) ListOrgs(ctx context.Context, req *connect.Request[vxv1.ListOrgsRequest]) (*connect.Response[vxv1.ListOrgsResponse], error) {
	orgs, roles, err := h.svc.ListOrgs(ctx, actorFrom(ctx), req.Msg.All)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListOrgsResponse{}
	for i, o := range orgs {
		out.Orgs = append(out.Orgs, orgPB(o, roles[i]))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) GetOrg(ctx context.Context, req *connect.Request[vxv1.GetOrgRequest]) (*connect.Response[vxv1.GetOrgResponse], error) {
	actor := actorFrom(ctx)
	d, err := h.svc.GetOrg(ctx, actor, req.Msg.Handle)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.GetOrgResponse{Org: orgPB(d.Org, d.MyRole)}
	for _, m := range d.Members {
		out.Members = append(out.Members, memberPB(m))
	}
	for _, t := range d.Teams {
		out.Teams = append(out.Teams, teamPB(t))
	}
	repos, roles, err := h.svc.ReadableRepos(ctx, actor, &d.Org.ID) // d.Repos with the caller's roles
	if err != nil {
		return nil, toConnect(err)
	}
	for i, r := range repos {
		pb := repoPB(service.RepoView{Repo: r, Role: roles[i]}, nil)
		pb.Org = r.Org
		out.Repos = append(out.Repos, pb)
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) UpdateOrg(ctx context.Context, req *connect.Request[vxv1.UpdateOrgRequest]) (*connect.Response[vxv1.UpdateOrgResponse], error) {
	actor := actorFrom(ctx)
	var def *service.RepoRole
	if r := req.Msg.DefaultRepoRole; r != "" {
		role := service.RepoRole(r)
		def = &role
	}
	o, err := h.svc.UpdateOrg(ctx, actor, req.Msg.Handle, req.Msg.DisplayName, def)
	if err != nil {
		return nil, toConnect(err)
	}
	d, err := h.svc.GetOrg(ctx, actor, o.Handle)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.UpdateOrgResponse{Org: orgPB(o, d.MyRole)}), nil
}

func (h *Handlers) SetMember(ctx context.Context, req *connect.Request[vxv1.SetMemberRequest]) (*connect.Response[vxv1.SetMemberResponse], error) {
	m, err := h.svc.SetMember(ctx, actorFrom(ctx), req.Msg.Org, req.Msg.Person, service.OrgRole(req.Msg.Role))
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.SetMemberResponse{Member: memberPB(m)}), nil
}

func (h *Handlers) RemoveMember(ctx context.Context, req *connect.Request[vxv1.RemoveMemberRequest]) (*connect.Response[vxv1.RemoveMemberResponse], error) {
	if err := h.svc.RemoveMember(ctx, actorFrom(ctx), req.Msg.Org, req.Msg.Person); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RemoveMemberResponse{}), nil
}

func (h *Handlers) CreateTeam(ctx context.Context, req *connect.Request[vxv1.CreateTeamRequest]) (*connect.Response[vxv1.CreateTeamResponse], error) {
	t, err := h.svc.CreateTeam(ctx, actorFrom(ctx), req.Msg.Org, req.Msg.Name)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.CreateTeamResponse{Team: teamPB(t)}), nil
}

func (h *Handlers) DeleteTeam(ctx context.Context, req *connect.Request[vxv1.DeleteTeamRequest]) (*connect.Response[vxv1.DeleteTeamResponse], error) {
	if err := h.svc.DeleteTeam(ctx, actorFrom(ctx), req.Msg.Org, req.Msg.Name); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.DeleteTeamResponse{}), nil
}

func (h *Handlers) SetTeamMember(ctx context.Context, req *connect.Request[vxv1.SetTeamMemberRequest]) (*connect.Response[vxv1.SetTeamMemberResponse], error) {
	t, err := h.svc.SetTeamMember(ctx, actorFrom(ctx), req.Msg.Org, req.Msg.Team, req.Msg.Person)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.SetTeamMemberResponse{Team: teamPB(t)}), nil
}

func (h *Handlers) RemoveTeamMember(ctx context.Context, req *connect.Request[vxv1.RemoveTeamMemberRequest]) (*connect.Response[vxv1.RemoveTeamMemberResponse], error) {
	t, err := h.svc.RemoveTeamMember(ctx, actorFrom(ctx), req.Msg.Org, req.Msg.Team, req.Msg.Person)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RemoveTeamMemberResponse{Team: teamPB(t)}), nil
}

func (h *Handlers) GrantRepo(ctx context.Context, req *connect.Request[vxv1.GrantRepoRequest]) (*connect.Response[vxv1.GrantRepoResponse], error) {
	g, err := h.svc.GrantRepo(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Subject, service.RepoRole(req.Msg.Role))
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GrantRepoResponse{Grant: grantPB(g)}), nil
}

func (h *Handlers) RevokeRepo(ctx context.Context, req *connect.Request[vxv1.RevokeRepoRequest]) (*connect.Response[vxv1.RevokeRepoResponse], error) {
	if err := h.svc.RevokeRepo(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Subject); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RevokeRepoResponse{}), nil
}

func (h *Handlers) RepoAccess(ctx context.Context, req *connect.Request[vxv1.RepoAccessRequest]) (*connect.Response[vxv1.RepoAccessResponse], error) {
	grants, people, err := h.svc.RepoAccess(ctx, actorFrom(ctx), req.Msg.Repo)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.RepoAccessResponse{}
	for _, g := range grants {
		out.Grants = append(out.Grants, grantPB(g))
	}
	for _, e := range people {
		out.People = append(out.People, &vxv1.RepoAccessEntry{Person: personPB(e.Person), Role: string(e.Role), Via: e.Via})
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) CreatePerson(ctx context.Context, req *connect.Request[vxv1.CreatePersonRequest]) (*connect.Response[vxv1.CreatePersonResponse], error) {
	m := req.Msg
	p, token, err := h.svc.CreatePerson(ctx, actorFrom(ctx), m.Handle, m.DisplayName, m.Email, m.SiteAdmin, m.Org)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.CreatePersonResponse{Person: personPB(p), Token: token}), nil
}

func (h *Handlers) ListPeople(ctx context.Context, _ *connect.Request[vxv1.ListPeopleRequest]) (*connect.Response[vxv1.ListPeopleResponse], error) {
	people, err := h.svc.ListPeople(ctx, actorFrom(ctx))
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ListPeopleResponse{People: peoplePB(people)}), nil
}
