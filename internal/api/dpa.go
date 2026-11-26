package api

import (
	"context"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
)

// ---------- The data processing agreement (#41) ----------

func agreementVersionPB(v *service.AgreementVersion) *vxv1.AgreementVersion {
	if v == nil {
		return nil
	}
	return &vxv1.AgreementVersion{Id: v.ID, Title: v.Title, Published: ts(v.Published), Effective: ts(v.Effective), Sha256: v.SHA256}
}

func agreementAcceptancePB(r *service.AgreementRecord) *vxv1.AgreementAcceptance {
	if r == nil {
		return nil
	}
	return &vxv1.AgreementAcceptance{Id: r.ID.String(), Version: r.VersionID, Sha256: r.SHA256, AcceptedBy: r.AcceptedByRef,
		AcceptedAt: ts(r.AcceptedAt), Ip: r.IP, Known: r.Known}
}

// GetOrgAgreement returns an organization's agreement status and, on
// request, a text. There is deliberately no RPC to accept.
func (h *Handlers) GetOrgAgreement(ctx context.Context, req *connect.Request[vxv1.GetOrgAgreementRequest]) (*connect.Response[vxv1.GetOrgAgreementResponse], error) {
	actor := actorFrom(ctx)
	v, err := h.svc.OrgAgreement(ctx, actor, req.Msg.Org)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.GetOrgAgreementResponse{Required: v.Required}
	if !v.Required {
		return connect.NewResponse(out), nil
	}
	out.Current, out.CurrentAccepted, out.Usable = agreementVersionPB(v.Current), agreementAcceptancePB(v.CurrentAccepted), v.Usable
	for i := range v.Announced {
		out.Announced = append(out.Announced, agreementVersionPB(&v.Announced[i]))
		if _, ok := v.AnnouncedAccepted[v.Announced[i].ID]; ok {
			out.AnnouncedAccepted = append(out.AnnouncedAccepted, v.Announced[i].ID)
		}
	}
	out.Pending, out.Missing, out.Owners, out.CanAccept, out.AcceptUrl = agreementVersionPB(v.Pending), v.Missing, v.Owners, v.CanAccept, v.AcceptURL
	if out.AcceptUrl != "" && out.AcceptUrl[0] == '/' {
		out.AcceptUrl = h.baseURL(ctx) + out.AcceptUrl
	}
	for i := range v.History {
		out.History = append(out.History, agreementAcceptancePB(&v.History[i]))
	}
	if req.Msg.Text {
		if req.Msg.Acceptance != "" {
			rec, text, err := h.svc.AcceptedAgreementText(ctx, actor, req.Msg.Org, req.Msg.Acceptance)
			if err != nil {
				return nil, toConnect(err)
			}
			out.Text = text
			out.TextVersion = &vxv1.AgreementVersion{Id: rec.VersionID, Title: rec.Title, Sha256: rec.SHA256}
		} else {
			ver, err := h.svc.AgreementText(ctx, actor, req.Msg.Org, req.Msg.Version)
			if err != nil {
				return nil, toConnect(err)
			}
			out.Text, out.TextVersion = ver.Text, agreementVersionPB(&ver)
		}
	}
	return connect.NewResponse(out), nil
}
