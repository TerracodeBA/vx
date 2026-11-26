package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
)

// ---------- Check queue (#47) ----------

func checkJobPB(v service.CheckJobView) *vxv1.CheckJob {
	j := v.CheckJob
	out := &vxv1.CheckJob{
		Id: j.ID.String(), Repo: j.RepoName, Effort: j.EffortSlug, Check: j.Check, Runner: j.Runner, Reporter: j.Reporter,
		Trigger: j.Trigger, ChangeId: j.ChangeID, BaseTree: j.BaseTree.String(), Tree: j.Tree.String(),
		Author: j.AuthorPrincipal, OnBehalfOf: j.AuthorPerson, State: j.State, Attempts: int32(j.Attempts), MaxAttempts: int32(j.MaxAttempts),
		TimeoutSeconds: int64(j.Timeout / time.Second), LeaseOwner: v.LeaseOwnerRef, Reason: j.Reason, ResultId: j.ResultID,
		Values: j.Values, Inputs: j.Inputs, CreatedAt: ts(j.CreatedAt), UpdatedAt: ts(j.UpdatedAt),
	}
	if !j.Changeset.IsZero() {
		out.Changeset = j.Changeset.String()
	}
	if v.LeaseOwnerRef != "" {
		out.LeaseExpiresAt = tsp(j.LeaseExpiresAt)
	}
	if j.SupersededBy != nil {
		out.SupersededBy = j.SupersededBy.String()
	}
	if in := v.Inputs; in != nil {
		b := &vxv1.CheckJobInputs{
			Effort: &vxv1.CheckJobEffort{Repo: in.Effort.Repo, Slug: in.Effort.Slug, Name: in.Effort.Name, Goal: in.Effort.Goal, BaseBranch: in.Effort.Branch},
			Change: &vxv1.CheckJobChange{Id: in.Change.ID, Changeset: in.Change.Changeset, Intent: in.Change.Intent, Author: in.Change.Author,
				OnBehalfOf: in.Change.OnBehalfOf, BaseTree: in.Change.BaseTree, Tree: in.Change.Tree},
			Diff: in.Diff, DiffCut: in.DiffCut, Values: in.Values, Missing: in.Missing,
		}
		for _, f := range in.ChangedFiles {
			b.ChangedFiles = append(b.ChangedFiles, &vxv1.CheckJobFile{Path: f.Path, Status: f.Status, Hash: f.Hash, Mode: f.Mode})
		}
		for _, f := range in.Files {
			b.Files = append(b.Files, &vxv1.CheckJobFile{Path: f.Path, Status: f.Status, Hash: f.Hash, Mode: f.Mode})
		}
		out.Bundle = b
	}
	return out
}

func ciTokenPB(v service.CITokenView) *vxv1.CIToken {
	return &vxv1.CIToken{Id: v.ID.String(), Repo: v.RepoName, Name: v.Name, Principal: v.Ref(), Checks: v.Checks,
		CreatedBy: v.CreatedByRef, CreatedAt: ts(v.CreatedAt), RevokedAt: tsp(v.RevokedAt)}
}

func (h *Handlers) RequestCheck(ctx context.Context, req *connect.Request[vxv1.RequestCheckRequest]) (*connect.Response[vxv1.RequestCheckResponse], error) {
	m := req.Msg
	v, err := h.svc.RequestCheck(ctx, actorFrom(ctx), service.RequestCheckInput{Repo: m.Repo, Effort: m.Effort, Check: m.Check, ChangeID: m.ChangeId, Values: m.Values})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RequestCheckResponse{Job: checkJobPB(v)}), nil
}

