package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/provider"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// Shipper pushes branches and opens pull requests on the git host.
type Shipper interface {
	ExportBranch(ctx context.Context, repo store.Repo, baseSHA, branch string, commits []provider.Commit) ([]string, error)
	OpenPullRequest(ctx context.Context, repo store.Repo, pr provider.PullRequest) (provider.OpenedPR, error)
}

// ShipStep is one landed change replayed onto the target.
type ShipStep struct {
	Changeset store.ChangesetRow
	Tree      hash.Hash // tree after this step, on top of the target
	Files     int       // files this step changes on the target
	Skipped   bool      // the target already has this change
}

// ShipPlan is what shipping an effort would produce.
type ShipPlan struct {
	Repo         store.Repo
	Effort       store.Effort
	Target       string
	TargetHead   store.ChangesetRow
	Branch       string
	Title        string
	Body         string
	Steps        []ShipStep
	Conflicts    []engine.Conflict
	ConflictedAt *store.ChangesetRow
	// Warnings are problems found in the tree that would ship, checked over
	// every path the effort changes on the target. They do not stop a ship.
	Warnings []engine.Warning
}

// Commits returns the steps that produce a commit.
func (p ShipPlan) Commits() []ShipStep {
	var out []ShipStep
	for _, s := range p.Steps {
		if !s.Skipped {
			out = append(out, s)
		}
	}
	return out
}

// PlanShipment replays the effort's changes, oldest first, onto the latest
// head of the target branch. Each change becomes one commit. A change whose
// parent is not the current tree is merged onto it, so an effort started on
// an older commit, or on another branch, still ships cleanly. Shipping
// needs admin on the repository.
func (s *Service) PlanShipment(ctx context.Context, actor Actor, repoName, slug, target string) (ShipPlan, error) {
	v, err := s.effortFor(ctx, actor, repoName, slug, RoleAdmin)
	if err != nil {
		return ShipPlan{}, err
	}
	if v.Repo.Provider == store.ProviderNone {
		return ShipPlan{}, errf(CodeFailedPrecondition, "repo %s has no git upstream to ship to", v.Repo.Name)
	}
	if target == "" {
		target = v.Effort.BaseBranch
	}
	branch, _, err := s.syncBranch(ctx, actor, v.Repo, target)
	if err != nil {
		return ShipPlan{}, err
	}
	head, err := s.Store.GetChangeset(ctx, v.Repo.ID, branch.Head)
	if err != nil {
		return ShipPlan{}, err
	}
	if head.GitSHA == "" {
		return ShipPlan{}, errf(CodeFailedPrecondition, "target %s has no git commit", target)
	}
	chain, err := s.Store.Chain(ctx, v.Repo.ID, v.Effort.Head, v.Effort.Base, 10000)
	if err != nil {
		return ShipPlan{}, err
	}
	plan := ShipPlan{Repo: v.Repo, Effort: v.Effort, Target: target, TargetHead: head, Branch: "vx/" + v.Effort.Slug}
	rp, err := s.replayChain(ctx, v.Repo.ID, chain, head.Tree, target, replayShip)
	if err != nil {
		return ShipPlan{}, err
	}
	plan.Steps, plan.Conflicts, plan.ConflictedAt = rp.Steps, rp.Conflicts, rp.ConflictedAt
	cur := rp.Tree
	if len(plan.Steps) == 0 && len(plan.Conflicts) == 0 {
		return plan, errf(CodeFailedPrecondition, "nothing to ship: effort %s has no landed changes", slug)
	}
	if len(plan.Conflicts) == 0 && cur != head.Tree {
		changes, err := object.DiffTrees(ctx, s.Blobs, head.Tree, cur)
		if err != nil {
			return ShipPlan{}, err
		}
		plan.Warnings = s.treeWarnings(ctx, cur, changes)
		plan.Warnings = append(plan.Warnings, engine.GeneratedWarnings(uniqueGenerated(rp.Generated))...)
		plan.Warnings = append(plan.Warnings, s.ruleWarnings(ctx, head.Tree, cur)...)
	}
	plan.Title = v.Effort.Name
	plan.Body = prBody(plan)
	return plan, nil
}

// replayMode says what replayChain does at a conflict.
type replayMode int

const (
	// replayShip stops at the first conflicting change and records the
	// merged trees as part of the repository, as shipping needs.
	replayShip replayMode = iota
	// replayForecast keeps going: a conflicting change leaves its file with
	// conflict markers and the replay goes on, so every conflicting path is
	// found. Nothing is recorded as part of the repository.
	replayForecast
)

