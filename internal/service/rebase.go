package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// RebaseView is a rebase with what a caller needs to show or resume it.
type RebaseView struct {
	Repo   store.Repo
	Effort store.Effort
	Rebase store.Rebase
	// UpToDate is set when there was nothing to rebase: the effort already
	// sits on the head of the branch. No rebase is recorded then.
	UpToDate bool
	// StoppedAt is the old-chain change whose replay conflicts, when the
	// rebase is stopped. Writing the merge of BaseTree (its parent's tree),
	// ChangeTree (its tree) and OntoTree (the rebased chain so far) with
	// conflict markers reproduces the conflicts.
	StoppedAt                      *store.ChangesetRow
	BaseTree, ChangeTree, OntoTree hash.Hash
	StartedBy, UpdatedBy           string // principal refs
}

// errRebaseMoved means someone continued or aborted the rebase first.
var errRebaseMoved = errors.New("rebase changed meanwhile")

// RebaseEffort replays the effort's changes, oldest first, onto the latest
// head of onto (default: the effort's base branch), after importing that
// branch's new commits. Each change is three-way merged onto the rebased
// chain so far with the same rules as a land, and keeps its change id,
// intent, author and evidence; it is marked as rebased. The effort head
// moves only when every change replayed, with a compare-and-swap; changes
// landed meanwhile are replayed too. When a change conflicts the rebase
// stops and waits for ContinueRebase or AbortRebase.
func (s *Service) RebaseEffort(ctx context.Context, actor Actor, repoName, slug, onto string) (RebaseView, error) {
	v, err := s.effortFor(ctx, actor, repoName, slug, RoleWrite)
	if err != nil {
		return RebaseView{}, err
	}
	if onto == "" {
		onto = v.Effort.BaseBranch
	}
	if v.Effort.State != store.EffortOpen {
		return RebaseView{}, errf(CodeFailedPrecondition, "effort %s is %s; only open efforts can be rebased", slug, v.Effort.State)
	}
	// The internal sync: vx repo sync also starts automatic rebases, which
	// must not race this one.
	branch, _, err := s.syncBranch(ctx, actor, v.Repo, onto)
	if err != nil {
		return RebaseView{}, err
	}
	return s.rebaseOnto(ctx, actor, v.Repo, v.Effort, branch)
}

// rebaseOnto is RebaseEffort onto an imported branch, for a caller that
// checked write on the repository (or vx itself, rebasing automatically).
func (s *Service) rebaseOnto(ctx context.Context, actor Actor, repo store.Repo, eff store.Effort, branch store.Branch) (RebaseView, error) {
	onto := branch.Name
	if active, err := s.Store.ActiveRebase(ctx, eff.ID); err == nil {
		return s.rebaseView(ctx, repo, eff, active), s.inProgress(ctx, eff, active)
	} else if !errors.Is(err, store.ErrNotFound) {
		return RebaseView{}, err
	}
	target, err := s.Store.GetChangeset(ctx, repo.ID, branch.Head)
	if err != nil {
		return RebaseView{}, err
	}
	if eff.Base == branch.Head && onto == eff.BaseBranch {
		return RebaseView{Repo: repo, Effort: eff, UpToDate: true}, nil
	}
	pending, err := s.replayList(ctx, repo, eff.Head, eff.Base)
	if err != nil {
		return RebaseView{}, err
	}
	r, err := s.Store.StartRebase(ctx, store.Rebase{
		RepoID: repo.ID, EffortID: eff.ID, OntoBranch: onto, Onto: target.Hash, OntoGitSHA: target.GitSHA,
		OldBase: eff.Base, OldHead: eff.Head, SeenHead: eff.Head, NewHead: target.Hash,
		Pending: pending, StartedBy: actor.ID,
	})
	if errors.Is(err, store.ErrExists) {
		// Someone started one between our check and the insert.
		if active, err2 := s.Store.ActiveRebase(ctx, eff.ID); err2 == nil {
			return s.rebaseView(ctx, repo, eff, active), s.inProgress(ctx, eff, active)
		}
	}
	if err != nil {
		return RebaseView{}, classify(err)
	}
	_ = s.Store.LogOp(ctx, &repo.ID, &actor.ID, "effort.rebase", eff.Slug,
		map[string]string{"base": eff.Base.String(), "head": eff.Head.String()},
		map[string]any{"rebase": r.ID.String(), "onto": onto, "onto_changeset": target.Hash.String(), "onto_git_sha": target.GitSHA, "changes": len(pending)})
	return s.advanceRebase(ctx, actor, repo, r, nil)
}

