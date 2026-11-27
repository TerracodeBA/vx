package service

// The check work queue (#47, part 2). A check whose runner is not the
// landing agent ("agent:KIND", "ci" or "human") becomes a job when it
// applies: on land of a change touching its paths (when = "land"), on ship
// of an effort touching them (when = "ship"), or when someone asks for it
// (vx check request NAME, any check with a runner). A job carries the
// check, the effort, the change and tree, and its inputs: the diff, the
// changed files, the effort, the change, the files a runner may fetch
// through the object API, and named values (preview_url, or an earlier
// check's output wired by pipelines).
//
// Runners take the oldest job they may run with a lease (vx check next),
// renew it while they work and report a result (vx check report), which
// finishes the job. An expired lease puts the job back with the attempt
// counted; after MaxJobAttempts expired leases it fails with the reason. A
// new tree for the same check of an effort supersedes the pending jobs of
// older trees. Taking a job locks candidate rows with FOR UPDATE SKIP
// LOCKED, so any number of servers share one queue and two runners never
// get the same job.
//
// Who may take what:
//   - agent:KIND jobs: agents whose person declared KIND for them (vx agent
//     runner NAME --kinds KIND), with write on the repository.
//   - ci jobs: CI tokens of the repository, for the checks they may report.
//   - human jobs: people with write on the repository; they also show on
//     the For you page.
//
// A runner is never given a job whose result it could not report: jobs of
// "independent" checks skip the change's author and their person's other
// agents (reporter trust, checktrust.go).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// Queue limits and defaults.
const (
	// MaxJobAttempts is how many leases of a job may expire before it fails.
	MaxJobAttempts = 3
	// DefaultJobTimeout is the lease of a check without a timeout.
	DefaultJobTimeout = 30 * time.Minute
	// MaxJobLease bounds a lease and a renewal.
	MaxJobLease = 24 * time.Hour
	// MaxJobDiff bounds the diff input of a job; the rest is cut.
	MaxJobDiff = 256 << 10
	// maxJobDiffFile is the largest file pair diffed for the diff input.
	maxJobDiffFile = 1 << 20
	// MaxJobValues bounds the named values of one job.
	MaxJobValues = 50
	// MaxJobValue bounds one named value.
	MaxJobValue = 4096
	// jobCandidates is how many queued jobs one take looks at.
	jobCandidates = 25
)

// Runner kinds of jobs.
const (
	RunnerCI    = "ci"
	RunnerHuman = "human"
	// RunnerAgentPrefix starts the runner of an agent job: agent:browser.
	RunnerAgentPrefix = "agent:"
)

// Standard job inputs, always in the bundle; any other declared input is a
// named value.
var standardInputs = []string{"diff", "changed_files", "effort", "change", "files"}

// jobDef is what the queue needs of a check definition.
type jobDef struct {
	Name            string
	When            string
	Paths           []string
	Runner          string
	Reporter        string
	AllowSamePerson bool
	Inputs          []string
	After           []string
	Timeout         time.Duration
}

// applies reports whether the check applies to a change of paths: no
// paths means every change.
func (d jobDef) applies(paths []string) bool {
	if len(d.Paths) == 0 {
		return true
	}
	for _, p := range paths {
		for _, g := range d.Paths {
			if rules.MatchGlob(g, p) {
				return true
			}
		}
	}
	return false
}

// queued reports whether the check is one the queue runs: its runner is not
// the landing agent.
func (d jobDef) queued() bool { return d.Runner != "" }

// CheckJobView is a job with what a runner needs: the resolved inputs.
type CheckJobView struct {
	store.CheckJob
	// LeaseOwnerRef is who holds the lease; empty when nobody does.
	LeaseOwnerRef string
	// Inputs is set by NextCheckJob and GetCheckJob.
	Inputs *JobInputs
}

// JobInputs is a job's input bundle.
type JobInputs struct {
	Effort       JobEffort         `json:"effort"`
	Change       JobChange         `json:"change"`
	ChangedFiles []JobFile         `json:"changed_files"`
	Files        []JobFile         `json:"files"` // fetchable through the object API (GetObjects with the repository)
	Diff         string            `json:"diff,omitempty"`
	DiffCut      bool              `json:"diff_cut,omitempty"`
	Values       map[string]string `json:"values"`
	// Missing lists declared inputs that have no value yet.
	Missing []string `json:"missing,omitempty"`
}

// JobEffort is the effort input.
type JobEffort struct {
	Repo   string `json:"repo"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Goal   string `json:"goal,omitempty"`
	Branch string `json:"base_branch"`
}

// JobChange is the change input. For a ship job it is the effort's head.
type JobChange struct {
	ID         string `json:"id,omitempty"`
	Changeset  string `json:"changeset,omitempty"`
	Intent     string `json:"intent,omitempty"`
	Author     string `json:"author,omitempty"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	BaseTree   string `json:"base_tree"`
	Tree       string `json:"tree"`
}