// replayResult is the outcome of replaying an effort's changes.
type replayResult struct {
	Steps        []ShipStep
	Tree         hash.Hash // the tree after the last replayed change
	Conflicts    []engine.Conflict
	ConflictedAt *store.ChangesetRow    // the first change that conflicted
	Generated    []engine.GeneratedFile // generated files both sides changed: taken from the change, not merged
}

// replayChain replays the vx changes of chain (newest first, as Store.Chain
// returns it), oldest first, onto the tree onto. A change whose parent tree
// is the current tree takes its own tree; any other change is three-way
// merged onto the current tree (base: its parent, ours: the change,
// theirs: the current tree, so keep-both puts the target's lines first),
// generated files excepted (vx.toml [[generated]]). This is the replay of
// shipping, and of the overlap forecast.
func (s *Service) replayChain(ctx context.Context, repoID uuid.UUID, chain []store.ChangesetRow, onto hash.Hash, target string, mode replayMode) (replayResult, error) {
	res := replayResult{Tree: onto}
	cur := onto
	for i := len(chain) - 1; i >= 0; i-- {
		c := chain[i]
		if c.Origin != object.OriginVX {
			continue
		}
		parent, err := s.Store.GetChangeset(ctx, repoID, c.FirstParent())
		if err != nil {
			return res, err
		}
		next := c.Tree
		if parent.Tree != cur {
			labels := merge.Labels{Base: "base", Ours: c.ChangeID, Theirs: target}
			opts := s.mergeOptions(ctx, c.Tree, cur)
			if mode == replayForecast {
				mr, err := engine.MergeTreesKeepConflicts(ctx, s.Blobs, parent.Tree, c.Tree, cur, labels, opts...)
				if err != nil {
					return res, err
				}
				if !mr.Clean() && res.ConflictedAt == nil {
					cc := c
					res.ConflictedAt = &cc
				}
				res.Conflicts = append(res.Conflicts, mr.Conflicts...)
				res.Generated = append(res.Generated, mr.Generated...)
				if mr.Tree.IsZero() { // a path conflict leaves no tree to go on with
					break
				}
				next = mr.Tree
			} else {
				mr, err := engine.MergeTrees(ctx, s.Blobs, parent.Tree, c.Tree, cur, labels, opts...)
				if err != nil {
					return res, err
				}
				if !mr.Clean() {
					res.Conflicts = mr.Conflicts
					cc := c
					res.ConflictedAt = &cc
					break
				}
				if err := s.recordTree(ctx, repoID, cur, mr.Tree); err != nil {
					return res, err
				}
				next = mr.Tree
				res.Generated = append(res.Generated, mr.Generated...)
			}
		}
		changes, err := object.DiffTrees(ctx, s.Blobs, cur, next)
		if err != nil {
			return res, err
		}
		res.Steps = append(res.Steps, ShipStep{Changeset: c, Tree: next, Files: len(changes), Skipped: len(changes) == 0})
		cur = next
	}
	res.Tree = cur
	return res, nil
}

// ShipInput controls OpenShipment.
type ShipInput struct {
	Repo, Effort, Target string
	Title, Body          string // empty: generated
	Draft                bool
}

// ShipResult is a pushed branch and, where the provider has them, its PR.
type ShipResult struct {
	Plan     ShipPlan
	Shipment store.Shipment
	PR       provider.OpenedPR
}