// ContinueInput resolves the change a stopped rebase conflicts on.
type ContinueInput struct {
	Repo, Effort string
	// StoppedAt is the conflicting change the caller resolved (its old-chain
	// hash), so a resolution of an earlier stop is never applied to a later
	// one.
	StoppedAt hash.Hash
	// Tree is the resolved tree of that change on top of the rebased chain
	// so far; its objects must be uploaded.
	Tree hash.Hash
}

// ContinueRebase records the resolved tree as the stopped change's rebased
// version, marked as resolved by the actor, and replays the rest. It may
// stop again on a later change.
func (s *Service) ContinueRebase(ctx context.Context, actor Actor, in ContinueInput) (RebaseView, error) {
	v, err := s.effortFor(ctx, actor, in.Repo, in.Effort, RoleWrite)
	if err != nil {
		return RebaseView{}, err
	}
	r, err := s.Store.ActiveRebase(ctx, v.Effort.ID)
	if errors.Is(err, store.ErrNotFound) {
		return RebaseView{}, errf(CodeFailedPrecondition, "effort %s has no rebase in progress; start one with: vx effort rebase %s", in.Effort, in.Effort)
	}
	if err != nil {
		return RebaseView{}, err
	}
	if r.State != store.RebaseStopped || len(r.Pending) == 0 {
		return RebaseView{}, errf(CodeFailedPrecondition, "the rebase of %s is %s, not stopped on a conflict; wait for it, or run vx effort rebase --abort", in.Effort, r.State)
	}
	if in.StoppedAt != r.Pending[0] {
		return s.rebaseView(ctx, v.Repo, v.Effort, r), errf(CodeFailedPrecondition,
			"the rebase of %s is now stopped at %s, not at %s; run vx effort rebase --continue again to get its conflicts", in.Effort, r.Pending[0].Short(), in.StoppedAt.Short())
	}
	cur, err := s.Store.GetChangeset(ctx, v.Repo.ID, r.NewHead)
	if err != nil {
		return RebaseView{}, err
	}
	if err := s.checkInRepo(ctx, v.Repo, cur.Tree, in.Tree); err != nil {
		return RebaseView{}, err
	}
	stopped, err := s.Store.GetChangeset(ctx, v.Repo.ID, r.Pending[0])
	if err != nil {
		return RebaseView{}, err
	}
	// What an agent wrote itself to resolve the conflict must be in its
	// scope; taking either side unchanged is not its own change.
	if err := s.checkRebaseResolution(ctx, actor, stopped.Tree, cur.Tree, in.Tree); err != nil {
		return RebaseView{}, err
	}
	seen := r.Version
	r.State, r.UpdatedBy = store.RebaseRunning, actor.ID
	ok, err := s.Store.UpdateRebase(ctx, r, seen)
	if err != nil {
		return RebaseView{}, err
	}
	if !ok {
		return RebaseView{}, errf(CodeFailedPrecondition, "someone else continued or aborted the rebase of %s meanwhile; run vx status to see where it is", in.Effort)
	}
	r.Version++
	_ = s.Store.LogOp(ctx, &v.Repo.ID, &actor.ID, "effort.rebase.continue", v.Effort.Slug, nil,
		map[string]string{"rebase": r.ID.String(), "resolved": r.Pending[0].String(), "tree": in.Tree.String()})
	return s.advanceRebase(ctx, actor, v.Repo, r, &in.Tree)
}