// JobFile is one changed file.
type JobFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // add, modify, delete
	Hash   string `json:"hash,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

// EnqueueInput is what land, ship and requests hand the queue.
type EnqueueInput struct {
	Repo    store.Repo
	Effort  store.Effort
	Trigger string // land, ship or manual
	// Change is the change the jobs are for: the landed one on land, the
	// effort head on ship, the workspace's change at a land gate.
	Change JobChangeRef
	// Base and Tree are what the diff goes from and to; results must be for
	// Tree.
	Base, Tree hash.Hash
	// Paths are the changed paths the checks' globs are matched against.
	Paths []string
	// Values are named input values (preview_url), by input name.
	Values      map[string]string
	RequestedBy *uuid.UUID
	// Only, when set, is the one check to enqueue whatever its when.
	Only string
}

// JobSpec is one job to enqueue (pipelines enqueue dependents with it).
type JobSpec struct {
	Repo        store.Repo
	Effort      store.Effort
	Def         jobDef
	Trigger     string
	Change      JobChangeRef
	Base, Tree  hash.Hash
	Values      map[string]string
	RequestedBy *uuid.UUID
}

// JobChangeRef is the change a job is for.
type JobChangeRef struct {
	ID        string
	Changeset hash.Hash // zero when it has not landed (a land gate)
	Intent    string
	Author    ChangeAuthor
}

// changeRefOf is the reference to a stored changeset.
func changeRefOf(cs store.ChangesetRow) JobChangeRef {
	if cs.Changeset == nil {
		return JobChangeRef{Changeset: cs.Hash}
	}
	return JobChangeRef{ID: cs.ChangeID, Changeset: cs.Hash, Intent: cs.Intent, Author: authorOf(cs.Author.Principal, cs.Author.OnBehalfOf)}
}

// enqueueCheckJob stores one job, superseding pending jobs of the same check
// on older trees of the effort. created is false when a pending job for the
// same tree existed; the values were merged into it.
func (s *Service) enqueueCheckJob(ctx context.Context, spec JobSpec) (store.CheckJob, bool, error) {
	timeout := spec.Def.Timeout
	if timeout <= 0 {
		timeout = DefaultJobTimeout
	}
	timeout = min(timeout, MaxJobLease)
	j := store.CheckJob{
		RepoID: spec.Repo.ID, EffortID: spec.Effort.ID, Check: spec.Def.Name, Runner: spec.Def.Runner,
		Reporter: spec.Def.Reporter, AllowSamePerson: spec.Def.AllowSamePerson, Trigger: spec.Trigger,
		BaseTree: spec.Base, Tree: spec.Tree, Inputs: spec.Def.Inputs, Values: spec.Values,
		MaxAttempts: MaxJobAttempts, Timeout: timeout, RequestedBy: spec.RequestedBy,
	}
	c := spec.Change
	j.ChangeID, j.Changeset, j.Intent = c.ID, c.Changeset, c.Intent
	j.AuthorPrincipal, j.AuthorPerson = c.Author.Principal, c.Author.Person
	var out store.CheckJob
	var created bool
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if out, created, err = tx.EnqueueCheckJob(ctx, j, s.Clock()); err != nil {
			return err
		}
		if !created {
			return nil
		}
		return tx.LogOp(ctx, &spec.Repo.ID, spec.RequestedBy, "check.job.create", spec.Repo.Name+"/"+spec.Effort.Slug,
			nil, map[string]any{"job": out.ID.String(), "check": out.Check, "runner": out.Runner, "trigger": out.Trigger,
				"change": out.ChangeID, "tree": out.Tree.String()})
	})
	return out, created, classify(err)
}

// enqueueCheckJobs makes the jobs of the queued checks that apply to a
// land, a ship or a request. Checks with after (pipelines) are enqueued
// when what they come after has passed (pipelineReady), not here.
func (s *Service) enqueueCheckJobs(ctx context.Context, in EnqueueInput, defs []jobDef) ([]store.CheckJob, error) {
	var out []store.CheckJob
	for _, d := range defs {
		if !d.queued() {
			continue
		}
		if in.Only != "" {
			if d.Name != in.Only {
				continue
			}
		} else if d.When != in.Trigger || !d.applies(in.Paths) || !s.pipelineReady(ctx, in, d, defs) {
			continue
		}
		j, _, err := s.enqueueCheckJob(ctx, JobSpec{Repo: in.Repo, Effort: in.Effort, Def: d, Trigger: in.Trigger,
			Change: in.Change, Base: in.Base, Tree: in.Tree, Values: in.Values, RequestedBy: in.RequestedBy})
		if err != nil {
			return out, err
		}
		out = append(out, j)
	}
	return out, nil
}

// pipelineReady reports whether d may run now on this tree: every check
// it runs after passed (or was neutral) on in.Tree, or does not apply to
// the change (see rules.Pipeline). After a failure it never is: the
// pipeline shows it as skipped instead.
func (s *Service) pipelineReady(ctx context.Context, in EnqueueInput, d jobDef, defs []jobDef) bool {
	if len(d.After) == 0 {
		return true
	}
	steps := make([]rules.Step, 0, len(defs))
	applies := map[string]bool{}
	for _, x := range defs {
		steps = append(steps, rules.Step{Name: x.Name, After: x.After, Inputs: x.Inputs})
		applies[x.Name] = x.applies(in.Paths)
	}
	results := s.treeResults(ctx, in.Repo, in.Effort, in.Change.ID, in.Tree)
	return slices.Contains(rules.PipelineReady(steps, results, func(n string) bool { return applies[n] }), d.Name)
}

// jobDefsAt returns the checks of a repository at the given trees as the
// queue sees them, through the same definitions the gates use (checkDefs:
// vx.toml's [[check]] tables and organization checks); the first tree's
// definition of a name wins. [checks] required and [[policy]] make no
// jobs. A vx.toml that does not parse makes none either; land reports it.
func (s *Service) jobDefsAt(ctx context.Context, repo store.Repo, trees ...hash.Hash) ([]jobDef, error) {
	defs, _, _ := s.checkDefs(ctx, repo, trees...)
	var out []jobDef
	seen := map[string]bool{}
	for _, d := range defs {
		if (d.Origin != rules.OriginCheck && d.Origin != rules.OriginOrg) || seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		out = append(out, jobDefOf(d))
	}
	return out, nil
}

// changedPaths lists the paths that differ between two trees.
func (s *Service) changedPaths(ctx context.Context, a, b hash.Hash) ([]string, error) {
	changes, err := object.DiffTrees(ctx, s.Blobs, a, b)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out, nil
}

// jobsAfterLand makes the jobs of "land" checks a landed change applies
// to. It never fails or slows the land beyond reading vx.toml: errors are
// logged.
func (s *Service) jobsAfterLand(ctx context.Context, repo store.Repo, effortID uuid.UUID, res LandResult) {
	if !res.Landed || res.Changeset.Changeset == nil {
		return
	}
	if err := s.enqueueLandJobs(ctx, repo, effortID, res.Changeset); err != nil {
		slog.Warn("check jobs after land", "repo", repo.Name, "change", res.Changeset.ChangeID, "err", err)
	}
}

func (s *Service) enqueueLandJobs(ctx context.Context, repo store.Repo, effortID uuid.UUID, cs store.ChangesetRow) error {
	base, err := s.parentTree(ctx, repo.ID, cs)
	if err != nil {
		return err
	}
	// The checks of the effort before this change and of the change itself
	// apply, as for the land gate: a change can add checks for itself,
	// never drop one the effort has.
	defs, err := s.jobDefsAt(ctx, repo, base, cs.Tree)
	if err != nil || !slices.ContainsFunc(defs, func(d jobDef) bool { return d.queued() && d.When == "land" }) {
		return err
	}
	eff, err := s.Store.EffortByID(ctx, effortID)
	if err != nil {
		return err
	}
	paths, err := s.changedPaths(ctx, base, cs.Tree)
	if err != nil {
		return err
	}
	_, err = s.enqueueCheckJobs(ctx, EnqueueInput{Repo: repo, Effort: eff, Trigger: "land", Change: changeRefOf(cs),
		Base: base, Tree: cs.Tree, Paths: paths}, defs)
	return err
}

// enqueueShipJobs makes the jobs of "ship" checks for an effort's head:
// the checks whose paths the effort changed from its base. Jobs that exist
// for the head tree are kept.
func (s *Service) enqueueShipJobs(ctx context.Context, repo store.Repo, eff store.Effort) ([]store.CheckJob, error) {
	head, err := s.Store.GetChangeset(ctx, repo.ID, eff.Head)
	if err != nil {
		return nil, classify(err)
	}
	defs, err := s.jobDefsAt(ctx, repo, head.Tree)
	if err != nil || !slices.ContainsFunc(defs, func(d jobDef) bool { return d.queued() && d.When == "ship" }) {
		return nil, err
	}
	base, err := s.Store.GetChangeset(ctx, repo.ID, eff.Base)
	if err != nil {
		return nil, classify(err)
	}
	paths, err := s.changedPaths(ctx, base.Tree, head.Tree)
	if err != nil {
		return nil, err
	}
	return s.enqueueCheckJobs(ctx, EnqueueInput{Repo: repo, Effort: eff, Trigger: "ship", Change: changeRefOf(head),
		Base: base.Tree, Tree: head.Tree, Paths: paths}, defs)
}

// enqueueGateJobs makes the jobs a land gate waits for: for each check
// named in missing that has a runner, a job on the snapshot tree the
// author wants to land (the change has no changeset yet). The land gate
// calls it when it refuses a land, so the runners can produce the results
// the author's next land needs; asking again for the same tree returns the
// same jobs.
func (s *Service) enqueueGateJobs(ctx context.Context, repo store.Repo, eff store.Effort, change JobChangeRef, base, tree hash.Hash, defs []jobDef, missing []string) ([]store.CheckJob, error) {
	var out []store.CheckJob
	for _, d := range defs {
		if !d.queued() || !slices.Contains(missing, d.Name) {
			continue
		}
		j, _, err := s.enqueueCheckJob(ctx, JobSpec{Repo: repo, Effort: eff, Def: d, Trigger: "land", Change: change, Base: base, Tree: tree})
		if err != nil {
			return out, err
		}
		out = append(out, j)
	}
	return out, nil
}

// jobsBeforeShip makes the ship jobs of an effort before a ship plan;
// errors are logged, never fail the plan (the gates decide).
func (s *Service) jobsBeforeShip(ctx context.Context, repo store.Repo, eff store.Effort) {
	if _, err := s.enqueueShipJobs(ctx, repo, eff); err != nil {
		slog.Warn("check jobs before ship", "repo", repo.Name, "effort", eff.Slug, "err", err)
	}
}

func (s *Service) parentTree(ctx context.Context, repoID uuid.UUID, cs store.ChangesetRow) (hash.Hash, error) {
	if len(cs.Parents) == 0 {
		return emptyTree, nil
	}
	p, err := s.Store.GetChangeset(ctx, repoID, cs.Parents[0])
	if err != nil {
		return hash.Hash{}, err
	}
	return p.Tree, nil
}

// RequestCheckInput asks for a check to run.
type RequestCheckInput struct {
	Repo, Effort string
	Check        string
	// ChangeID picks a change; empty: the effort's head (its whole diff).
	ChangeID string
	Values   map[string]string
}

// RequestCheck makes a job for a check that has a runner, on the effort's
// head or one change, whatever the check's when. Values are named inputs
// (preview_url). Asking again for the same tree merges the values into the
// pending job. Needs write.
func (s *Service) RequestCheck(ctx context.Context, actor Actor, in RequestCheckInput) (CheckJobView, error) {
	v, err := s.effortFor(ctx, actor, in.Repo, in.Effort, RoleWrite)
	if err != nil {
		return CheckJobView{}, err
	}
	if v.Effort.State != store.EffortOpen {
		return CheckJobView{}, errf(CodeFailedPrecondition, "effort %s is %s: checks run on open efforts", v.Effort.Slug, v.Effort.State)
	}
	values, err := cleanJobValues(in.Values)
	if err != nil {
		return CheckJobView{}, err
	}
	var cs store.ChangesetRow
	var base hash.Hash
	if strings.TrimSpace(in.ChangeID) == "" {
		if cs, err = s.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Head); err != nil {
			return CheckJobView{}, classify(err)
		}
		b, err := s.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Base)
		if err != nil {
			return CheckJobView{}, classify(err)
		}
		base = b.Tree
	} else {
		if cs, err = s.Store.ResolveChangeset(ctx, v.Repo.ID, strings.TrimSpace(in.ChangeID)); err != nil {
			return CheckJobView{}, classify(err)
		}
		if cs.EffortID == nil || *cs.EffortID != v.Effort.ID {
			return CheckJobView{}, errf(CodeNotFound, "change %s is not in effort %s; vx log lists its changes", in.ChangeID, v.Effort.Slug)
		}
		if base, err = s.parentTree(ctx, v.Repo.ID, cs); err != nil {
			return CheckJobView{}, err
		}
	}
	// The checks are those of the effort's head: a change cannot bring its
	// own definition of a check along.
	head, err := s.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Head)
	if err != nil {
		return CheckJobView{}, classify(err)
	}
	defs, err := s.jobDefsAt(ctx, v.Repo, head.Tree)
	if err != nil {
		return CheckJobView{}, err
	}
	var def *jobDef
	var names []string
	for i := range defs {
		if defs[i].queued() {
			names = append(names, defs[i].Name)
		}
		if defs[i].Name == in.Check {
			def = &defs[i]
		}
	}
	switch {
	case def == nil && len(names) == 0:
		return CheckJobView{}, errf(CodeFailedPrecondition, "%s has no check %q, and no check with a runner; add a [[check]] with runner = \"agent:KIND\", \"ci\" or \"human\" to vx.toml", v.Repo.Name, in.Check)
	case def == nil:
		return CheckJobView{}, errf(CodeFailedPrecondition, "%s has no check %q; checks with a runner: %s", v.Repo.Name, in.Check, strings.Join(names, ", "))
	case !def.queued():
		return CheckJobView{}, errf(CodeInvalid, "check %s has no runner: the landing agent runs it with vx land; there is no job to request", def.Name)
	}
	j, _, err := s.enqueueCheckJob(ctx, JobSpec{Repo: v.Repo, Effort: v.Effort, Def: *def, Trigger: "manual",
		Change: changeRefOf(cs), Base: base, Tree: cs.Tree, Values: values, RequestedBy: &actor.ID})
	if err != nil {
		return CheckJobView{}, err
	}
	return s.jobView(ctx, j), nil
}

// cleanJobValues checks named values: names like inputs, bounded sizes.
func cleanJobValues(values map[string]string) (map[string]string, error) {
	if len(values) > MaxJobValues {
		return nil, errf(CodeInvalid, "%d values; keep it to %d", len(values), MaxJobValues)
	}
	out := map[string]string{}
	for k, v := range values {
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, " \t\r\n=") || len(k) > 100 {
			return nil, errf(CodeInvalid, "value name %q: give NAME=VALUE with a name like preview_url", k)
		}
		if len(v) > MaxJobValue {
			return nil, errf(CodeInvalid, "value %s is %d bytes; keep it to %d (put large inputs in files)", k, len(v), MaxJobValue)
		}
		out[k] = v
	}
	return out, nil
}

// NextJobInput says which job a runner wants.
type NextJobInput struct {
	// Repo and Effort narrow the queue; empty Repo: every repository the
	// caller may run jobs in.
	Repo, Effort string
	// Runner is a runner kind the caller serves: browser (agent:browser),
	// ci or human. Empty: every kind it serves.
	Runner string
}

// runnerKindsOf returns the job runners actor may run, and the CI token it
// is, if any.
func (s *Service) runnerKindsOf(ctx context.Context, actor Actor) ([]string, *CITokenView, error) {
	ci, err := s.ciTokenOf(ctx, actor)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case ci != nil:
		return []string{RunnerCI}, ci, nil
	case actor.Kind == store.KindHuman:
		return []string{RunnerHuman}, nil, nil
	case actor.Kind == store.KindAgent:
		kinds, err := s.Store.AgentRunnerKinds(ctx, actor.ID)
		if err != nil {
			return nil, nil, err
		}
		out := make([]string, 0, len(kinds))
		for _, k := range kinds {
			out = append(out, RunnerAgentPrefix+k)
		}
		return out, nil, nil
	}
	return nil, nil, nil
}

// normRunner turns "browser" into "agent:browser"; ci and human stay.
func normRunner(r string) string {
	r = strings.TrimSpace(r)
	if r == "" || r == RunnerCI || r == RunnerHuman || strings.HasPrefix(r, RunnerAgentPrefix) {
		return r
	}
	return RunnerAgentPrefix + r
}

// NextCheckJob gives the caller the oldest queued job it may run, with a
// lease for the check's timeout, and its inputs; nil when there is none.
// Expired leases are put back (or failed) first.
func (s *Service) NextCheckJob(ctx context.Context, actor Actor, in NextJobInput) (*CheckJobView, error) {
	kinds, ci, err := s.runnerKindsOf(ctx, actor)
	if err != nil {
		return nil, err
	}
	// The repository first: one the caller cannot read is "not found",
	// whatever it may run.
	repoIDs, effortID, err := s.jobScope(ctx, actor, ci, in.Repo, in.Effort, RoleWrite)
	if err != nil {
		return nil, err
	}
	if len(kinds) == 0 {
		if actor.Kind == store.KindAgent {
			return nil, errf(CodeFailedPrecondition, "%s serves no runner kinds, so no job is for it; %s can declare some: vx agent runner %s --kinds browser", actor.Ref(), ownerRef(actor), agentName(actor.Handle))
		}
		return nil, errf(CodePermission, "%s runs no checks: jobs go to agents with runner kinds, CI tokens and people", actor.Ref())
	}
	if r := normRunner(in.Runner); r != "" {
		if !slices.Contains(kinds, r) {
			return nil, errf(CodeInvalid, "%s does not serve %s jobs; it serves %s", actor.Ref(), r, strings.Join(kinds, ", "))
		}
		kinds = []string{r}
	}
	if len(repoIDs) == 0 {
		return nil, nil
	}
	me, err := s.reporterIdentity(ctx, actor)
	if err != nil {
		return nil, err
	}
	now := s.Clock()
	if _, _, err := s.Store.ReapCheckJobs(ctx, now); err != nil {
		return nil, err
	}
	var got *store.CheckJob
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		cands, err := tx.QueuedCheckJobs(ctx, repoIDs, effortID, kinds, jobCandidates)
		if err != nil {
			return err
		}
		for _, j := range cands {
			if ci != nil && !ci.MayReport(j.Check) {
				continue
			}
			if ok, _ := ReporterAllowed(j.Reporter, j.AllowSamePerson, ChangeAuthor{Principal: j.AuthorPrincipal, Person: j.AuthorPerson}, me); !ok {
				continue
			}
			leased, err := tx.LeaseCheckJob(ctx, j.ID, actor.ID, now, now.Add(j.Timeout))
			if err != nil {
				return err
			}
			got = &leased
			return tx.LogOp(ctx, &j.RepoID, &actor.ID, "check.job.lease", j.RepoName+"/"+j.EffortSlug, nil,
				map[string]any{"job": j.ID.String(), "check": j.Check, "until": leased.LeaseExpiresAt})
		}
		return nil
	})
	if err != nil || got == nil {
		return nil, classify(err)
	}
	v := s.jobView(ctx, *got)
	if v.Inputs, err = s.jobInputs(ctx, *got); err != nil {
		return nil, err
	}
	return &v, nil
}

// jobScope returns the repositories (and effort) a caller's job query is
// about: the named repository with need (a CI token: its own), or every
// repository the caller can read with need on it and queued jobs.
func (s *Service) jobScope(ctx context.Context, actor Actor, ci *CITokenView, repo, effort string, need RepoRole) ([]uuid.UUID, *uuid.UUID, error) {
	repo, effort = strings.TrimSpace(repo), strings.TrimSpace(effort)
	if ci != nil {
		if repo == "" {
			repo = ci.RepoName
		}
		r, err := s.ciRepo(ctx, ci, repo)
		if err != nil {
			return nil, nil, err
		}
		return s.withEffort(ctx, r, effort)
	}
	if repo != "" {
		r, _, err := s.Authorize(ctx, actor, repo, need)
		if err != nil {
			return nil, nil, err
		}
		return s.withEffort(ctx, r, effort)
	}
	if effort != "" {
		return nil, nil, errf(CodeInvalid, "--effort needs --repo")
	}
	ids, _, err := s.ReadableRepoIDs(ctx, actor)
	if err != nil {
		return nil, nil, err
	}
	var out []uuid.UUID
	for _, id := range ids {
		if _, err := s.AuthorizeID(ctx, actor, id, need); err == nil {
			out = append(out, id)
		}
	}
	return out, nil, nil
}

func (s *Service) withEffort(ctx context.Context, r store.Repo, effort string) ([]uuid.UUID, *uuid.UUID, error) {
	if effort == "" {
		return []uuid.UUID{r.ID}, nil, nil
	}
	e, err := s.Store.GetEffort(ctx, r.ID, effort)
	if err != nil {
		return nil, nil, classify(err)
	}
	return []uuid.UUID{r.ID}, &e.ID, nil
}

// jobFor loads a job by id (or an 8+ character prefix) in a repository
// actor may read (a CI token: its own).
func (s *Service) jobFor(ctx context.Context, actor Actor, repo, id string) (store.CheckJob, error) {
	ci, err := s.ciTokenOf(ctx, actor)
	if err != nil {
		return store.CheckJob{}, err
	}
	repoIDs, _, err := s.jobScope(ctx, actor, ci, repo, "", RoleRead)
	if err != nil {
		return store.CheckJob{}, err
	}
	notFound := errf(CodeNotFound, "no check job %s; vx check jobs lists them", id)
	id = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
	if len(id) < 8 || strings.Trim(id, "0123456789abcdef") != "" {
		return store.CheckJob{}, errf(CodeInvalid, "job id %q: give the id vx check next printed, or 8+ characters of it", id)
	}
	rows, err := s.Store.CheckJobsByPrefix(ctx, repoIDs, id)
	if err != nil {
		return store.CheckJob{}, err
	}
	switch len(rows) {
	case 0:
		return store.CheckJob{}, notFound
	case 1:
		return rows[0], nil
	}
	return store.CheckJob{}, errf(CodeInvalid, "job id %s is ambiguous: give more of it", id)
}

// GetCheckJob returns one job with its inputs. Read on its repository, or
// a CI token of it.
func (s *Service) GetCheckJob(ctx context.Context, actor Actor, repo, id string) (CheckJobView, error) {
	j, err := s.jobFor(ctx, actor, repo, id)
	if err != nil {
		return CheckJobView{}, err
	}
	v := s.jobView(ctx, j)
	if v.Inputs, err = s.jobInputs(ctx, j); err != nil {
		return CheckJobView{}, err
	}
	return v, nil
}

// RenewCheckJob extends the caller's lease on a job by d (0: the check's
// timeout), counted from now.
func (s *Service) RenewCheckJob(ctx context.Context, actor Actor, repo, id string, d time.Duration) (CheckJobView, error) {
	j, err := s.jobFor(ctx, actor, repo, id)
	if err != nil {
		return CheckJobView{}, err
	}
	if d <= 0 {
		d = j.Timeout
	}
	if d > MaxJobLease {
		return CheckJobView{}, errf(CodeInvalid, "--for %s: a lease lasts at most %s; renew again later", d, MaxJobLease)
	}
	now := s.Clock()
	if _, _, err := s.Store.ReapCheckJobs(ctx, now); err != nil {
		return CheckJobView{}, err
	}
	out, err := s.Store.RenewCheckJob(ctx, j.ID, actor.ID, now, now.Add(d))
	if errors.Is(err, store.ErrLeaseLost) {
		return CheckJobView{}, s.leaseLost(ctx, actor, j.ID)
	}
	if err != nil {
		return CheckJobView{}, err
	}
	return s.jobView(ctx, out), nil
}

// ReleaseCheckJob gives the caller's leased job back to the queue without
// counting an attempt, with a reason others see.
func (s *Service) ReleaseCheckJob(ctx context.Context, actor Actor, repo, id, reason string) (CheckJobView, error) {
	j, err := s.jobFor(ctx, actor, repo, id)
	if err != nil {
		return CheckJobView{}, err
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		return CheckJobView{}, errf(CodeInvalid, "reason is %d characters; keep it to 500", len(reason))
	}
	if reason == "" {
		reason = "released by " + actor.Ref()
	} else {
		reason = "released by " + actor.Ref() + ": " + reason
	}
	out, err := s.Store.ReleaseCheckJob(ctx, j.ID, actor.ID, s.Clock(), reason)
	if errors.Is(err, store.ErrLeaseLost) {
		return CheckJobView{}, s.leaseLost(ctx, actor, j.ID)
	}
	if err != nil {
		return CheckJobView{}, err
	}
	if err := s.Store.LogOp(ctx, &j.RepoID, &actor.ID, "check.job.release", j.RepoName+"/"+j.EffortSlug, nil,
		map[string]any{"job": j.ID.String(), "check": j.Check, "reason": reason}); err != nil {
		return CheckJobView{}, err
	}
	return s.jobView(ctx, out), nil
}

// leaseLost says why the caller no longer holds a job's lease.
func (s *Service) leaseLost(ctx context.Context, actor Actor, id uuid.UUID) error {
	j, err := s.Store.CheckJobByID(ctx, id)
	if err != nil {
		return classify(err)
	}
	short := j.ID.String()[:8]
	switch {
	case j.State == store.JobSuperseded:
		return errf(CodeFailedPrecondition, "job %s was superseded (%s): stop and take the next one with vx check next", short, j.Reason)
	case j.State == store.JobDone:
		return errf(CodeFailedPrecondition, "job %s is done: a result was reported", short)
	case j.State == store.JobFailed:
		return errf(CodeFailedPrecondition, "job %s failed: %s", short, j.Reason)
	case j.State == store.JobLeased && (j.LeaseOwner == nil || *j.LeaseOwner != actor.ID):
		return errf(CodeFailedPrecondition, "job %s is leased to %s now: your lease expired; take the next one with vx check next", short, s.principalRef(ctx, *j.LeaseOwner))
	}
	return errf(CodeFailedPrecondition, "you hold no lease on job %s (it is %s; %s): take it with vx check next", short, j.State, j.Reason)
}

// ListJobsInput selects jobs for ListCheckJobs.
type ListJobsInput struct {
	Repo, Effort string
	// States: queued, leased, done, failed, superseded; empty: all.
	States []string
	Runner string
	Limit  int
}

// ListCheckJobs lists a repository's (or effort's) jobs, pending first.
// Read on the repository, or a CI token of it; without a repository, every
// repository the caller can read.
func (s *Service) ListCheckJobs(ctx context.Context, actor Actor, in ListJobsInput) ([]CheckJobView, error) {
	ci, err := s.ciTokenOf(ctx, actor)
	if err != nil {
		return nil, err
	}
	for _, st := range in.States {
		if !slices.Contains([]string{store.JobQueued, store.JobLeased, store.JobDone, store.JobFailed, store.JobSuperseded}, st) {
			return nil, errf(CodeInvalid, "state %q: use queued, leased, done, failed or superseded", st)
		}
	}
	repoIDs, effortID, err := s.jobScope(ctx, actor, ci, in.Repo, in.Effort, RoleRead)
	if err != nil || len(repoIDs) == 0 {
		return nil, err
	}
	if _, _, err := s.Store.ReapCheckJobs(ctx, s.Clock()); err != nil {
		return nil, err
	}
	var runners []string
	if r := normRunner(in.Runner); r != "" {
		runners = []string{r}
	}
	rows, err := s.Store.ListCheckJobs(ctx, store.CheckJobFilter{RepoIDs: repoIDs, EffortID: effortID, States: in.States, Runners: runners, Limit: in.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]CheckJobView, 0, len(rows))
	for _, j := range rows {
		out = append(out, s.jobView(ctx, j))
	}
	return out, nil
}

// HumanJobs returns the pending human jobs of the repositories actor can
// write, for the For you page; nil for agents and services.
func (s *Service) HumanJobs(ctx context.Context, actor Actor) ([]CheckJobView, error) {
	if actor.Kind != store.KindHuman {
		return nil, nil
	}
	repoIDs, _, err := s.jobScope(ctx, actor, nil, "", "", RoleWrite)
	if err != nil || len(repoIDs) == 0 {
		return nil, err
	}
	rows, err := s.Store.ListCheckJobs(ctx, store.CheckJobFilter{RepoIDs: repoIDs, States: []string{store.JobQueued, store.JobLeased},
		Runners: []string{RunnerHuman}, Limit: forYouMax})
	if err != nil {
		return nil, err
	}
	me, err := s.reporterIdentity(ctx, actor)
	if err != nil {
		return nil, err
	}
	var out []CheckJobView
	for _, j := range rows {
		if ok, _ := ReporterAllowed(j.Reporter, j.AllowSamePerson, ChangeAuthor{Principal: j.AuthorPrincipal, Person: j.AuthorPerson}, me); ok {
			out = append(out, s.jobView(ctx, j))
		}
	}
	return out, nil
}

// afterCheckResult runs when a result that counts is stored for a check on
// a tree of an effort: it finishes the check's pending jobs for that tree
// (and pipelines start what comes after it).
func (s *Service) afterCheckResult(ctx context.Context, repo store.Repo, effortID uuid.UUID, check string, tree hash.Hash, resultID uuid.UUID) {
	jobs, err := s.Store.FinishCheckJobs(ctx, effortID, check, tree, resultID.String(), s.Clock())
	if err != nil {
		slog.Warn("finishing check jobs", "repo", repo.Name, "check", check, "err", err)
		return
	}
	for _, j := range jobs {
		if err := s.Store.LogOp(ctx, &repo.ID, nil, "check.job.done", repo.Name+"/"+j.EffortSlug, nil,
			map[string]any{"job": j.ID.String(), "check": j.Check, "result": resultID.String()}); err != nil {
			slog.Warn("logging check job", "repo", repo.Name, "err", err)
		}
	}
	s.pipelineAdvance(ctx, repo, effortID, check, tree, resultID)
}

func (s *Service) jobView(ctx context.Context, j store.CheckJob) CheckJobView {
	v := CheckJobView{CheckJob: j}
	if j.LeaseOwner != nil && j.State == store.JobLeased {
		v.LeaseOwnerRef = s.principalRef(ctx, *j.LeaseOwner)
	}
	return v
}

// jobInputs resolves a job's input bundle.
func (s *Service) jobInputs(ctx context.Context, j store.CheckJob) (*JobInputs, error) {
	eff, err := s.Store.EffortByID(ctx, j.EffortID)
	if err != nil {
		return nil, classify(err)
	}
	in := &JobInputs{
		Effort: JobEffort{Repo: j.RepoName, Slug: eff.Slug, Name: eff.Name, Goal: eff.Goal, Branch: eff.BaseBranch},
		Change: JobChange{ID: j.ChangeID, Intent: j.Intent, Author: j.AuthorPrincipal, OnBehalfOf: j.AuthorPerson,
			BaseTree: j.BaseTree.String(), Tree: j.Tree.String()},
		Values: j.Values,
	}
	if !j.Changeset.IsZero() {
		in.Change.Changeset = j.Changeset.String()
	}
	base := j.BaseTree
	if base.IsZero() {
		base = emptyTree
	}
	changes, err := object.DiffTrees(ctx, s.Blobs, base, j.Tree)
	if err != nil {
		return nil, err
	}
	sort.Slice(changes, func(a, b int) bool { return changes[a].Path < changes[b].Path })
	in.ChangedFiles, in.Files = []JobFile{}, []JobFile{}
	for _, c := range changes {
		f := JobFile{Path: c.Path, Status: string(c.Op)}
		if c.Op != object.OpDelete {
			f.Hash, f.Mode = c.New.Hash.String(), c.New.Mode.String()
			in.Files = append(in.Files, f)
		}
		in.ChangedFiles = append(in.ChangedFiles, f)
	}
	if len(j.Inputs) == 0 || slices.Contains(j.Inputs, "diff") {
		if in.Diff, in.DiffCut, err = s.jobDiff(ctx, changes); err != nil {
			return nil, err
		}
	}
	for _, name := range j.Inputs {
		if slices.Contains(standardInputs, name) {
			continue
		}
		if _, ok := j.Values[name]; !ok {
			in.Missing = append(in.Missing, name)
		}
	}
	return in, nil
}

// jobDiff is the unified diff of changes, cut at MaxJobDiff.
func (s *Service) jobDiff(ctx context.Context, changes []object.Change) (string, bool, error) {
	var b strings.Builder
	for _, c := range changes {
		if b.Len() >= MaxJobDiff {
			b.WriteString("... diff cut at 256 KiB: fetch the files to see the rest\n")
			return b.String(), true, nil
		}
		var old, cur []byte
		var err error
		if !c.Old.Hash.IsZero() && c.Op != object.OpAdd {
			if old, err = s.readFile(ctx, c.Old.Hash); err != nil {
				return "", false, err
			}
		}
		if !c.New.Hash.IsZero() && c.Op != object.OpDelete {
			if cur, err = s.readFile(ctx, c.New.Hash); err != nil {
				return "", false, err
			}
		}
		oldName, newName := "a/"+c.Path, "b/"+c.Path
		if c.Op == object.OpAdd {
			oldName = "/dev/null"
		}
		if c.Op == object.OpDelete {
			newName = "/dev/null"
		}
		switch {
		case bytes.IndexByte(old, 0) >= 0 || bytes.IndexByte(cur, 0) >= 0:
			fmt.Fprintf(&b, "Binary files %s and %s differ\n", oldName, newName)
		case len(old)+len(cur) > maxJobDiffFile:
			fmt.Fprintf(&b, "--- %s\n+++ %s\n(file too large to diff here: fetch it)\n", oldName, newName)
		default:
			b.WriteString(merge.Unified(oldName, newName, old, cur, 3))
		}
	}
	out := b.String()
	if len(out) > MaxJobDiff {
		cut := strings.LastIndexByte(out[:MaxJobDiff], '\n')
		return out[:cut+1] + "... diff cut at 256 KiB: fetch the files to see the rest\n", true, nil
	}
	return out, false, nil
}

func (s *Service) readFile(ctx context.Context, h hash.Hash) ([]byte, error) {
	_, data, err := s.Blobs.Get(ctx, h)
	return data, err
}

// HumanCheck is a check waiting for a person, for the For you page.
type HumanCheck struct {
	JobID      uuid.UUID
	Repo       string
	Effort     string // slug
	EffortName string
	Check      string
	ChangeID   string
	Tree       hash.Hash
	Since      time.Time
	Inputs     map[string]string // named values, e.g. preview_url
}

// HumanChecksFor lists the human jobs waiting for actor: queued ones (and
// those actor holds) in repositories actor can write whose result actor
// may report (never their own change of an independent or human check).
// Agents and services get none.
func (s *Service) HumanChecksFor(ctx context.Context, actor Actor) ([]HumanCheck, error) {
	jobs, err := s.HumanJobs(ctx, actor)
	if err != nil {
		return nil, err
	}
	out := []HumanCheck{}
	for _, j := range jobs {
		if j.State == store.JobLeased && (j.LeaseOwner == nil || *j.LeaseOwner != actor.ID) {
			continue
		}
		hc := HumanCheck{JobID: j.ID, Repo: j.RepoName, Effort: j.EffortSlug, Check: j.Check, ChangeID: j.ChangeID,
			Tree: j.Tree, Since: j.CreatedAt, Inputs: j.Values}
		if eff, err := s.Store.EffortByID(ctx, j.EffortID); err == nil {
			hc.EffortName = eff.Name
		}
		out = append(out, hc)
	}
	return out, nil
}