// OpenShipment pushes the planned commits to vx/<effort> and opens or
// updates the pull request. Shipping again replaces the branch and updates
// the same PR. vx never merges: that stays a decision on the git host.
// PlanShipment checks admin on the repository.
func (s *Service) OpenShipment(ctx context.Context, actor Actor, in ShipInput) (ShipResult, error) {
	if _, _, err := s.Authorize(ctx, actor, in.Repo, RoleAdmin); err != nil {
		return ShipResult{}, err
	}
	if s.Shipper == nil {
		return ShipResult{}, errf(CodeFailedPrecondition, "this server cannot ship: no git bridge configured")
	}
	plan, err := s.PlanShipment(ctx, actor, in.Repo, in.Effort, in.Target)
	if err != nil {
		return ShipResult{}, err
	}
	if len(plan.Conflicts) > 0 {
		return ShipResult{Plan: plan}, errf(CodeFailedPrecondition,
			"change %s conflicts with %s; sync the effort onto %s first", plan.ConflictedAt.ChangeID, plan.Target, plan.Target)
	}
	steps := plan.Commits()
	if len(steps) == 0 {
		return ShipResult{Plan: plan}, errf(CodeFailedPrecondition, "nothing to ship: %s already has every change", plan.Target)
	}
	if in.Title != "" {
		plan.Title = in.Title
	}
	if in.Body != "" {
		plan.Body = in.Body
	}
	commits := make([]provider.Commit, 0, len(steps))
	for _, st := range steps {
		c := st.Changeset
		commits = append(commits, provider.Commit{
			Tree: st.Tree, Message: commitMessage(plan.Effort.Slug, c.Changeset),
			AuthorName: c.Author.Name, AuthorEmail: c.Author.Email, When: c.CreatedAt,
		})
	}
	shas, err := s.Shipper.ExportBranch(ctx, plan.Repo, plan.TargetHead.GitSHA, plan.Branch, commits)
	if err != nil {
		return ShipResult{Plan: plan}, errf(CodeUnavailable, "push %s: %v", plan.Branch, err)
	}
	res := ShipResult{Plan: plan}
	state := "pushed"
	pr, err := s.Shipper.OpenPullRequest(ctx, plan.Repo, provider.PullRequest{
		Head: plan.Branch, Base: plan.Target, Title: plan.Title, Body: plan.Body, Draft: in.Draft,
	})
	switch {
	case errors.Is(err, provider.ErrNoPullRequests):
		res.PR = pr // may carry a link where a person opens the PR
	case err != nil:
		return res, errf(CodeUnavailable, "branch %s was pushed, but opening the pull request failed: %v", plan.Branch, err)
	default:
		res.PR = pr
		state = "open"
		if in.Draft {
			state = "draft"
		}
	}
	sh := store.Shipment{
		EffortID: plan.Effort.ID, Title: plan.Title, Branch: plan.Branch, Target: plan.Target,
		BaseGitSHA: plan.TargetHead.GitSHA, HeadGitSHA: shas[len(shas)-1], URL: pr.URL, State: state, CreatedBy: actor.ID,
	}
	if pr.Number > 0 {
		sh.ProviderRef = fmt.Sprint(pr.Number)
	}
	for i, st := range steps {
		sh.Changes = append(sh.Changes, store.ShipmentChange{Position: i, Changeset: st.Changeset.Hash, ChangeID: st.Changeset.ChangeID, GitSHA: shas[i]})
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if res.Shipment, err = tx.SaveShipment(ctx, sh); err != nil {
			return err
		}
		return tx.LogOp(ctx, &plan.Repo.ID, &actor.ID, "ship", plan.Effort.Slug, nil,
			map[string]any{"branch": plan.Branch, "target": plan.Target, "commits": len(shas), "pr": pr.URL})
	})
	return res, err
}

// ListShipments returns an effort's shipments (read).
func (s *Service) ListShipments(ctx context.Context, actor Actor, repoName, slug string) ([]store.Shipment, error) {
	v, err := s.GetEffort(ctx, actor, repoName, slug)
	if err != nil {
		return nil, err
	}
	return s.Store.ListShipments(ctx, v.Effort.ID)
}

// commitMessage is the intent plus trailers that carry vx provenance into git.
func commitMessage(slug string, c *object.Changeset) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(c.Intent))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Vx-Change-Id: %s\n", c.ChangeID)
	fmt.Fprintf(&b, "Vx-Effort: %s\n", slug)
	if c.Author.Principal != "" {
		fmt.Fprintf(&b, "Vx-Agent: %s\n", c.Author.Principal)
	}
	if c.Author.OnBehalfOf != "" {
		fmt.Fprintf(&b, "Vx-On-Behalf-Of: %s\n", c.Author.OnBehalfOf)
	}
	if c.Author.Model != "" {
		fmt.Fprintf(&b, "Vx-Model: %s\n", c.Author.Model)
	}
	if r := c.Rebase; r != nil {
		fmt.Fprintf(&b, "Vx-Rebased-Onto: %s\n", rebasedOnto(c))
		if r.ResolvedBy != "" {
			fmt.Fprintf(&b, "Vx-Resolved-By: %s\n", r.ResolvedBy)
		}
	}
	if c.Evidence != nil {
		for _, ch := range c.Evidence.Checks {
			text := checkText(ch)
			if c.Merged() {
				text += ", before merge"
			}
			if c.Rebased() {
				text += ", before rebase"
			}
			fmt.Fprintf(&b, "Vx-Evidence: %s\n", text)
		}
	}
	return b.String()
}