// AbortRebase drops the effort's rebase in progress. The effort is left as
// it was; the changesets the rebase stored stay unreferenced.
func (s *Service) AbortRebase(ctx context.Context, actor Actor, repoName, slug string) (RebaseView, error) {
	v, err := s.effortFor(ctx, actor, repoName, slug, RoleWrite)
	if err != nil {
		return RebaseView{}, err
	}
	r, err := s.Store.ActiveRebase(ctx, v.Effort.ID)
	if errors.Is(err, store.ErrNotFound) {
		return RebaseView{}, errf(CodeFailedPrecondition, "effort %s has no rebase in progress", slug)
	}
	if err != nil {
		return RebaseView{}, err
	}
	seen := r.Version
	r.State, r.UpdatedBy = store.RebaseAborted, actor.ID
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		ok, err := tx.UpdateRebase(ctx, r, seen)
		if err != nil {
			return err
		}
		if !ok {
			return errf(CodeFailedPrecondition, "the rebase of %s finished or changed meanwhile; run vx status to see where it is", slug)
		}
		return tx.LogOp(ctx, &v.Repo.ID, &actor.ID, "effort.rebase.abort", v.Effort.Slug, nil, map[string]string{"rebase": r.ID.String()})
	})
	if err != nil {
		return RebaseView{}, classify(err)
	}
	r.Version++
	return s.rebaseView(ctx, v.Repo, v.Effort, r), nil
}

// GetRebase returns the effort's rebase in progress, or else its latest
// one. Found is false when the effort was never rebased.
func (s *Service) GetRebase(ctx context.Context, actor Actor, repoName, slug string) (RebaseView, bool, error) {
	v, err := s.effortFor(ctx, actor, repoName, slug, RoleRead)
	if err != nil {
		return RebaseView{}, false, err
	}
	r, err := s.Store.ActiveRebase(ctx, v.Effort.ID)
	if errors.Is(err, store.ErrNotFound) {
		r, err = s.Store.LatestRebase(ctx, v.Effort.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return RebaseView{Repo: v.Repo, Effort: v.Effort}, false, nil
	}
	if err != nil {
		return RebaseView{}, false, err
	}
	return s.rebaseView(ctx, v.Repo, v.Effort, r), true, nil
}

// inProgress is the error for starting a second rebase.
func (s *Service) inProgress(ctx context.Context, eff store.Effort, r store.Rebase) error {
	who := s.principalRef(ctx, r.StartedBy)
	if r.State == store.RebaseStopped && len(r.Pending) > 0 {
		return errf(CodeFailedPrecondition, "a rebase of %s onto %s by %s is already in progress, stopped on a conflict at %s: resolve it with vx effort rebase --continue, or drop it with vx effort rebase --abort",
			eff.Slug, r.OntoBranch, who, r.Pending[0].Short())
	}
	return errf(CodeFailedPrecondition, "a rebase of %s onto %s by %s is already running; wait for it, or drop it with vx effort rebase --abort", eff.Slug, r.OntoBranch, who)
}

// replayList returns the vx changes from stop (exclusive) to head, oldest
// first: what a rebase replays.
func (s *Service) replayList(ctx context.Context, repo store.Repo, head, stop hash.Hash) ([]hash.Hash, error) {
	if head == stop {
		return nil, nil
	}
	ok, err := s.Store.OnChain(ctx, repo.ID, head, stop, 100000)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("effort head %s does not descend from %s", head.Short(), stop.Short())
	}
	chain, err := s.Store.Chain(ctx, repo.ID, head, stop, 100000)
	if err != nil {
		return nil, err
	}
	out := make([]hash.Hash, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Origin == object.OriginVX {
			out = append(out, chain[i].Hash)
		}
	}
	return out, nil
}

