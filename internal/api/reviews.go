package api

import (
	"context"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// Land statuses (LandResponse.status).
const (
	landStatusLanded    = "landed"
	landStatusPending   = "pending_review"
	landStatusConflicts = "conflicts"
	landStatusMoved     = "effort_moved"
)

// landResponsePB converts a land's result; effort is the effort's slug.
func landResponsePB(caller service.Actor, res service.LandResult, effort string) *vxv1.LandResponse {
	out := &vxv1.LandResponse{
		Landed: res.Landed, Rebased: res.Rebased, AutoMerged: res.AutoMerged,
		Conflicts: conflictsPB(res.Conflicts), EffortHead: res.EffortHead.String(), EffortMoved: res.EffortMoved,
		Warnings: warningsPB(res.Warnings), KeptBoth: res.KeptBoth, MergedByDecl: res.MergedByDecl, Overlaps: overlapsPB(res.Overlaps),
	}
	for _, c := range res.OverlappingClaims {
		out.OverlappingClaims = append(out.OverlappingClaims, claimPB(caller, c))
	}
	switch {
	case res.Landed:
		out.Status = landStatusLanded
		out.Changeset = changesetRowPB(res.Changeset, effort, res.Files)
	case res.Review != nil:
		out.Status = landStatusPending
		out.Review = reviewPB(*res.Review)
	case res.EffortMoved:
		out.Status = landStatusMoved
	default:
		out.Status = landStatusConflicts
	}
	return out
}

func reviewPB(r service.Review) *vxv1.Review {
	out := &vxv1.Review{
		Id: r.ID.String(), Repo: r.RepoName, Effort: r.EffortSlug, WorkspaceId: r.WorkspaceID.String(),
		ChangeId: r.ChangeID, Intent: r.Intent, Author: principalPB(service.Actor{Principal: r.Author, Parent: r.AuthorOwner}),
		Base: r.Base.String(), Tree: r.Tree.String(), Evidence: evidencePB(r.Evidence), Paths: r.Paths, Policies: r.Policies,
		State: string(r.State), Conflicts: r.Conflicts, DecidedAt: tsp(r.DecidedAt), Reason: r.Reason, Landed: r.Landed.String(),
		CreatedAt: ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt), CanDecide: r.CanDecide, CannotDecide: r.CannotDecide,
	}
	if r.State == store.ReviewPending {
		out.Waiting = r.WaitingText()
	}
	for _, l := range r.Approvers {
		out.Approvers = append(out.Approvers, &vxv1.ApproverList{Approvers: l})
	}
	if r.DecidedBy != nil {
		out.DecidedBy = principalPB(service.Actor{Principal: *r.DecidedBy})
	}
	if r.Files != nil {
		out.Files = fileChangesPB(r.Files)
	}
	return out
}

func (h *Handlers) ListReviews(ctx context.Context, req *connect.Request[vxv1.ListReviewsRequest]) (*connect.Response[vxv1.ListReviewsResponse], error) {
	m := req.Msg
	q := service.ReviewQuery{Repo: m.Repo, Effort: m.Effort, Limit: int(m.Limit)}
	for _, s := range m.States {
		switch st := store.ReviewState(s); st {
		case store.ReviewPending, store.ReviewApproved, store.ReviewRejected, store.ReviewWithdrawn:
			q.States = append(q.States, st)
		default:
			return nil, toConnect(&service.Error{Code: service.CodeInvalid, Msg: "review state " + s + " is not one of pending, approved, rejected, withdrawn"})
		}
	}
	rs, err := h.svc.ListReviews(ctx, actorFrom(ctx), q)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListReviewsResponse{}
	for _, r := range rs {
		out.Reviews = append(out.Reviews, reviewPB(r))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) GetReview(ctx context.Context, req *connect.Request[vxv1.GetReviewRequest]) (*connect.Response[vxv1.GetReviewResponse], error) {
	r, err := h.svc.GetReview(ctx, actorFrom(ctx), req.Msg.Id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetReviewResponse{Review: reviewPB(r)}), nil
}

func (h *Handlers) ApproveReview(ctx context.Context, req *connect.Request[vxv1.ApproveReviewRequest]) (*connect.Response[vxv1.ApproveReviewResponse], error) {
	actor := actorFrom(ctx)
	d, err := h.svc.ApproveReview(ctx, actor, req.Msg.Id)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ApproveReviewResponse{Review: reviewPB(d.Review), Conflicts: conflictsPB(d.Conflicts)}
	if d.Landed != nil {
		out.Land = landResponsePB(actor, *d.Landed, d.Review.EffortSlug)
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) RejectReview(ctx context.Context, req *connect.Request[vxv1.RejectReviewRequest]) (*connect.Response[vxv1.RejectReviewResponse], error) {
	d, err := h.svc.RejectReview(ctx, actorFrom(ctx), req.Msg.Id, req.Msg.Reason)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RejectReviewResponse{Review: reviewPB(d.Review)}), nil
}
