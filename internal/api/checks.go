package api

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// CheckDefPB converts a check definition.
func CheckDefPB(d rules.CheckDef) *vxv1.CheckDef {
	return &vxv1.CheckDef{
		Name: d.Name, When: string(d.When), Paths: d.Paths, Runner: d.Runner, Reporter: string(d.Reporter), Inputs: d.Inputs,
		OutputsRequired: d.Outputs.Required, OutputsArtifacts: d.Outputs.Artifacts, After: d.After, Blocks: string(d.Blocks),
		Timeout: rules.FormatTimeout(d.Timeout), Run: d.Run, Origin: string(d.Origin), Line: int32(d.Line), Describe: d.Describe(),
	}
}

func checkResultPB(r store.CheckResult, repo, effort string) *vxv1.CheckResult {
	out := &vxv1.CheckResult{
		Id: r.ID.String(), Repo: repo, Effort: effort, ChangeId: r.ChangeID, Tree: r.Tree.String(), Check: r.CheckName,
		Status: r.Status, Summary: r.Summary, LogSize: r.LogSize, DataJson: string(r.Data), DataName: r.DataName,
		Reporter: r.Reporter, OnBehalfOf: r.OnBehalfOf, Model: r.Model, How: r.How, Source: r.Source,
		StartedAt: tsp(r.StartedAt), FinishedAt: tsp(r.FinishedAt), CreatedAt: ts(r.CreatedAt),
	}
	if !r.LogHash.IsZero() {
		out.LogHash = r.LogHash.String()
	}
	if r.CITokenID != nil {
		out.CiTokenId = r.CITokenID.String()
	}
	for _, a := range r.Artifacts {
		out.Artifacts = append(out.Artifacts, &vxv1.CheckArtifact{Name: a.Name, ContentType: a.ContentType, Hash: a.Hash.String(), Size: a.Size})
	}
	return out
}

func optHash(field, s string) (hash.Hash, error) {
	if s == "" {
		return hash.Zero, nil
	}
	return parseHash(field, s)
}

func optTime(field, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %q is not an RFC 3339 time", field, s)
	}
	return t, nil
}

// ReportCheck implements CheckService.
func (h *Handlers) ReportCheck(ctx context.Context, req *connect.Request[vxv1.ReportCheckRequest]) (*connect.Response[vxv1.ReportCheckResponse], error) {
	m := req.Msg
	tree, err := optHash("tree", m.Tree)
	if err != nil {
		return nil, invalid(err)
	}
	started, err := optTime("started_at", m.StartedAt)
	if err != nil {
		return nil, invalid(err)
	}
	finished, err := optTime("finished_at", m.FinishedAt)
	if err != nil {
		return nil, invalid(err)
	}
	in := service.ReportInput{
		Repo: m.Repo, Effort: m.Effort, ChangeID: m.ChangeId, Tree: tree, Name: m.Check, Status: m.Status, Summary: m.Summary,
		Log: m.Log, Data: m.Data, DataName: m.DataName, How: m.How, Model: m.Model, Started: started, Finished: finished,
	}
	for _, a := range m.Artifacts {
		in.Artifacts = append(in.Artifacts, service.ArtifactInput{Name: a.Name, ContentType: a.ContentType, Data: a.Data})
	}
	r, err := h.svc.ReportCheck(ctx, actorFrom(ctx), in)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ReportCheckResponse{Result: countedPB(r, r.RepoName, r.EffortSlug)}), nil
}

func countedPB(r service.CheckResult, repo, effort string) *vxv1.CheckResult {
	out := checkResultPB(r.CheckResult, repo, effort)
	out.Counts, out.Why = r.Counts, r.Why
	return out
}

// CheckStatusPB converts what a change still needs.
func CheckStatusPB(v service.CheckStatusView) *vxv1.CheckStatusResponse {
	out := &vxv1.CheckStatusResponse{ChangeId: v.ChangeID, Tree: v.Tree.String(), Head: v.Head, Landed: v.Landed, Rules: v.Rules, Problem: v.Problem, Warnings: v.Warnings}
	for _, c := range v.Checks {
		st := &vxv1.CheckState{Def: CheckDefPB(c.Def), Applies: c.Applies, State: c.State, How: c.How}
		if c.Latest != nil {
			st.Latest = countedPB(*c.Latest, v.Repo, v.Effort)
		}
		for _, r := range c.Results {
			st.Results = append(st.Results, countedPB(r, v.Repo, v.Effort))
		}
		out.Checks = append(out.Checks, st)
	}
	for _, r := range v.Other {
		out.Other = append(out.Other, checkResultPB(r, v.Repo, v.Effort))
	}
	return out
}

// CheckStatus implements CheckService.
func (h *Handlers) CheckStatus(ctx context.Context, req *connect.Request[vxv1.CheckStatusRequest]) (*connect.Response[vxv1.CheckStatusResponse], error) {
	m := req.Msg
	tree, err := optHash("tree", m.Tree)
	if err != nil {
		return nil, invalid(err)
	}
	v, err := h.svc.CheckStatus(ctx, actorFrom(ctx), service.CheckStatusInput{Repo: m.Repo, Effort: m.Effort, ChangeID: m.ChangeId, Tree: tree})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(CheckStatusPB(v)), nil
}

// GetCheckResult implements CheckService.
func (h *Handlers) GetCheckResult(ctx context.Context, req *connect.Request[vxv1.GetCheckResultRequest]) (*connect.Response[vxv1.GetCheckResultResponse], error) {
	r, err := h.svc.GetCheckResult(ctx, actorFrom(ctx), req.Msg.Id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetCheckResultResponse{Result: checkResultPB(r.CheckResult, r.RepoName, r.EffortSlug)}), nil
}

// GetCheckArtifact implements CheckService.
func (h *Handlers) GetCheckArtifact(ctx context.Context, req *connect.Request[vxv1.GetCheckArtifactRequest]) (*connect.Response[vxv1.GetCheckArtifactResponse], error) {
	a, data, err := h.svc.CheckArtifact(ctx, actorFrom(ctx), req.Msg.Id, req.Msg.Name)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetCheckArtifactResponse{Artifact: &vxv1.CheckArtifact{
		Name: a.Name, ContentType: a.ContentType, Hash: a.Hash.String(), Size: a.Size, Data: data,
	}}), nil
}
