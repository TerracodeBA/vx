package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/TerracodeBA/vx/internal/service"
)

// Rules is a repository's vx.toml as tools return it.
type Rules struct {
	Source         string          `json:"source" jsonschema:"where the rules come from, e.g. 'vx.toml at effort x 3f2a1c9d0e7b'; empty: no vx.toml"`
	Unique         []UniqueRule    `json:"unique" jsonschema:"files matching glob may not share the key; reserve a key with vx_reserve"`
	Generated      []GeneratedRule `json:"generated" jsonschema:"files vx never text-merges; regenerate them with command"`
	RequiredChecks []string        `json:"required_checks" jsonschema:"commands to run with vx land --run; others are recorded as not verified"`
	Policies       []LandingPolicy `json:"policies" jsonschema:"how changes to some paths land; the strictest matching policy applies, fast for paths no policy covers"`
	Reservations   []Reservation   `json:"reservations" jsonschema:"keys handed out by vx_reserve, newest first"`
}

// LandingPolicy is a [[policy]] of vx.toml.
type LandingPolicy struct {
	Paths     []string `json:"paths"`
	Mode      string   `json:"mode" jsonschema:"fast: lands at once; gated: vx land refuses unless every check of require ran with --run and passed; human: also waits for a person to approve (vx land prints Waiting for a person to review); agents cannot approve"`
	Require   []string `json:"require,omitempty" jsonschema:"checks a gated or human land must run with vx land --run"`
	Approvers []string `json:"approvers,omitempty" jsonschema:"human: who may approve (user:HANDLE, team:ORG/TEAM); empty: people with write"`
}

// UniqueRule is a [[unique]] rule.
type UniqueRule struct {
	Name string `json:"name"`
	Glob string `json:"glob"`
	Key  string `json:"key" jsonschema:"regular expression; its first group, matched on the file name, is the key"`
}

// GeneratedRule is a [[generated]] rule.
type GeneratedRule struct {
	Glob    string `json:"glob"`
	Command string `json:"command,omitempty"`
}

// Reservation is a key vx_reserve handed out.
type Reservation struct {
	Rule       string `json:"rule"`
	Value      string `json:"value"`
	Effort     string `json:"effort,omitempty"`
	ReservedBy string `json:"reserved_by,omitempty"`
	CreatedAt  string `json:"created_at"`
}

type rulesInput struct {
	Repo   string `json:"repo" jsonschema:"repository name"`
	Effort string `json:"effort,omitempty" jsonschema:"effort slug: the rules at its head; empty: the default branch"`
}

type reserveInput struct {
	Repo   string `json:"repo" jsonschema:"repository name"`
	Effort string `json:"effort" jsonschema:"effort slug the key is for"`
	Rule   string `json:"rule" jsonschema:"name of a [[unique]] rule, e.g. migration"`
}

type reserveOutput struct {
	Rule    string `json:"rule"`
	Value   string `json:"value" jsonschema:"the reserved key, formatted like the existing ones, e.g. 017"`
	Example string `json:"example" jsonschema:"a file name using it, e.g. internal/store/migrations/017_<name>.sql"`
}

func (t *tools) registerRules(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "vx_rules",
		Description: "Show the repository's vx.toml rules: [[unique]] keys files may not share (e.g. migration numbers), " +
			"[[generated]] files vx never text-merges, required checks for vx land --run, and the keys reserved so far.",
		Annotations: readOnly("Repository rules"),
	}, t.rules)
	sdk.AddTool(srv, &sdk.Tool{
		Name: "vx_reserve",
		Description: "Reserve the next free key of a vx.toml [[unique]] rule for an effort, e.g. the next migration number, " +
			"across the target branch, every open effort and earlier reservations. Do this before adding such a file.",
		Annotations: &sdk.ToolAnnotations{Title: "Reserve a key"},
	}, t.reserve)
}

func (t *tools) rules(ctx context.Context, req *sdk.CallToolRequest, in rulesInput) (*sdk.CallToolResult, Rules, error) {
	a, err := actorOf(req)
	if err != nil {
		return nil, Rules{}, err
	}
	if err := need(in.Repo, "repo"); err != nil {
		return nil, Rules{}, err
	}
	r, err := t.svc.GetRules(ctx, a, in.Repo, in.Effort)
	if err != nil {
		return nil, Rules{}, t.userError("vx_rules", err)
	}
	res, err := t.svc.ListReservations(ctx, a, in.Repo, "")
	if err != nil {
		return nil, Rules{}, t.userError("vx_rules", err)
	}
	return nil, rulesOf(r, res), nil
}

func rulesOf(r service.Rules, res []service.Reservation) Rules {
	out := Rules{Source: r.Source, Unique: []UniqueRule{}, Generated: []GeneratedRule{}, RequiredChecks: r.RequiredChecks, Policies: []LandingPolicy{}, Reservations: []Reservation{}}
	if out.RequiredChecks == nil {
		out.RequiredChecks = []string{}
	}
	for _, u := range r.Unique {
		out.Unique = append(out.Unique, UniqueRule{Name: u.Name, Glob: u.Glob, Key: u.Key})
	}
	for _, g := range r.Generated {
		out.Generated = append(out.Generated, GeneratedRule{Glob: g.Glob, Command: g.Command})
	}
	for _, p := range r.Policies {
		out.Policies = append(out.Policies, LandingPolicy{Paths: p.Paths, Mode: string(p.Mode), Require: p.Require, Approvers: p.Approvers})
	}
	for _, x := range res {
		out.Reservations = append(out.Reservations, Reservation{Rule: x.Rule, Value: x.Value, Effort: x.EffortSlug, ReservedBy: x.ReservedBy, CreatedAt: ts(x.CreatedAt)})
	}
	return out
}

func (t *tools) reserve(ctx context.Context, req *sdk.CallToolRequest, in reserveInput) (*sdk.CallToolResult, reserveOutput, error) {
	a, err := actorOf(req)
	if err != nil {
		return nil, reserveOutput{}, err
	}
	if err := need(in.Repo, "repo", in.Effort, "effort", in.Rule, "rule"); err != nil {
		return nil, reserveOutput{}, err
	}
	v, ex, err := t.svc.Reserve(ctx, a, in.Repo, in.Effort, in.Rule)
	if err != nil {
		return nil, reserveOutput{}, t.userError("vx_reserve", err)
	}
	return nil, reserveOutput{Rule: in.Rule, Value: v, Example: ex}, nil
}
