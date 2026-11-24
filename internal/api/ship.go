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
		Warnings: warningsPB(p.Warnings),
	}
	if p.ConflictedAt != nil {
		out.ConflictedChange = p.ConflictedAt.ChangeID
	}
	out.Steps = stepsPB(p.Steps, p.Effort.Slug)
	out.Split, out.Suggested, out.Problems, out.Notes = string(p.Split), p.Suggested, p.Problems, p.Notes
	for _, sh := range p.Stale {
		out.Stale = append(out.Stale, shipmentPB(p.Effort.Slug, sh))
	}
	for _, g := range p.Groups {
		pg := &vxv1.ShipGroup{
			Part: int32(g.Part), Name: g.Name, Title: g.Title, Branch: g.Branch, Base: g.Base,
			Steps: stepsPB(g.Steps, p.Effort.Slug), Files: g.Files, Why: g.Why,
			Conflicts: conflictsPB(g.Conflicts), Problem: g.Problem, Warnings: warningsPB(g.Warnings), Body: g.Body,
		}
		if g.ConflictedAt != nil {
			pg.ConflictedChange = g.ConflictedAt.ChangeID
		}
		if g.Previous != nil {
			pg.PrNumber, pg.Url = g.Previous.ProviderRef, g.Previous.URL
		}
		out.Groups = append(out.Groups, pg)
	}
	return out
}

func stepsPB(steps []service.ShipStep, slug string) []*vxv1.ShipStep {
	var out []*vxv1.ShipStep
	for _, st := range steps {
		out = append(out, &vxv1.ShipStep{
			Changeset: changesetRowPB(st.Changeset, slug, nil),
			Files:     int32(st.Files), Skipped: st.Skipped,
		})
	}
	return out
}

func groupsIn(in []*vxv1.ShipGroupInput) [][]string {
	var out [][]string
	for _, g := range in {
		out = append(out, g.ChangeIds)
	}
	return out
}

func shipmentPB(effort string, s store.Shipment) *vxv1.Shipment {
	out := &vxv1.Shipment{
		Id: s.ID.String(), Effort: effort, Title: s.Title, Branch: s.Branch, Target: s.Target,
		BaseGitSha: s.BaseGitSHA, HeadGitSha: s.HeadGitSHA, PrNumber: s.ProviderRef, Url: s.URL,
		State: s.State, UpdatedAt: ts(s.UpdatedAt), Split: s.Split, Part: int32(s.Part),
	}
	for _, c := range s.Changes {
		out.ChangeIds = append(out.ChangeIds, c.ChangeID)
	}
	return out
}

func (h *Handlers) PlanShipment(ctx context.Context, req *connect.Request[vxv1.PlanShipmentRequest]) (*connect.Response[vxv1.PlanShipmentResponse], error) {
	m := req.Msg
	split, err := service.ParseSplitMode(m.Split)
	if err != nil {
		return nil, toConnect(err)
	}
	var p service.ShipPlan
	if split != service.SplitNone {
		p, err = h.svc.PlanSplit(ctx, actorFrom(ctx), service.SplitInput{Repo: m.Repo, Effort: m.Effort, Target: m.Target, Mode: split, Groups: groupsIn(m.Groups)})
	} else {
		p, err = h.svc.PlanShipment(ctx, actorFrom(ctx), m.Repo, m.Effort, m.Target)
	}
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.PlanShipmentResponse{Plan: planPB(p)}), nil
}

func (h *Handlers) OpenShipment(ctx context.Context, req *connect.Request[vxv1.OpenShipmentRequest]) (*connect.Response[vxv1.OpenShipmentResponse], error) {
	m := req.Msg
	split, err := service.ParseSplitMode(m.Split)
	if err != nil {
		return nil, toConnect(err)
	}
	res, err := h.svc.OpenShipment(ctx, actorFrom(ctx), service.ShipInput{
		Repo: m.Repo, Effort: m.Effort, Target: m.Target, Title: m.Title, Body: m.Body, Draft: m.Draft,
		Split: split, Groups: groupsIn(m.Groups),
	})
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.OpenShipmentResponse{
		Shipment: shipmentPB(m.Effort, res.Shipment), Plan: planPB(res.Plan), PrUpdated: res.PR.Updated,
	}
	for _, p := range res.Parts {
		out.Shipments = append(out.Shipments, shipmentPB(m.Effort, p.Shipment))
		out.PrsUpdated = append(out.PrsUpdated, p.PR.Updated)
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) ListShipments(ctx context.Context, req *connect.Request[vxv1.ListShipmentsRequest]) (*connect.Response[vxv1.ListShipmentsResponse], error) {
	list, err := h.svc.ListShipments(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	cis, err := h.svc.ShipmentsCI(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListShipmentsResponse{}
	for _, s := range list {
		pb := shipmentPB(req.Msg.Effort, s)
		if ci, ok := cis[s.ID]; ok {
			pb.CiState, pb.CiSha = ci.State, ci.HeadSHA
			for _, c := range ci.Checks {
				pb.CiChecks = append(pb.CiChecks, &vxv1.ShipmentCheck{Name: c.Name, Source: c.Source, Status: c.Status, Conclusion: c.Conclusion, Url: c.URL})
			}
		}
		out.Shipments = append(out.Shipments, pb)
	}
	return connect.NewResponse(out), nil
}
