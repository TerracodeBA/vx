package api

import (
	"context"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
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

// errNotYet is the answer of RPCs whose part of v0.3 has not landed.
func errNotYet(what string) error {
	return &service.Error{Code: service.CodeUnavailable, Msg: what + " are not implemented yet"}
}

func (h *Handlers) GetRules(ctx context.Context, req *connect.Request[vxv1.GetRulesRequest]) (*connect.Response[vxv1.GetRulesResponse], error) {
	return nil, toConnect(errNotYet("vx.toml rules"))
}

func (h *Handlers) Reserve(ctx context.Context, req *connect.Request[vxv1.ReserveRequest]) (*connect.Response[vxv1.ReserveResponse], error) {
	v, ex, err := h.svc.Reserve(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort, req.Msg.Rule)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ReserveResponse{Value: v, Example: ex}), nil
}

func (h *Handlers) ListReservations(ctx context.Context, req *connect.Request[vxv1.ListReservationsRequest]) (*connect.Response[vxv1.ListReservationsResponse], error) {
	return nil, toConnect(errNotYet("reservations"))
}