// advanceRebase replays r.Pending onto r.NewHead. resolved, when set, is
// the resolved tree of the first pending change. It then replays whatever
// landed on the old chain meanwhile, and moves the effort head once the
// whole chain replayed and nobody landed in between.
func (s *Service) advanceRebase(ctx context.Context, actor Actor, repo store.Repo, r store.Rebase, resolved *hash.Hash) (RebaseView, error) {
	attempts := s.MaxLandAttempts
	if attempts <= 0 {
		attempts = 25
	}
	var stored []hash.Hash // replayed changesets stored by this call
	for attempt := 0; ; attempt++ {
		for len(r.Pending) > 0 {
			h, conflicts, err := s.replayOne(ctx, actor, repo, r, resolved)
			if err != nil {
				return RebaseView{}, err
			}
			resolved = nil
			if len(conflicts) > 0 {
				return s.stopRebase(ctx, actor, repo, r, conflicts)
			}
			r.Replayed = append(r.Replayed, store.Replayed{From: r.Pending[0], To: h})
			r.NewHead = h
			r.Pending = r.Pending[1:]
			stored = append(stored, h)
		}
		eff, err := s.Store.EffortByID(ctx, r.EffortID)
		if err != nil {
			return RebaseView{}, classify(err)
		}
		if eff.State != store.EffortOpen {
			return RebaseView{}, errf(CodeFailedPrecondition, "effort %s became %s during the rebase; run vx effort rebase --abort", eff.Slug, eff.State)
		}
		if eff.Head != r.SeenHead {
			// Lands on the old chain since the rebase started: replay them too.
			more, err := s.replayList(ctx, repo, eff.Head, r.SeenHead)
			if err != nil {
				return RebaseView{}, err
			}
			r.Pending, r.SeenHead = more, eff.Head
			continue
		}
		err = s.finishRebase(ctx, actor, repo, eff, r)
		if errors.Is(err, errLostRace) && attempt < attempts {
			continue // someone landed between the check and the swap
		}
		if errors.Is(err, errLostRace) {
			return RebaseView{}, errf(CodeUnavailable, "effort %s is too busy to finish the rebase: gave up after %d attempts, run vx effort rebase --continue to try again", eff.Slug, attempts)
		}
		if errors.Is(err, errRebaseMoved) {
			return RebaseView{}, errf(CodeFailedPrecondition, "the rebase of %s was aborted or continued by someone else meanwhile; nothing changed. Run vx status to see where it is", eff.Slug)
		}
		if err != nil {
			return RebaseView{}, classify(err)
		}
		r.State, r.Version = store.RebaseDone, r.Version+1
		eff, err = s.Store.EffortByID(ctx, r.EffortID)
		if err != nil {
			return RebaseView{}, classify(err)
		}
		return s.rebaseView(ctx, repo, eff, r), nil
	}
}

