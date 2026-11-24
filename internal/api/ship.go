package api

import (
	"context"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func planPB(p service.ShipPlan) *vxv1.ShipmentPlan {
	out := &vxv1.ShipmentPlan{
		Target: p.Target, TargetGitSha: p.TargetHead.GitSHA, Branch: p.Branch,
		Title: p.Title, Body: p.Body, Conflicts: conflictsPB(p.Conflicts),
	}
	if p.ConflictedAt != nil {
		out.ConflictedChange = p.ConflictedAt.ChangeID
	}
	for _, st := range p.Steps {
		out.Steps = append(out.Steps, &vxv1.ShipStep{
			Changeset: changesetPB(st.Changeset.Hash, st.Changeset.Changeset, p.Effort.Slug, nil),
			Files:     int32(st.Files), Skipped: st.Skipped,
		})
	}
	return out
}

func shipmentPB(effort string, s store.Shipment) *vxv1.Shipment {
	out := &vxv1.Shipment{
		Id: s.ID.String(), Effort: effort, Title: s.Title, Branch: s.Branch, Target: s.Target,
		BaseGitSha: s.BaseGitSHA, HeadGitSha: s.HeadGitSHA, PrNumber: s.ProviderRef, Url: s.URL,
		State: s.State, UpdatedAt: ts(s.UpdatedAt),
	}
	for _, c := range s.Changes {
		out.ChangeIds = append(out.ChangeIds, c.ChangeID)
	}
	return out
}

func (h *Handlers) PlanShipment(ctx context.Context, req *connect.Request[vxv1.PlanShipmentRequest]) (*connect.Response[vxv1.PlanShipmentResponse], error) {
	p, err := h.svc.PlanShipment(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort, req.Msg.Target)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.PlanShipmentResponse{Plan: planPB(p)}), nil
}

func (h *Handlers) OpenShipment(ctx context.Context, req *connect.Request[vxv1.OpenShipmentRequest]) (*connect.Response[vxv1.OpenShipmentResponse], error) {
	m := req.Msg
	res, err := h.svc.OpenShipment(ctx, actorFrom(ctx), service.ShipInput{
		Repo: m.Repo, Effort: m.Effort, Target: m.Target, Title: m.Title, Body: m.Body, Draft: m.Draft,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.OpenShipmentResponse{
		Shipment: shipmentPB(m.Effort, res.Shipment), Plan: planPB(res.Plan), PrUpdated: res.PR.Updated,
	}), nil
}

func (h *Handlers) ListShipments(ctx context.Context, req *connect.Request[vxv1.ListShipmentsRequest]) (*connect.Response[vxv1.ListShipmentsResponse], error) {
	list, err := h.svc.ListShipments(ctx, req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListShipmentsResponse{}
	for _, s := range list {
		out.Shipments = append(out.Shipments, shipmentPB(req.Msg.Effort, s))
	}
	return connect.NewResponse(out), nil
}
