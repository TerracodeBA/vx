package api

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
)

// claimPB converts a claim for the caller, who decides its trust label.
func claimPB(caller service.Actor, c service.Claim) *vxv1.Claim {
	out := &vxv1.Claim{
		Id: c.ID.String(), Repo: c.RepoName, Effort: c.EffortSlug, Targets: c.Targets,
		Owner:  principalPB(service.Actor{Principal: c.Owner, Parent: c.OwnerParent}),
		Reason: c.Reason, CreatedAt: ts(c.CreatedAt), ExpiresAt: ts(c.ExpiresAt),
		Trust: service.ClaimTrust(caller, c),
	}
	if c.WorkspaceID != nil {
		out.WorkspaceId = c.WorkspaceID.String()
	}
	return out
}

func claimsPB(caller service.Actor, cs []service.Claim) []*vxv1.Claim {
	out := make([]*vxv1.Claim, 0, len(cs))
	for _, c := range cs {
		out = append(out, claimPB(caller, c))
	}
	return out
}

// taskPB converts a task for the caller.
func taskPB(caller service.Actor, t service.Task) *vxv1.Task {
	out := &vxv1.Task{
		Id: t.ID.String(), Repo: t.RepoName, Effort: t.EffortSlug, Title: t.Title, Body: t.Body,
		State: t.State, Blocked: t.Blocked,
		CreatedBy: principalPB(service.Actor{Principal: t.CreatedBy, Parent: t.CreatedParent}),
		CreatedAt: ts(t.CreatedAt), UpdatedAt: ts(t.UpdatedAt), Trust: service.TaskTrust(caller, t),
	}
	if t.Owner != nil {
		out.Owner = principalPB(service.Actor{Principal: *t.Owner})
	}
	for _, d := range t.DependsOn {
		out.DependsOn = append(out.DependsOn, d.String())
	}
	return out
}

func tasksPB(caller service.Actor, ts []service.Task) []*vxv1.Task {
	out := make([]*vxv1.Task, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskPB(caller, t))
	}
	return out
}

func (h *Handlers) Claim(ctx context.Context, req *connect.Request[vxv1.ClaimRequest]) (*connect.Response[vxv1.ClaimResponse], error) {
	actor := actorFrom(ctx)
	m := req.Msg
	c, others, err := h.svc.MakeClaim(ctx, actor, service.ClaimInput{
		Repo: m.Repo, Effort: m.Effort, Targets: m.Targets, Reason: m.Reason,
		TTL: time.Duration(m.TtlSeconds) * time.Second, WorkspaceID: strings.TrimSpace(m.WorkspaceId),
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ClaimResponse{Claim: claimPB(actor, c), Overlapping: claimsPB(actor, others)}), nil
}

func (h *Handlers) Release(ctx context.Context, req *connect.Request[vxv1.ReleaseRequest]) (*connect.Response[vxv1.ReleaseResponse], error) {
	actor := actorFrom(ctx)
	cs, err := h.svc.ReleaseClaims(ctx, actor, strings.TrimSpace(req.Msg.Id), strings.TrimSpace(req.Msg.WorkspaceId))
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ReleaseResponse{Released: claimsPB(actor, cs)}), nil
}

func (h *Handlers) ListClaims(ctx context.Context, req *connect.Request[vxv1.ListClaimsRequest]) (*connect.Response[vxv1.ListClaimsResponse], error) {
	actor := actorFrom(ctx)
	cs, err := h.svc.ListClaims(ctx, actor, req.Msg.Repo, req.Msg.Effort, req.Msg.Targets)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ListClaimsResponse{Claims: claimsPB(actor, cs)}), nil
}

func (h *Handlers) AddTask(ctx context.Context, req *connect.Request[vxv1.AddTaskRequest]) (*connect.Response[vxv1.AddTaskResponse], error) {
	actor := actorFrom(ctx)
	m := req.Msg
	t, err := h.svc.AddTask(ctx, actor, service.AddTaskInput{
		Repo: m.Repo, Effort: m.Effort, Title: m.Title, Body: m.Body, DependsOn: m.DependsOn, Owner: m.Owner,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.AddTaskResponse{Task: taskPB(actor, t)}), nil
}

func (h *Handlers) UpdateTask(ctx context.Context, req *connect.Request[vxv1.UpdateTaskRequest]) (*connect.Response[vxv1.UpdateTaskResponse], error) {
	actor := actorFrom(ctx)
	m := req.Msg
	t, unblocked, err := h.svc.UpdateTask(ctx, actor, service.UpdateTaskInput{
		ID: m.Id, Action: strings.TrimSpace(m.Action), Owner: m.Owner, Force: m.Force,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.UpdateTaskResponse{Task: taskPB(actor, t), Unblocked: tasksPB(actor, unblocked)}), nil
}

func (h *Handlers) ListTasks(ctx context.Context, req *connect.Request[vxv1.ListTasksRequest]) (*connect.Response[vxv1.ListTasksResponse], error) {
	actor := actorFrom(ctx)
	m := req.Msg
	t, err := h.svc.ListTasks(ctx, actor, service.ListTasksInput{Repo: m.Repo, Effort: m.Effort, States: m.States, Mine: m.Mine})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ListTasksResponse{Tasks: tasksPB(actor, t)}), nil
}