// replayOne replays the first pending change onto r.NewHead and stores the
// result without an effort (finishRebase adopts it). With conflicts nothing
// is stored.
func (s *Service) replayOne(ctx context.Context, actor Actor, repo store.Repo, r store.Rebase, resolved *hash.Hash) (hash.Hash, []store.RebaseConflict, error) {
	c, err := s.Store.GetChangeset(ctx, repo.ID, r.Pending[0])
	if err != nil {
		return hash.Zero, nil, err
	}
	cur, err := s.Store.GetChangeset(ctx, repo.ID, r.NewHead)
	if err != nil {
		return hash.Zero, nil, err
	}
	next := c.Tree
	resolvedBy := ""
	var generated []engine.GeneratedFile
	var sides *engine.GoSides
	var byDecl []string
	switch {
	case resolved != nil:
		next, resolvedBy = *resolved, actor.Ref()
	default:
		parent, err := s.Store.GetChangeset(ctx, repo.ID, c.FirstParent())
		if err != nil {
			return hash.Zero, nil, err
		}
		if parent.Tree != cur.Tree {
			mr, err := engine.MergeTrees(ctx, s.Blobs, parent.Tree, c.Tree, cur.Tree, rebaseLabels(c, r), s.mergeOptions(ctx, c.Tree, cur.Tree)...)
			if err != nil {
				return hash.Zero, nil, err
			}
			if !mr.Clean() {
				conflicts := s.rebaseConflicts(ctx, mr.Conflicts)
				// Generated files both sides changed are not conflicts, but
				// whoever resolves this change should regenerate them too.
				for _, w := range engine.GeneratedWarnings(mr.Generated) {
					conflicts = append(conflicts, store.RebaseConflict{Path: w.Path, Kind: string(engine.WarningGenerated), Detail: w.Detail, Generated: true})
				}
				return hash.Zero, conflicts, nil
			}
			// Merged trees are new objects: make them part of the repo, as
			// land and ship do, or nobody could fetch the rebased files.
			if err := s.recordTree(ctx, repo.ID, cur.Tree, mr.Tree); err != nil {
				return hash.Zero, nil, err
			}
			next = mr.Tree
			generated = mr.Generated
			byDecl = mr.MergedByDecl
			sides = &engine.GoSides{Base: parent.Tree, Ours: c.Tree, Theirs: cur.Tree, Merged: next,
				OursName: "change " + c.ChangeID, TheirsName: r.OntoBranch + " (rebased)"}
		}
	}
	changes, err := object.DiffTrees(ctx, s.Blobs, cur.Tree, next)
	if err != nil {
		return hash.Zero, nil, err
	}
	files, err := s.lineStats(ctx, changes)
	if err != nil {
		return hash.Zero, nil, err
	}
	nc := *c.Changeset
	nc.Parents = []hash.Hash{cur.Hash}
	nc.Tree = next
	nc.Rebase = &object.Rebase{
		Onto: r.Onto, OntoGitSHA: r.OntoGitSHA, Branch: r.OntoBranch, From: c.Hash,
		By: actor.Ref(), ResolvedBy: resolvedBy, At: s.Now(),
	}
	var warnings []engine.Warning
	if len(changes) > 0 {
		warnings = s.treeWarnings(ctx, next, changes)
		if sides != nil {
			warnings = append(warnings, s.sideWarnings(ctx, *sides)...)
		}
		warnings = append(warnings, engine.MergedByDeclWarnings(byDecl)...)
		warnings = append(warnings, engine.GeneratedWarnings(generated)...)
		warnings = append(warnings, s.ruleWarnings(ctx, cur.Tree, next)...)
	}
	var h hash.Hash
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if h, err = tx.InsertChangeset(ctx, repo.ID, nil, &nc, files); err != nil {
			return err
		}
		if len(warnings) > 0 {
			return tx.SetChangesetWarnings(ctx, repo.ID, h, storeWarnings(warnings))
		}
		return nil
	})
	return h, nil, err
}

// rebaseLabels names the sides in conflict markers: the change being
// replayed is "ours", the rebased chain so far (the branch) "theirs", the
// same roles as when shipping, so keep-both puts the branch's lines first.
func rebaseLabels(c store.ChangesetRow, r store.Rebase) merge.Labels {
	onto := r.OntoBranch
	if r.OntoGitSHA != "" {
		onto += " " + short(r.OntoGitSHA)
	}
	return merge.Labels{Base: "base " + c.FirstParent().Short(), Ours: "change " + c.ChangeID, Theirs: onto + " (rebased)"}
}

// rebaseConflicts converts merge conflicts, flagging generated files.
func (s *Service) rebaseConflicts(ctx context.Context, cs []engine.Conflict) []store.RebaseConflict {
	out := make([]store.RebaseConflict, 0, len(cs))
	for _, c := range cs {
		rc := store.RebaseConflict{Path: c.Path, Kind: string(c.Kind), Detail: c.Detail}
		for _, f := range []object.File{c.Ours, c.Theirs, c.Base} {
			if f.Hash.IsZero() || f.Mode == object.ModeSymlink {
				continue
			}
			if data, err := blob.GetKind(ctx, s.Blobs, hash.KindBlob, f.Hash); err == nil && engine.IsGenerated(data) {
				rc.Generated = true
				break
			}
		}
		out = append(out, rc)
	}
	return out
}

