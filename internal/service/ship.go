package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/provider"
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
// an older commit, or on another branch, still ships cleanly.
func (s *Service) PlanShipment(ctx context.Context, actor Actor, repoName, slug, target string) (ShipPlan, error) {
	v, err := s.GetEffort(ctx, repoName, slug)
	if err != nil {
		return ShipPlan{}, err
	}
	if v.Repo.Provider == store.ProviderNone {
		return ShipPlan{}, errf(CodeFailedPrecondition, "repo %s has no git upstream to ship to", v.Repo.Name)
	}
	if target == "" {
		target = v.Effort.BaseBranch
	}
	branch, _, err := s.SyncBranch(ctx, actor, repoName, target)
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
	cur := head.Tree
	for i := len(chain) - 1; i >= 0; i-- {
		c := chain[i]
		if c.Origin != object.OriginVX {
			continue
		}
		parent, err := s.Store.GetChangeset(ctx, v.Repo.ID, c.FirstParent())
		if err != nil {
			return ShipPlan{}, err
		}
		next := c.Tree
		if parent.Tree != cur {
			mr, err := engine.MergeTrees(ctx, s.Blobs, parent.Tree, c.Tree, cur, merge.Labels{Base: "base", Ours: c.ChangeID, Theirs: target})
			if err != nil {
				return ShipPlan{}, err
			}
			if !mr.Clean() {
				plan.Conflicts = mr.Conflicts
				cc := c
				plan.ConflictedAt = &cc
				break
			}
			next = mr.Tree
		}
		changes, err := object.DiffTrees(ctx, s.Blobs, cur, next)
		if err != nil {
			return ShipPlan{}, err
		}
		plan.Steps = append(plan.Steps, ShipStep{Changeset: c, Tree: next, Files: len(changes), Skipped: len(changes) == 0})
		cur = next
	}
	if len(plan.Steps) == 0 && len(plan.Conflicts) == 0 {
		return plan, errf(CodeFailedPrecondition, "nothing to ship: effort %s has no landed changes", slug)
	}
	if len(plan.Conflicts) == 0 && cur != head.Tree {
		changes, err := object.DiffTrees(ctx, s.Blobs, head.Tree, cur)
		if err != nil {
			return ShipPlan{}, err
		}
		plan.Warnings = s.treeWarnings(ctx, cur, changes)
	}
	plan.Title = v.Effort.Name
	plan.Body = prBody(plan)
	return plan, nil
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
func (s *Service) OpenShipment(ctx context.Context, actor Actor, in ShipInput) (ShipResult, error) {
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

// ListShipments returns an effort's shipments.
func (s *Service) ListShipments(ctx context.Context, repoName, slug string) ([]store.Shipment, error) {
	v, err := s.GetEffort(ctx, repoName, slug)
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
	if c.Evidence != nil {
		for _, ch := range c.Evidence.Checks {
			text := checkText(ch)
			if c.Merged() {
				text += ", before merge"
			}
			fmt.Fprintf(&b, "Vx-Evidence: %s\n", text)
		}
	}
	return b.String()
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
	for _, st := range p.Commits() {
		c := st.Changeset
		who := c.Author.Principal
		if c.Author.OnBehalfOf != "" {
			who += " for " + c.Author.OnBehalfOf
			people[c.Author.OnBehalfOf] = true
		}
		agents[c.Author.Principal] = true
		fmt.Fprintf(&b, "- %s (`%s`, %s)\n", c.Title(), c.ChangeID, who)
		if c.Evidence != nil {
			for _, ch := range c.Evidence.Checks {
				line := fmt.Sprintf("- `%s`: %s", c.ChangeID, checkText(ch))
				if c.Merged() {
					line += " on the author's snapshot, before vx merged it onto newer work"
				}
				checks = append(checks, line)
			}
		}
	}
	if len(checks) > 0 {
		b.WriteString("\n## Evidence\n\n")
		b.WriteString(strings.Join(checks, "\n"))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n---\nBuilt in vx effort `%s` by %d agent(s)", p.Effort.Slug, len(agents))
	if len(people) > 0 {
		fmt.Fprintf(&b, " for %d person(s)", len(people))
	}
	fmt.Fprintf(&b, ". %d commit(s) on top of `%s` at %s.\n", len(p.Commits()), p.Target, short(p.TargetHead.GitSHA))
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
