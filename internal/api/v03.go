package api

import (
	"context"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// overlapPB converts a forecast collision.
func overlapPB(o service.Overlap) *vxv1.Overlap {
	return &vxv1.Overlap{
		Repo: o.RepoName, Effort: o.Effort.Slug, OtherEffort: o.Other.Slug, OtherName: o.Other.Name,
		OtherOwner: o.OtherOwner.Ref(), Status: o.Status, SharedFiles: o.SharedFiles, Conflicts: o.Conflicts,
		Warnings: warningsPB(o.Warnings), CheckedAt: ts(o.CheckedAt),
	}
}

func overlapsPB(os []service.Overlap) []*vxv1.Overlap {
	out := make([]*vxv1.Overlap, 0, len(os))
	for _, o := range os {
		out = append(out, overlapPB(o))
	}
	return out
}

func (h *Handlers) ListOverlaps(ctx context.Context, req *connect.Request[vxv1.ListOverlapsRequest]) (*connect.Response[vxv1.ListOverlapsResponse], error) {
	os, err := h.svc.ListOverlaps(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ListOverlapsResponse{Overlaps: overlapsPB(os)}), nil
}

// rulesPB converts a repository's vx.toml rules.
func rulesPB(r service.Rules) *vxv1.RepoRules {
	out := &vxv1.RepoRules{RequiredChecks: r.RequiredChecks, Source: r.Source}
	for _, u := range r.Unique {
		out.Unique = append(out.Unique, &vxv1.UniqueRule{Name: u.Name, Glob: u.Glob, Key: u.Key})
	}
	for _, g := range r.Generated {
		out.Generated = append(out.Generated, &vxv1.GeneratedRule{Glob: g.Glob, Command: g.Command})
	}
	return out
}

func (h *Handlers) GetRules(ctx context.Context, req *connect.Request[vxv1.GetRulesRequest]) (*connect.Response[vxv1.GetRulesResponse], error) {
	r, err := h.svc.GetRules(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetRulesResponse{Rules: rulesPB(r)}), nil
}

func (h *Handlers) Reserve(ctx context.Context, req *connect.Request[vxv1.ReserveRequest]) (*connect.Response[vxv1.ReserveResponse], error) {
	v, ex, err := h.svc.Reserve(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort, req.Msg.Rule)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ReserveResponse{Value: v, Example: ex}), nil
}

func (h *Handlers) ListReservations(ctx context.Context, req *connect.Request[vxv1.ListReservationsRequest]) (*connect.Response[vxv1.ListReservationsResponse], error) {
	rs, err := h.svc.ListReservations(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Rule)
	if err != nil {
		return nil, toConnect(err)
	}
	out := make([]*vxv1.Reservation, 0, len(rs))
	for _, r := range rs {
		out = append(out, &vxv1.Reservation{Rule: r.Rule, Value: r.Value, Effort: r.EffortSlug, ReservedBy: r.ReservedBy, CreatedAt: ts(r.CreatedAt)})
	}
	return connect.NewResponse(&vxv1.ListReservationsResponse{Reservations: out}), nil
}

func (h *Handlers) CloseEffort(ctx context.Context, req *connect.Request[vxv1.CloseEffortRequest]) (*connect.Response[vxv1.CloseEffortResponse], error) {
	m := req.Msg
	e, err := h.svc.CloseEffort(ctx, actorFrom(ctx), m.Repo, m.Effort, store.EffortState(m.State))
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.CloseEffortResponse{Effort: effortPB(m.Repo, e, h.ref(ctx, e.OwnerID), 0)}), nil
}
