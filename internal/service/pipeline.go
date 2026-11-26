package service

// Pipelines on the server (#47 part 3): when a result arrives, the checks
// that run after it and are now ready become jobs carrying the earlier
// checks' outputs as inputs, and the effort's GitHub check runs are
// updated. Checks after a failure are not queued: rules.Pipeline shows
// them as skipped, with the reason, in vx check status, on the web and on
// GitHub.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// pipelineAdvance runs after a result for def on tree was stored (the
// check queue's afterCheckResult calls it last). Errors are logged: the
// result is stored either way.
func (s *Service) pipelineAdvance(ctx context.Context, repo store.Repo, effortID uuid.UUID, check string, tree hash.Hash, resultID uuid.UUID) {
	effort, err := s.Store.EffortByID(ctx, effortID)
	if err != nil {
		slog.Default().Warn("pipeline: effort", "repo", repo.Name, "err", err)
		return
	}
	changeID := ""
	view, err := s.checkStatusOf(ctx, repo, effort, "", tree)
	for _, c := range view.Checks {
		if c.Latest != nil && c.Latest.ID == resultID {
			changeID = c.Latest.ChangeID
		}
	}
	if changeID != "" {
		view, err = s.checkStatusOf(ctx, repo, effort, changeID, tree)
	}
	if err != nil {
		slog.Default().Warn("pipeline: status", "repo", repo.Name, "effort", effort.Slug, "err", err)
		return
	}
	org, _ := s.orgChecksOf(ctx, repo)
	placed := placeChecks(view, org)
	outputs := map[string]rules.Output{}
	for _, pc := range placed {
		if pc.Result != nil {
			outputs[pc.Def.Name] = s.resultOutputs(repo, pc.Result)
		}
	}
	for _, pc := range placed {
		if pc.State != rules.StateReady || pc.Def.Runner == "" || !contains(pc.Def.After, check) {
			continue // not ready, a plain command the landing agent runs, or not after this check
		}
		values, missing := rules.ResolveInputs(rules.Step{Name: pc.Def.Name, After: pc.Def.After, Inputs: pc.Def.Inputs}, outputs)
		if len(missing) > 0 {
			slog.Default().Info("pipeline: inputs not reported by the earlier checks", "repo", repo.Name, "check", pc.Def.Name, "missing", missing)
		}
		if err := s.enqueueDependent(ctx, repo, effort, pc.Def, changeID, tree, values); err != nil {
			slog.Default().Warn("pipeline: enqueue", "repo", repo.Name, "check", pc.Def.Name, "err", err)
		}
	}
	s.publishCheckRuns(ctx, repo, effort)
}

// resultOutputs are a result's data fields (the scalar values of its JSON
// object, as text) and its artifacts (each a link to it on vx), for the
// inputs of later checks.
func (s *Service) resultOutputs(repo store.Repo, r *CheckResult) rules.Output {
	out := rules.Output{Data: map[string]string{}, Artifacts: map[string]string{}}
	var obj map[string]any
	if len(r.Data) > 0 && json.Unmarshal(r.Data, &obj) == nil {
		for k, v := range obj {
			switch x := v.(type) {
			case string:
				out.Data[k] = x
			case float64:
				out.Data[k] = strconv.FormatFloat(x, 'f', -1, 64)
			case bool:
				out.Data[k] = strconv.FormatBool(x)
			case nil:
			default:
				if b, err := json.Marshal(x); err == nil {
					out.Data[k] = string(b)
				}
			}
		}
	}
	for _, a := range r.Artifacts {
		out.Artifacts[a.Name] = s.publicURL() + "/ui/checks/artifact?" + url.Values{"repo": {repo.Name}, "result": {r.ID.String()}, "name": {a.Name}}.Encode()
	}
	return out
}

// CheckStatusNotes are what vx check status says about a status view
// besides the checks: that the change edits vx.toml (the checks of the
// effort's base apply until it lands), and warnings about vx.toml checks
// left out because an organization check has their name.
func CheckStatusNotes(editsRules bool, warnings []string) []string {
	var out []string
	if editsRules {
		out = append(out, fmt.Sprintf("this change edits %s: the checks of the effort's base apply until it lands (a check it adds applies too; one it removes or weakens still applies)", rules.File))
	}
	return append(out, warnings...)
}

