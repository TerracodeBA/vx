package api

import (
	"context"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// ---------- Rebase ----------

func (h *Handlers) RebaseEffort(ctx context.Context, req *connect.Request[vxv1.RebaseEffortRequest]) (*connect.Response[vxv1.RebaseEffortResponse], error) {
	v, err := h.svc.RebaseEffort(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort, req.Msg.Onto)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.RebaseEffortResponse{Effort: h.rebaseEffortPB(ctx, v), UpToDate: v.UpToDate}
	if !v.UpToDate {
		out.Rebase = rebasePB(v)
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) ContinueRebase(ctx context.Context, req *connect.Request[vxv1.ContinueRebaseRequest]) (*connect.Response[vxv1.ContinueRebaseResponse], error) {
	at, err := parseHash("stopped_at", req.Msg.StoppedAt)
	if err != nil {
		return nil, invalid(err)
	}
	tree, err := parseHash("tree", req.Msg.Tree)
	if err != nil {
		return nil, invalid(err)
	}
	v, err := h.svc.ContinueRebase(ctx, actorFrom(ctx), service.ContinueInput{Repo: req.Msg.Repo, Effort: req.Msg.Effort, StoppedAt: at, Tree: tree})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ContinueRebaseResponse{Rebase: rebasePB(v), Effort: h.rebaseEffortPB(ctx, v)}), nil
}

func (h *Handlers) AbortRebase(ctx context.Context, req *connect.Request[vxv1.AbortRebaseRequest]) (*connect.Response[vxv1.AbortRebaseResponse], error) {
	v, err := h.svc.AbortRebase(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.AbortRebaseResponse{Rebase: rebasePB(v), Effort: h.rebaseEffortPB(ctx, v)}), nil
}

func (h *Handlers) GetRebase(ctx context.Context, req *connect.Request[vxv1.GetRebaseRequest]) (*connect.Response[vxv1.GetRebaseResponse], error) {
	v, ok, err := h.svc.GetRebase(ctx, req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.GetRebaseResponse{}
	if ok {
		out.Rebase = rebasePB(v)
	}
	return connect.NewResponse(out), nil
}

// rebaseEffortPB is the effort after a rebase call, with the rebase while it
// is in progress.
func (h *Handlers) rebaseEffortPB(ctx context.Context, v service.RebaseView) *vxv1.Effort {
	open, _ := h.svc.Store.ListWorkspaces(ctx, v.Effort.ID, store.WorkspaceOpen)
	e := effortPB(v.Repo.Name, v.Effort, h.ref(ctx, v.Effort.OwnerID), len(open))
	if !v.UpToDate && v.Rebase.State.Active() {
		e.Rebase = rebasePB(v)
	}
	return e
}

// activeRebasePB converts the rebase in progress of an effort view.
func (h *Handlers) activeRebasePB(ctx context.Context, v service.EffortView) *vxv1.Rebase {
	if v.Rebase == nil {
		return nil
	}
	rv, ok, err := h.svc.GetRebase(ctx, v.Repo.Name, v.Effort.Slug)
	if err != nil || !ok {
		return nil
	}
	return rebasePB(rv)
}

func rebasePB(v service.RebaseView) *vxv1.Rebase {
	r := v.Rebase
	out := &vxv1.Rebase{
		Id: r.ID.String(), State: string(r.State), OntoBranch: r.OntoBranch, Onto: r.Onto.String(), OntoGitSha: r.OntoGitSHA,
		OldBase: r.OldBase.String(), OldHead: r.OldHead.String(), NewHead: r.NewHead.String(),
		Replayed: int32(len(r.Replayed)), Pending: int32(len(r.Pending)),
		StartedBy: v.StartedBy, UpdatedBy: v.UpdatedBy, CreatedAt: ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt),
	}
	if v.StoppedAt != nil {
		out.StoppedAt = changesetRowPB(*v.StoppedAt, v.Effort.Slug, nil)
		out.BaseTree, out.ChangeTree, out.OntoTree = v.BaseTree.String(), v.ChangeTree.String(), v.OntoTree.String()
	}
	for _, c := range r.Conflicts {
		out.Conflicts = append(out.Conflicts, &vxv1.RebaseConflict{Path: c.Path, Kind: c.Kind, Detail: c.Detail, Generated: c.Generated})
	}
	return out
}
