package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// CheckResult is one result of a check as the tools return it.
type CheckResult struct {
	ID         string          `json:"id"`
	Check      string          `json:"check" jsonschema:"the check; empty: a vx land --run command no check names"`
	Status     string          `json:"status" jsonschema:"pass, fail, neutral or pending"`
	ChangeID   string          `json:"change_id"`
	Tree       string          `json:"tree" jsonschema:"the exact tree the result describes"`
	Summary    string          `json:"summary,omitempty"`
	Reporter   string          `json:"reporter" jsonschema:"who reported it, e.g. agent:alice/claude-review"`
	OnBehalfOf string          `json:"on_behalf_of,omitempty"`
	Model      string          `json:"model,omitempty"`
	How        string          `json:"how,omitempty" jsonschema:"the command, when vx ran a run check"`
	Source     string          `json:"source" jsonschema:"report, land (vx land --run) or review (a person approved under a human policy)"`
	LogSize    int64           `json:"log_size,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	DataName   string          `json:"data_name,omitempty"`
	Artifacts  []ArtifactInfo  `json:"artifacts,omitempty"`
	CreatedAt  string          `json:"created_at"`
}

// ArtifactInfo names a file attached to a result.
type ArtifactInfo struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// CheckInfo is one check and its state on a change.
type CheckInfo struct {
	Name       string       `json:"name"`
	Describe   string       `json:"describe" jsonschema:"where it is defined"`
	When       string       `json:"when" jsonschema:"land, ship or manual"`
	Blocks     string       `json:"blocks" jsonschema:"land, ship or nothing: what it stops while it has no passing result"`
	Reporter   string       `json:"reporter" jsonschema:"whose results count: self, independent, ci or human"`
	Runner     string       `json:"runner,omitempty"`
	Run        string       `json:"run,omitempty" jsonschema:"the command of a run check (vx land --run)"`
	Applies    bool         `json:"applies" jsonschema:"the change touches the check's paths"`
	State      string       `json:"state" jsonschema:"missing, pending, passed, failed, neutral, or not needed (the change touches none of its paths)"`
	Latest     *CheckResult `json:"latest,omitempty" jsonschema:"the newest result that counts"`
	NotCounted []string     `json:"not_counted,omitempty" jsonschema:"newer results that do not count, and why"`
	How        string       `json:"how" jsonschema:"how to provide a result"`
}

type checkStatusOutput struct {
	Warnings []string      `json:"warnings,omitempty" jsonschema:"checks left out"`
	ChangeID string        `json:"change_id"`
	Tree     string        `json:"tree"`
	Head     bool          `json:"head" jsonschema:"the change is the effort head"`
	Checks   []CheckInfo   `json:"checks"`
	Other    []CheckResult `json:"other,omitempty" jsonschema:"results on this tree for no check of vx.toml"`
	Problem  string        `json:"problem,omitempty" jsonschema:"vx.toml does not parse"`
}

type checkStatusInput struct {
	Repo     string `json:"repo" jsonschema:"repository name"`
	Effort   string `json:"effort" jsonschema:"effort slug"`
	ChangeID string `json:"change_id,omitempty" jsonschema:"change id; empty: the effort head"`
	Tree     string `json:"tree,omitempty" jsonschema:"the tree to look at; empty: the change's newest (a change not landed yet needs its snapshot tree)"`
}

type artifactIn struct {
	Name        string `json:"name" jsonschema:"file name without directories, e.g. flow.gif"`
	ContentType string `json:"content_type,omitempty" jsonschema:"e.g. image/gif; empty: from the name"`
	Base64      string `json:"base64" jsonschema:"the file's bytes, base64"`
}

type checkReportInput struct {
	Repo      string          `json:"repo" jsonschema:"repository name"`
	Effort    string          `json:"effort" jsonschema:"effort slug"`
	ChangeID  string          `json:"change_id,omitempty" jsonschema:"change id; empty: the effort head"`
	Tree      string          `json:"tree,omitempty" jsonschema:"the exact tree checked; empty: the change's newest tree"`
	Check     string          `json:"check" jsonschema:"the check's name in vx.toml"`
	Status    string          `json:"status" jsonschema:"pass, fail, neutral or pending"`
	Summary   string          `json:"summary" jsonschema:"what was checked and found"`
	Log       string          `json:"log,omitempty" jsonschema:"the full log or report, as text"`
	Artifacts []artifactIn    `json:"artifacts,omitempty" jsonschema:"files the check produced (screenshots, recordings, reports)"`
	Data      json.RawMessage `json:"data,omitempty" jsonschema:"structured output, JSON, as the check's outputs require"`
	DataName  string          `json:"data_name,omitempty" jsonschema:"the name of the data, e.g. steps.json"`
	Model     string          `json:"model,omitempty"`
}

type checkShowInput struct {
	ID       string `json:"id" jsonschema:"check result id or a prefix of at least 8 characters"`
	Artifact string `json:"artifact,omitempty" jsonschema:"return this artifact's bytes (base64); 'log' for the log"`
}

type checkShowOutput struct {
	Result   CheckResult `json:"result"`
	Artifact string      `json:"artifact,omitempty" jsonschema:"the artifact asked for"`
	Base64   string      `json:"base64,omitempty" jsonschema:"its bytes, base64"`
	Text     string      `json:"text,omitempty" jsonschema:"the log, as text"`
}

// maxMCPArtifact bounds what vx_check_show returns inline.
const maxMCPArtifact = 4 << 20

func (t *tools) registerChecks(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "vx_check_status",
		Description: "What a change still needs: each check of vx.toml ([[check]], required checks, landing policies) with its state on the change's exact tree " +
			"(missing, pending, passed, failed, neutral), who reported the result that counts, and how to provide a missing one.",
		Annotations: readOnly("Check status"),
	}, t.checkStatus)
	sdk.AddTool(srv, &sdk.Tool{
		Name: "vx_check_report",
		Description: "Report a result of a check vx.toml defines, bound to a change and the exact tree it describes, with a log, artifacts and JSON data. " +
			"Whether it counts depends on the check's reporter (self, independent, ci, human); run checks count only when vx ran their command (vx land --run, vx check run).",
		Annotations: &sdk.ToolAnnotations{Title: "Report a check result"},
	}, t.checkReport)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "vx_check_show",
		Description: "Show one check result; with artifact, also return that artifact's bytes (base64) or the log ('log').",
		Annotations: readOnly("Check result"),
	}, t.checkShow)
}

func checkResultOf(r store.CheckResult) CheckResult {
	out := CheckResult{
		ID: r.ID.String(), Check: r.CheckName, Status: r.Status, ChangeID: r.ChangeID, Tree: r.Tree.String(), Summary: r.Summary,
		Reporter: r.Reporter, OnBehalfOf: r.OnBehalfOf, Model: r.Model, How: r.How, Source: r.Source, LogSize: r.LogSize,
		Data: r.Data, DataName: r.DataName, CreatedAt: ts(r.CreatedAt),
	}
	for _, a := range r.Artifacts {
		out.Artifacts = append(out.Artifacts, ArtifactInfo{Name: a.Name, ContentType: a.ContentType, Size: a.Size})
	}
	return out
}

func optHash(s string) (hash.Hash, error) {
	if s == "" {
		return hash.Zero, nil
	}
	return hash.Parse(s)
}

func (t *tools) checkStatus(ctx context.Context, req *sdk.CallToolRequest, in checkStatusInput) (*sdk.CallToolResult, checkStatusOutput, error) {
	a, err := actorOf(req)
	if err != nil {
		return nil, checkStatusOutput{}, err
	}
	if err := need(in.Repo, "repo", in.Effort, "effort"); err != nil {
		return nil, checkStatusOutput{}, err
	}
	tree, err := optHash(in.Tree)
	if err != nil {
		return nil, checkStatusOutput{}, fmt.Errorf("tree: %w", err)
	}
	v, err := t.svc.CheckStatus(ctx, a, service.CheckStatusInput{Repo: in.Repo, Effort: in.Effort, ChangeID: in.ChangeID, Tree: tree})
	if err != nil {
		return nil, checkStatusOutput{}, t.userError("vx_check_status", err)
	}
	out := checkStatusOutput{ChangeID: v.ChangeID, Tree: v.Tree.String(), Head: v.Head, Checks: []CheckInfo{}, Problem: v.Problem, Warnings: v.Warnings}
	for _, c := range v.Checks {
		ci := CheckInfo{Name: c.Def.Name, Describe: c.Def.Describe(), When: string(c.Def.When), Blocks: string(c.Def.Blocks),
			Reporter: string(c.Def.Reporter), Runner: c.Def.Runner, Run: c.Def.Run, Applies: c.Applies, State: c.State, How: c.How}
		if c.Latest != nil {
			r := checkResultOf(c.Latest.CheckResult)
			ci.Latest = &r
		}
		for _, n := range c.NotCounted() {
			ci.NotCounted = append(ci.NotCounted, fmt.Sprintf("%s by %s: %s", n.Status, n.Reporter, n.Why))
		}
		out.Checks = append(out.Checks, ci)
	}
	for _, r := range v.Other {
		out.Other = append(out.Other, checkResultOf(r))
	}
	return nil, out, nil
}

func (t *tools) checkReport(ctx context.Context, req *sdk.CallToolRequest, in checkReportInput) (*sdk.CallToolResult, CheckResult, error) {
	a, err := actorOf(req)
	if err != nil {
		return nil, CheckResult{}, err
	}
	if err := need(in.Repo, "repo", in.Effort, "effort", in.Check, "check", in.Status, "status"); err != nil {
		return nil, CheckResult{}, err
	}
	tree, err := optHash(in.Tree)
	if err != nil {
		return nil, CheckResult{}, fmt.Errorf("tree: %w", err)
	}
	ri := service.ReportInput{Repo: in.Repo, Effort: in.Effort, ChangeID: in.ChangeID, Tree: tree, Name: in.Check, Status: in.Status,
		Summary: in.Summary, Log: []byte(in.Log), DataName: in.DataName, Model: in.Model}
	if len(in.Data) > 0 && string(in.Data) != "null" {
		ri.Data = in.Data
	}
	for _, x := range in.Artifacts {
		data, err := base64.StdEncoding.DecodeString(x.Base64)
		if err != nil {
			return nil, CheckResult{}, fmt.Errorf("artifact %s: base64: %w", x.Name, err)
		}
		ri.Artifacts = append(ri.Artifacts, service.ArtifactInput{Name: x.Name, ContentType: x.ContentType, Data: data})
	}
	r, err := t.svc.ReportCheck(ctx, a, ri)
	if err != nil {
		return nil, CheckResult{}, t.userError("vx_check_report", err)
	}
	return nil, checkResultOf(r.CheckResult), nil
}

func (t *tools) checkShow(ctx context.Context, req *sdk.CallToolRequest, in checkShowInput) (*sdk.CallToolResult, checkShowOutput, error) {
	a, err := actorOf(req)
	if err != nil {
		return nil, checkShowOutput{}, err
	}
	if err := need(in.ID, "id"); err != nil {
		return nil, checkShowOutput{}, err
	}
	r, err := t.svc.GetCheckResult(ctx, a, in.ID)
	if err != nil {
		return nil, checkShowOutput{}, t.userError("vx_check_show", err)
	}
	out := checkShowOutput{Result: checkResultOf(r.CheckResult)}
	if in.Artifact == "" {
		return nil, out, nil
	}
	name := in.Artifact
	if name == "log" {
		name = ""
	}
	art, data, err := t.svc.CheckArtifact(ctx, a, r.ID.String(), name)
	if err != nil {
		return nil, checkShowOutput{}, t.userError("vx_check_show", err)
	}
	if len(data) > maxMCPArtifact {
		return nil, checkShowOutput{}, fmt.Errorf("%s is %d bytes, more than vx_check_show returns (%d): download it with vx check show %s --download DIR", art.Name, len(data), maxMCPArtifact, r.ID.String()[:8])
	}
	out.Artifact = art.Name
	if name == "" {
		out.Text = string(data)
	} else {
		out.Base64 = base64.StdEncoding.EncodeToString(data)
	}
	return nil, out, nil
}
