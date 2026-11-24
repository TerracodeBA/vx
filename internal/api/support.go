package api

// Support access (#33): repository admins give a superadmin a
// time-limited role on one repository.

import (
	"context"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
)

func supportPB(g service.SupportGrant) *vxv1.SupportGrant {
	return &vxv1.SupportGrant{
		Repo: g.Repo, Person: personPB(g.Person), Role: string(g.Role), ExpiresAt: ts(g.ExpiresAt),
		Reason: g.Reason, GrantedBy: g.GrantedBy, CreatedAt: ts(g.CreatedAt),
	}
}

func (h *Handlers) GrantSupport(ctx context.Context, req *connect.Request[vxv1.GrantSupportRequest]) (*connect.Response[vxv1.GrantSupportResponse], error) {
	m := req.Msg
	g, err := h.svc.GrantSupport(ctx, actorFrom(ctx), service.SupportInput{
		Repo: m.Repo, Person: m.Person, Role: service.RepoRole(m.Role),
		For: time.Duration(m.DurationSeconds) * time.Second, Reason: m.Reason,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GrantSupportResponse{Grant: supportPB(g)}), nil
}

func (h *Handlers) RevokeSupport(ctx context.Context, req *connect.Request[vxv1.RevokeSupportRequest]) (*connect.Response[vxv1.RevokeSupportResponse], error) {
	if err := h.svc.RevokeSupport(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Person); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RevokeSupportResponse{}), nil
}

func (h *Handlers) ListSupport(ctx context.Context, req *connect.Request[vxv1.ListSupportRequest]) (*connect.Response[vxv1.ListSupportResponse], error) {
	gs, err := h.svc.ListSupport(ctx, actorFrom(ctx), req.Msg.Repo)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListSupportResponse{}
	for _, g := range gs {
		out.Grants = append(out.Grants, supportPB(g))
	}
	return connect.NewResponse(out), nil
}