// PipelineView is the checks of a change (or of an effort's head) on one
// tree, placed in their pipeline, for vx check status and the web.
type PipelineView struct {
	View     CheckStatusView // what CheckStatus says
	Repo     store.Repo
	Effort   store.Effort
	ChangeID string
	Tree     hash.Hash
	Checks   []PipelineCheck // in pipeline order
	// Notes: the change edits vx.toml, vx.toml checks left out for an
	// organization check's name, a vx.toml that does not parse.
	Notes []string
	// EditsRules: the change edits vx.toml.
	EditsRules bool
	Org        OrgChecks
}

// CheckCounts sum up a pipeline.
type CheckCounts struct{ Passed, Failed, Waiting, Skipped int }

// Counts sums up the view: neutral counts as passed, ready as waiting.
func (v PipelineView) Counts() CheckCounts {
	var c CheckCounts
	for _, ch := range v.Checks {
		switch ch.State {
		case rules.StatePassed, rules.StateNeutral:
			c.Passed++
		case rules.StateFailed:
			c.Failed++
		case rules.StateReady, rules.StateWaiting:
			c.Waiting++
		case rules.StateSkipped:
			c.Skipped++
		}
	}
	return c
}

// PipelineStatus is CheckStatus with every check placed in its pipeline
// (read, as CheckStatus: CI tokens of the repository too): in dependency
// order, indented under the check it runs after, with where it comes from
// and why it waits or was skipped. The notes say when the change edits
// vx.toml itself: the checks of the effort's base apply until it lands.
func (s *Service) PipelineStatus(ctx context.Context, actor Actor, in CheckStatusInput) (PipelineView, error) {
	view, err := s.CheckStatus(ctx, actor, in)
	if err != nil {
		return PipelineView{}, err
	}
	repo, err := s.Store.RepoByName(ctx, view.Repo)
	if err != nil {
		return PipelineView{}, classify(err)
	}
	eff, err := s.Store.GetEffort(ctx, repo.ID, view.Effort)
	if err != nil {
		return PipelineView{}, classify(err)
	}
	org, _ := s.orgChecksOf(ctx, repo)
	out := PipelineView{View: view, Repo: repo, Effort: eff, ChangeID: view.ChangeID, Tree: view.Tree, Checks: placeChecks(view, org), Org: org}
	// Only a change that has not landed yet: once it lands, its vx.toml
	// is the effort's.
	if t, err := s.resolveCheckTarget(ctx, repo, eff, view.ChangeID, view.Tree); err == nil && !t.Landed && !t.BaseTree.IsZero() && !t.Tree.IsZero() {
		out.EditsRules = s.rulesFileChanged(ctx, t.BaseTree, t.Tree)
	}
	out.View.Warnings = dedupe(view.Warnings)
	notes := out.View.Warnings
	if view.Problem != "" {
		notes = append([]string{rules.File + " does not parse, so its checks are not known: " + view.Problem}, notes...)
	}
	out.Notes = CheckStatusNotes(out.EditsRules, dedupe(notes))
	return out, nil
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// enqueueDependent makes the job of a check whose earlier checks are done
// on tree, with their outputs as named values.
func (s *Service) enqueueDependent(ctx context.Context, repo store.Repo, effort store.Effort, def rules.CheckDef, changeID string, tree hash.Hash, values map[string]string) error {
	t, err := s.resolveCheckTarget(ctx, repo, effort, changeID, tree)
	if err != nil {
		return err
	}
	trigger := string(def.When)
	if trigger == "" {
		trigger = "land"
	}
	_, _, err = s.enqueueCheckJob(ctx, JobSpec{Repo: repo, Effort: effort, Def: jobDefOf(def), Trigger: trigger,
		Change: JobChangeRef{ID: t.ChangeID, Author: t.Author}, Base: t.BaseTree, Tree: t.Tree, Values: values})
	return err
}

// treeResults are the results of a change's checks on tree, by check name
// (pass, fail or neutral), for rules.Pipeline. Errors leave it empty: the
// checks after others then wait.
func (s *Service) treeResults(ctx context.Context, repo store.Repo, effort store.Effort, changeID string, tree hash.Hash) map[string]string {
	view, err := s.checkStatusOf(ctx, repo, effort, changeID, tree)
	if err != nil {
		slog.Default().Warn("pipeline: results", "repo", repo.Name, "effort", effort.Slug, "err", err)
		return nil
	}
	out := map[string]string{}
	for _, c := range view.Checks {
		switch c.State {
		case CheckStatePassed:
			out[c.Def.Name] = rules.ResultPass
		case CheckStateFailed:
			out[c.Def.Name] = rules.ResultFail
		case CheckStateNeutral:
			out[c.Def.Name] = rules.ResultNeutral
		}
	}
	return out
}
