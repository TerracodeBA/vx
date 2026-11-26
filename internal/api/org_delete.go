package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
)

// ---------- Deleting organizations (#40) ----------

func optTS(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ts(*t)
}

func (h *Handlers) deletionPB(d service.OrgDeletion) *vxv1.OrgDeletion {
	return &vxv1.OrgDeletion{
		Org: d.Org, OrgId: d.OrgID.String(), RequestedBy: d.RequestedBy,
		DeletedAt: ts(d.DeletedAt), PurgeAfter: ts(d.PurgeAfter), PurgeStarted: optTS(d.PurgeStarted), PurgedAt: optTS(d.PurgedAt),
		Counts: d.Counts, Confirmation: d.Confirmation(h.svc.GitHub.PublicURL),
	}
}

func (h *Handlers) DeleteOrg(ctx context.Context, req *connect.Request[vxv1.DeleteOrgRequest]) (*connect.Response[vxv1.DeleteOrgResponse], error) {
	d, err := h.svc.DeleteOrg(ctx, actorFrom(ctx), req.Msg.Handle, req.Msg.Confirm)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.DeleteOrgResponse{Deletion: h.deletionPB(d)}), nil
}

func (h *Handlers) RestoreOrg(ctx context.Context, req *connect.Request[vxv1.RestoreOrgRequest]) (*connect.Response[vxv1.RestoreOrgResponse], error) {
	o, err := h.svc.RestoreOrg(ctx, actorFrom(ctx), req.Msg.Handle)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RestoreOrgResponse{Org: orgPB(o, service.OrgOwner)}), nil
}

func (h *Handlers) GetOrgDeletion(ctx context.Context, req *connect.Request[vxv1.GetOrgDeletionRequest]) (*connect.Response[vxv1.GetOrgDeletionResponse], error) {
	d, err := h.svc.OrgDeletionRecord(ctx, actorFrom(ctx), req.Msg.Handle)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetOrgDeletionResponse{Deletion: h.deletionPB(d)}), nil
}

func (h *Handlers) ListDeletedOrgs(ctx context.Context, _ *connect.Request[vxv1.ListDeletedOrgsRequest]) (*connect.Response[vxv1.ListDeletedOrgsResponse], error) {
	ds, err := h.svc.DeletedOrgs(ctx, actorFrom(ctx))
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListDeletedOrgsResponse{}
	for _, d := range ds {
		out.Deletions = append(out.Deletions, h.deletionPB(d))
	}
	return connect.NewResponse(out), nil
}