// stopRebase records that the first pending change conflicts.
func (s *Service) stopRebase(ctx context.Context, actor Actor, repo store.Repo, r store.Rebase, conflicts []store.RebaseConflict) (RebaseView, error) {
	seen := r.Version
	r.State, r.Conflicts, r.UpdatedBy = store.RebaseStopped, conflicts, actor.ID
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		ok, err := tx.UpdateRebase(ctx, r, seen)
		if err != nil {
			return err
		}
		if !ok {
			return errRebaseMoved
		}
		paths := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			paths = append(paths, c.Path)
		}
		return tx.LogOp(ctx, &repo.ID, &actor.ID, "effort.rebase.stop", r.EffortID.String(), nil,
			map[string]any{"rebase": r.ID.String(), "at": r.Pending[0].String(), "conflicts": paths})
	})
	if errors.Is(err, errRebaseMoved) {
		return RebaseView{}, errf(CodeFailedPrecondition, "the rebase was aborted or continued by someone else meanwhile; run vx status to see where it is")
	}
	if err != nil {
		return RebaseView{}, classify(err)
	}
	r.Version++
	eff, err := s.Store.EffortByID(ctx, r.EffortID)
	if err != nil {
		return RebaseView{}, classify(err)
	}
	return s.rebaseView(ctx, repo, eff, r), nil
}

// finishRebase moves the effort onto the rebased chain and marks the
// rebase done, both with a compare-and-swap, in one transaction.
func (s *Service) finishRebase(ctx context.Context, actor Actor, repo store.Repo, eff store.Effort, r store.Rebase) (err error) {
	seen := r.Version
	r.State, r.Conflicts, r.UpdatedBy = store.RebaseDone, nil, actor.ID
	defer func() {
		if err == nil {
			s.scheduleForecast(repo, eff.ID)
		}
	}()
	return s.Store.InTx(ctx, func(tx *store.Tx) error {
		ok, err := tx.UpdateRebase(ctx, r, seen)
		if err != nil {
			return err
		}
		if !ok {
			return errRebaseMoved
		}
		ok, err = tx.RebaseEffort(ctx, eff.ID, eff.Version, r.OntoBranch, r.Onto, r.NewHead)
		if err != nil {
			return err
		}
		if !ok {
			return errLostRace
		}
		hs := make([]hash.Hash, 0, len(r.Replayed))
		for _, p := range r.Replayed {
			hs = append(hs, p.To)
		}
		if err := tx.AdoptChangesets(ctx, repo.ID, eff.ID, hs); err != nil {
			return err
		}
		return tx.LogOp(ctx, &repo.ID, &actor.ID, "effort.rebase.done", eff.Slug,
			map[string]string{"base": eff.Base.String(), "head": eff.Head.String()},
			map[string]any{"rebase": r.ID.String(), "base": r.Onto.String(), "head": r.NewHead.String(), "onto": r.OntoBranch, "changes": len(r.Replayed)})
	})
}

func (s *Service) rebaseView(ctx context.Context, repo store.Repo, eff store.Effort, r store.Rebase) RebaseView {
	v := RebaseView{Repo: repo, Effort: eff, Rebase: r, StartedBy: s.principalRef(ctx, r.StartedBy), UpdatedBy: s.principalRef(ctx, r.UpdatedBy)}
	if r.State != store.RebaseStopped || len(r.Pending) == 0 {
		return v
	}
	c, err := s.Store.GetChangeset(ctx, repo.ID, r.Pending[0])
	if err != nil {
		return v
	}
	v.StoppedAt = &c
	v.ChangeTree = c.Tree
	if p, err := s.Store.GetChangeset(ctx, repo.ID, c.FirstParent()); err == nil {
		v.BaseTree = p.Tree
	}
	if cur, err := s.Store.GetChangeset(ctx, repo.ID, r.NewHead); err == nil {
		v.OntoTree = cur.Tree
	}
	return v
}

// principalRef names a principal for messages; empty if unknown.
func (s *Service) principalRef(ctx context.Context, id uuid.UUID) string {
	p, err := s.Store.PrincipalByID(ctx, id)
	if err != nil {
		return ""
	}
	return p.Ref()
}