// rebasedOnto names where a rebased change was replayed: "main at 1a2b3c4d5e6f".
func rebasedOnto(c *object.Changeset) string {
	r := c.Rebase
	branch := r.Branch
	if branch == "" {
		branch = "the target"
	}
	if r.OntoGitSHA != "" {
		return branch + " at " + short(r.OntoGitSHA)
	}
	return branch + " at vx " + r.Onto.Short()
}

func checkText(ch object.Check) string {
	s := ch.Name + " " + ch.Result
	var notes []string
	if ch.Passed > 0 || ch.Failed > 0 {
		notes = append(notes, fmt.Sprintf("%d passed, %d failed", ch.Passed, ch.Failed))
	}
	if ch.RanByVX {
		notes = append(notes, "run by vx")
	} else {
		notes = append(notes, "reported")
	}
	if ch.Notes != "" {
		notes = append(notes, ch.Notes)
	}
	return s + " (" + strings.Join(notes, ", ") + ")"
}

// prBody describes the effort for reviewers: goal, changes, evidence and
// who built it.
func prBody(p ShipPlan) string {
	var b strings.Builder
	if g := strings.TrimSpace(p.Effort.Goal); g != "" {
		fmt.Fprintf(&b, "## Goal\n\n%s\n\n", g)
	}
	b.WriteString("## Changes\n\n")
	agents := map[string]bool{}
	people := map[string]bool{}
	var checks []string
	var rebased *object.Changeset // the latest rebased change, if any
	for _, st := range p.Commits() {
		c := st.Changeset
		who := c.Author.Principal
		if c.Author.OnBehalfOf != "" {
			who += " for " + c.Author.OnBehalfOf
			people[c.Author.OnBehalfOf] = true
		}
		agents[c.Author.Principal] = true
		fmt.Fprintf(&b, "- %s (`%s`, %s)\n", c.Title(), c.ChangeID, who)
		if c.Rebase != nil {
			rebased = c.Changeset
			if c.Rebase.ResolvedBy != "" {
				fmt.Fprintf(&b, "  - conflict resolved during the rebase by %s\n", c.Rebase.ResolvedBy)
			}
		}
		if c.Evidence != nil {
			for _, ch := range c.Evidence.Checks {
				line := fmt.Sprintf("- `%s`: %s", c.ChangeID, checkText(ch))
				if c.Merged() {
					line += " on the author's snapshot, before vx merged it onto newer work"
				}
				if c.Rebased() {
					line += ", before the effort was rebased onto " + rebasedOnto(c.Changeset)
				}
				checks = append(checks, line)
			}
			for _, nv := range c.Evidence.NotVerified {
				checks = append(checks, fmt.Sprintf("- `%s`: not verified: %s", c.ChangeID, rules.NotVerified(nv)))
			}
		}
	}
	if len(checks) > 0 {
		b.WriteString("\n## Evidence\n\n")
		b.WriteString(strings.Join(checks, "\n"))
		b.WriteString("\n")
	}
	if len(p.Warnings) > 0 {
		b.WriteString("\n## Warnings\n\n")
		for _, w := range p.Warnings {
			fmt.Fprintf(&b, "- %s\n", w.Detail)
		}
	}
	fmt.Fprintf(&b, "\n---\nBuilt in vx effort `%s` by %d agent(s)", p.Effort.Slug, len(agents))
	if len(people) > 0 {
		fmt.Fprintf(&b, " for %d person(s)", len(people))
	}
	fmt.Fprintf(&b, ". %d commit(s) on top of `%s` at %s.\n", len(p.Commits()), p.Target, short(p.TargetHead.GitSHA))
	if rebased != nil {
		fmt.Fprintf(&b, "The effort was rebased onto %s", "`"+strings.Replace(rebasedOnto(rebased), " at ", "` at ", 1))
		if rebased.Rebase.By != "" {
			fmt.Fprintf(&b, " by %s", rebased.Rebase.By)
		}
		b.WriteString("; evidence of the rebased changes was gathered before the rebase.\n")
	}
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// uniqueGenerated keeps the last report of each generated path (a file
// several replayed changes touched is listed once).
func uniqueGenerated(gs []engine.GeneratedFile) []engine.GeneratedFile {
	idx := map[string]int{}
	var out []engine.GeneratedFile
	for _, g := range gs {
		if i, ok := idx[g.Path]; ok {
			out[i] = g
			continue
		}
		idx[g.Path] = len(out)
		out = append(out, g)
	}
	return out
}