func (h *Handlers) NextCheckJob(ctx context.Context, req *connect.Request[vxv1.NextCheckJobRequest]) (*connect.Response[vxv1.NextCheckJobResponse], error) {
	m := req.Msg
	v, err := h.svc.NextCheckJob(ctx, actorFrom(ctx), service.NextJobInput{Repo: m.Repo, Effort: m.Effort, Runner: m.Runner})
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.NextCheckJobResponse{}
	if v != nil {
		out.Job = checkJobPB(*v)
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) GetCheckJob(ctx context.Context, req *connect.Request[vxv1.GetCheckJobRequest]) (*connect.Response[vxv1.GetCheckJobResponse], error) {
	v, err := h.svc.GetCheckJob(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetCheckJobResponse{Job: checkJobPB(v)}), nil
}

func (h *Handlers) RenewCheckJob(ctx context.Context, req *connect.Request[vxv1.RenewCheckJobRequest]) (*connect.Response[vxv1.RenewCheckJobResponse], error) {
	m := req.Msg
	v, err := h.svc.RenewCheckJob(ctx, actorFrom(ctx), m.Repo, m.Id, time.Duration(m.Seconds)*time.Second)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RenewCheckJobResponse{Job: checkJobPB(v)}), nil
}

func (h *Handlers) ReleaseCheckJob(ctx context.Context, req *connect.Request[vxv1.ReleaseCheckJobRequest]) (*connect.Response[vxv1.ReleaseCheckJobResponse], error) {
	m := req.Msg
	v, err := h.svc.ReleaseCheckJob(ctx, actorFrom(ctx), m.Repo, m.Id, m.Reason)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ReleaseCheckJobResponse{Job: checkJobPB(v)}), nil
}

func (h *Handlers) ListCheckJobs(ctx context.Context, req *connect.Request[vxv1.ListCheckJobsRequest]) (*connect.Response[vxv1.ListCheckJobsResponse], error) {
	m := req.Msg
	vs, err := h.svc.ListCheckJobs(ctx, actorFrom(ctx), service.ListJobsInput{Repo: m.Repo, Effort: m.Effort, States: m.States, Runner: m.Runner, Limit: int(m.Limit)})
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListCheckJobsResponse{}
	for _, v := range vs {
		out.Jobs = append(out.Jobs, checkJobPB(v))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) GetAgentRunner(ctx context.Context, req *connect.Request[vxv1.GetAgentRunnerRequest]) (*connect.Response[vxv1.GetAgentRunnerResponse], error) {
	v, err := h.svc.GetAgentRunner(ctx, actorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetAgentRunnerResponse{Agent: principalPB(service.Actor{Principal: v.Agent}), Kinds: v.Kinds}), nil
}

func (h *Handlers) SetAgentRunner(ctx context.Context, req *connect.Request[vxv1.SetAgentRunnerRequest]) (*connect.Response[vxv1.SetAgentRunnerResponse], error) {
	v, err := h.svc.SetAgentRunner(ctx, actorFrom(ctx), req.Msg.Agent, req.Msg.Kinds)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.SetAgentRunnerResponse{Agent: principalPB(service.Actor{Principal: v.Agent}), Kinds: v.Kinds}), nil
}

func (h *Handlers) CreateCIToken(ctx context.Context, req *connect.Request[vxv1.CreateCITokenRequest]) (*connect.Response[vxv1.CreateCITokenResponse], error) {
	m := req.Msg
	v, secret, err := h.svc.CreateCIToken(ctx, actorFrom(ctx), m.Repo, m.Name, m.Checks)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.CreateCITokenResponse{Token: ciTokenPB(v), Secret: secret}), nil
}

func (h *Handlers) ListCITokens(ctx context.Context, req *connect.Request[vxv1.ListCITokensRequest]) (*connect.Response[vxv1.ListCITokensResponse], error) {
	vs, err := h.svc.ListCITokens(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Revoked)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListCITokensResponse{}
	for _, v := range vs {
		out.Tokens = append(out.Tokens, ciTokenPB(v))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) RevokeCIToken(ctx context.Context, req *connect.Request[vxv1.RevokeCITokenRequest]) (*connect.Response[vxv1.RevokeCITokenResponse], error) {
	v, err := h.svc.RevokeCIToken(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Name)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RevokeCITokenResponse{Token: ciTokenPB(v)}), nil
}
