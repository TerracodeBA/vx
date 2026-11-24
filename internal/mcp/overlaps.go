package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/TerracodeBA/vx/internal/service"
)

// Overlap is a forecast collision between the effort and another open
// effort: what happens when both ship.
type Overlap struct {
	Effort      string    `json:"effort" jsonschema:"the effort you asked about"`
	OtherEffort string    `json:"other_effort" jsonschema:"the other effort's slug"`
	OtherName   string    `json:"other_name"`
	OtherOwner  string    `json:"other_owner" jsonschema:"principal that owns the other effort"`
	Status      string    `json:"status" jsonschema:"clean (no shared files), overlap (shared files merge cleanly or a rule breaks) or conflict"`
	SharedFiles []string  `json:"shared_files" jsonschema:"files both efforts changed since their bases"`
	Conflicts   []string  `json:"conflicts" jsonschema:"shared files that would not merge when both ship"`
	Warnings    []Warning `json:"warnings" jsonschema:"what the combined code breaks: duplicate Go declarations (kind duplicate), vx.toml rules such as migration numbers (kind rule)"`
	CheckedAt   string    `json:"checked_at"`
}

type overlapsInput struct {
	Repo   string `json:"repo" jsonschema:"repository name"`
	Effort string `json:"effort,omitempty" jsonschema:"effort slug; empty: every pair of open efforts in the repository"`
	All    bool   `json:"all,omitempty" jsonschema:"also return clean pairs"`
}

type overlapsOutput struct {
	Overlaps []Overlap `json:"overlaps"`
}

func (t *tools) registerOverlaps(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "vx_overlaps",
		Description: "Forecast what happens when this effort and the other open efforts of the repository (same base branch) " +
			"both ship: files both change, files that would not merge, and what the combined code breaks (duplicate Go " +
			"declarations, vx.toml rules such as two migrations with one number). vx recomputes it after every land and when " +
			"the base branch moves, and posts overlap notes on both boards. Check it before editing shared files and before " +
			"shipping; on a conflict, agree with the other effort's owner (vx_ask) who changes what.",
		Annotations: readOnly("Overlaps with other efforts"),
	}, t.overlaps)
}

func (t *tools) overlaps(ctx context.Context, req *sdk.CallToolRequest, in overlapsInput) (*sdk.CallToolResult, overlapsOutput, error) {
	a, err := actorOf(req)
	if err != nil {
		return nil, overlapsOutput{}, err
	}
	if err := need(in.Repo, "repo"); err != nil {
		return nil, overlapsOutput{}, err
	}
	os, err := t.svc.ListOverlaps(ctx, a, in.Repo, in.Effort)
	if err != nil {
		return nil, overlapsOutput{}, t.userError("vx_overlaps", err)
	}
	out := overlapsOutput{Overlaps: []Overlap{}}
	for _, o := range os {
		if o.Status == service.OverlapClean && !in.All {
			continue
		}
		out.Overlaps = append(out.Overlaps, overlap(o))
	}
	return nil, out, nil
}

func overlap(o service.Overlap) Overlap {
	out := Overlap{
		Effort: o.Effort.Slug, OtherEffort: o.Other.Slug, OtherName: o.Other.Name, OtherOwner: o.OtherOwner.Ref(),
		Status: o.Status, SharedFiles: o.SharedFiles, Conflicts: o.Conflicts, Warnings: []Warning{}, CheckedAt: ts(o.CheckedAt),
	}
	if out.SharedFiles == nil {
		out.SharedFiles = []string{}
	}
	if out.Conflicts == nil {
		out.Conflicts = []string{}
	}
	for _, w := range o.Warnings {
		out.Warnings = append(out.Warnings, Warning{Path: w.Path, Kind: string(w.Kind), Detail: w.Detail})
	}
	return out
}

func overlapList(os []service.Overlap) []Overlap {
	out := make([]Overlap, 0, len(os))
	for _, o := range os {
		out = append(out, overlap(o))
	}
	return out
}
