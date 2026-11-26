package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// LandInput is a snapshot to land into a workspace's effort.
type LandInput struct {
	WorkspaceID uuid.UUID
	Base        hash.Hash // changeset the snapshot was made on
	Tree        hash.Hash // snapshot root tree; objects must be uploaded
	Intent      string    // empty: the workspace intent
	Evidence    *object.Evidence
	DependsOn   []string
	Model       string
	Session     string
	// RequireUpToDate refuses to merge onto newer work.
	RequireUpToDate bool
}

// LandResult is the outcome of Land.
type LandResult struct {
	Landed     bool
	Changeset  store.ChangesetRow
	Files      []store.FileChange
	Rebased    bool              // the effort moved, the snapshot was merged onto it
	AutoMerged []string          // paths changed on both sides that merged cleanly
	Conflicts  []engine.Conflict // set when Landed is false
	EffortHead hash.Hash
	// EffortMoved is set when RequireUpToDate refused the land.
	EffortMoved bool
	// Warnings are problems found in the landed tree, such as a Go function
	// declared twice in one package after a clean text merge, a vx.toml
	// [[unique]] key used twice (kind "rule"), or a generated file both
	// sides changed, kept from the workspace (kind "generated"). They never
	// stop a land.
	Warnings []engine.Warning
	// OverlappingClaims are active claims of other principals on files this
	// land changed. They never stop a land.
	OverlappingClaims []Claim
	// KeptBoth lists paths where both sides inserted different lines at the
	// same place and the merge kept both; worth a look.
	KeptBoth []string
	// MergedByDecl lists .go paths whose lines conflicted but whose
	// top-level declarations did not, merged declaration by declaration;
	// worth a look, like KeptBoth.
	MergedByDecl []string
	// Overlaps are the effort's stored forecasts with other open efforts.
	// The forecast for this land runs after it, in the background, so they
	// may still describe the effort before this land.
	Overlaps []Overlap
	// CheckResults are the check results this land stored: its vx land
	// --run results, on the snapshot they ran on.
	CheckResults []store.CheckResult
	// Review is set when a "human" landing policy of vx.toml applies: the
	// change waits for a person as this pending review, Landed is false and
	// the effort head did not move.
	Review *Review
}

// ErrNothingToLand is returned when a snapshot has no changes.
var ErrNothingToLand = errors.New("nothing to land")

var errLostRace = errors.New("effort head moved")

// Land merges a workspace snapshot into its effort (write).
//
// When the effort has not moved since the snapshot's base, the snapshot
// becomes the new head. When it has, the snapshot is merged onto the current
// head with a three-way merge, and only real conflicts are sent back. The
// head moves with a compare-and-swap, so concurrent lands never lose work:
// the loser merges again on the new head and retries.
func (s *Service) Land(ctx context.Context, actor Actor, in LandInput) (res LandResult, err error) {
	defer func() { recordLand(res, err) }()
	v, err := s.ownWorkspace(ctx, actor, in.WorkspaceID)
	if err != nil {
		return LandResult{}, err
	}
	if v.Workspace.State != store.WorkspaceOpen {
		return LandResult{}, errf(CodeFailedPrecondition, "workspace is already %s", v.Workspace.State)
	}
	if err := s.requireAgreement(ctx, v.Repo, "landing"); err != nil {
		return LandResult{}, err
	}
	if in.Base.IsZero() {
		in.Base = v.Workspace.Base
	}
	if err := s.checkOnEffort(ctx, v.Repo, v.Effort, in.Base); err != nil {
		return LandResult{}, err
	}
	base, err := s.Store.GetChangeset(ctx, v.Repo.ID, in.Base)
	if err != nil {
		return LandResult{}, classify(err)
	}
	if err := s.checkInRepo(ctx, v.Repo, base.Tree, in.Tree); err != nil {
		return LandResult{}, err
	}
	// An agent's path scope and budgets, after the repository role and
	// before the landing policies.
	if err := s.checkAgentLand(ctx, actor, base.Tree, in.Tree); err != nil {
		return LandResult{}, err
	}
	intent := strings.TrimSpace(in.Intent)
	if intent == "" {
		intent = v.Workspace.Intent
	}
	decision, err := s.landPolicy(ctx, v, base, in)
	if err != nil {
		return LandResult{}, err
	}
	// The checks of vx.toml that block land ([[check]], organization
	// rules) need a passing result on exactly this snapshot.
	gated, err := s.landGate(ctx, actor, v, base, in)
	if err != nil {
		return LandResult{}, err
	}
	if gated {
		in.RequireUpToDate = true
	}
	in.Evidence = s.withRequiredChecks(ctx, in.Tree, in.Evidence)
	switch decision.Mode {
	case rules.ModeHuman:
		return s.submitReview(ctx, actor, v, base, in, intent, decision)
	case rules.ModeGated:
		// The checks ran on the snapshot: land exactly that, never a merge.
		in.RequireUpToDate = true
	}

	attempts := s.MaxLandAttempts
	if attempts <= 0 {
		attempts = 25
	}
	for attempt := 0; attempt < attempts; attempt++ {
		res, err := s.landOnce(ctx, actor, v, base, in, intent, nil)
		if errors.Is(err, errLostRace) {
			continue
		}
		if err == nil && res.Landed {
			// Never slows or fails the land: the stored forecasts are read
			// as they are and the new forecast runs in the background.
			if res.Overlaps, err = s.storedOverlaps(ctx, v.Repo, v.Effort.ID); err != nil {
				slog.Warn("overlaps after land", "repo", v.Repo.Name, "effort", v.Effort.Slug, "err", err)
				res.Overlaps, err = nil, nil
			}
			s.scheduleForecast(v.Repo, v.Effort.ID)
			s.jobsAfterLand(ctx, v.Repo, v.Effort.ID, res)
			s.afterLandChecks(ctx, v.Repo, v.Effort, res)
		}
		return res, err
	}
	return LandResult{}, errf(CodeUnavailable, "effort %s is too busy: gave up after %d attempts, try again", v.Effort.Slug, attempts)
}

// landExtra is what an approved review adds to a land.
type landExtra struct {
	// inTx runs in the land's transaction once the changeset is stored as
	// landed: it marks the review approved.
	inTx func(tx *store.Tx, landed hash.Hash) error
}

func (s *Service) landOnce(ctx context.Context, actor Actor, v WorkspaceView, base store.ChangesetRow, in LandInput, intent string, extra *landExtra) (LandResult, error) {
	eff, err := s.Store.EffortByID(ctx, v.Effort.ID)
	if err != nil {
		return LandResult{}, classify(err)
	}
	if eff.State != store.EffortOpen {
		return LandResult{}, errf(CodeFailedPrecondition, "effort %s is %s", eff.Slug, eff.State)
	}
	head, err := s.Store.GetChangeset(ctx, v.Repo.ID, eff.Head)
	if err != nil {
		return LandResult{}, err
	}
	res := LandResult{EffortHead: eff.Head}

	newTree := in.Tree
	if eff.Head != base.Hash && in.RequireUpToDate {
		res.EffortMoved = true
		return res, nil
	}
	var generated []engine.GeneratedFile
	if eff.Head != base.Hash {
		mr, err := engine.MergeTrees(ctx, s.repoWrites(v.Repo.ID), base.Tree, in.Tree, head.Tree, merge.Labels{
			Base:   "base " + base.Hash.Short(),
			Ours:   "workspace",
			Theirs: "effort " + eff.Head.Short(),
		}, s.mergeOptions(ctx, in.Tree, head.Tree)...)
		if err != nil {
			return LandResult{}, err
		}
		if !mr.Clean() {
			res.Conflicts = mr.Conflicts
			return res, nil
		}
		// The merge made new trees and blobs from the repository's own
		// objects: they are part of it now.
		if err := s.recordTree(ctx, v.Repo.ID, head.Tree, mr.Tree); err != nil {
			return LandResult{}, err
		}
		newTree = mr.Tree
		res.Rebased = true
		res.AutoMerged = mr.AutoMerged
		res.KeptBoth = mr.KeptBoth
		res.MergedByDecl = mr.MergedByDecl
		generated = mr.Generated
	}

	changes, err := object.DiffTrees(ctx, s.Blobs, head.Tree, newTree)
	if err != nil {
		return LandResult{}, err
	}
	if len(changes) == 0 {
		return LandResult{}, &Error{Code: CodeFailedPrecondition, Msg: "nothing to land: the effort already has these changes", Err: ErrNothingToLand}
	}
	files, err := s.lineStats(ctx, changes)
	if err != nil {
		return LandResult{}, err
	}
	res.Warnings = s.treeWarnings(ctx, newTree, changes)
	if res.Rebased {
		res.Warnings = append(res.Warnings, s.sideWarnings(ctx, engine.GoSides{
			Base: base.Tree, Ours: in.Tree, Theirs: head.Tree, Merged: newTree, OursName: "this land", TheirsName: "the effort",
		})...)
	}
	res.Warnings = append(res.Warnings, engine.GeneratedWarnings(generated)...)
	res.Warnings = append(res.Warnings, s.ruleWarnings(ctx, head.Tree, newTree)...)
	res.Warnings = append(res.Warnings, s.orgCheckWarnings(ctx, v.Repo, head.Tree, newTree)...)

	cs := &object.Changeset{
		Version:  object.CurrentVersion,
		ChangeID: v.Workspace.ChangeID,
		Parents:  []hash.Hash{eff.Head},
		Tree:     newTree,
		Origin:   object.OriginVX,
		Author: object.Author{
			Principal:  actor.Ref(),
			Name:       actor.Human().DisplayName,
			Email:      actor.Human().Email,
			OnBehalfOf: actor.OnBehalfOf(),
			Model:      in.Model,
			Session:    in.Session,
		},
		Intent:    intent,
		DependsOn: in.DependsOn,
		Evidence:  in.Evidence,
		CreatedAt: s.Clock(),
	}
	if newTree != in.Tree {
		cs.SnapshotTree = in.Tree
	}

	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		ws, err := tx.GetWorkspaceForUpdate(ctx, v.Workspace.ID)
		if err != nil {
			return err
		}
		if ws.State != store.WorkspaceOpen {
			return errf(CodeFailedPrecondition, "workspace is already %s", ws.State)
		}
		// An approved review (extra) skips the agent's daily budget: it was
		// checked when the review was submitted, and a person decided.
		if extra == nil {
			if err := s.recheckLandsToday(ctx, tx, actor); err != nil {
				return err
			}
		}
		h, err := tx.InsertChangeset(ctx, v.Repo.ID, &eff.ID, cs, files)
		if err != nil {
			return err
		}
		if len(res.Warnings) > 0 {
			if err := tx.SetChangesetWarnings(ctx, v.Repo.ID, h, storeWarnings(res.Warnings)); err != nil {
				return err
			}
		}
		// vx land --run results are evidence of the change on the tree
		// they ran on (the snapshot), for the checks that run them.
		if res.CheckResults, err = s.recordLandChecks(ctx, tx, actor, v, in); err != nil {
			return err
		}
		ok, err := tx.AdvanceEffort(ctx, eff.ID, eff.Version, h)
		if err != nil {
			return err
		}
		if !ok {
			return errLostRace
		}
		if err := tx.FinishWorkspace(ctx, ws.ID, store.WorkspaceLanded, h); err != nil {
			return err
		}
		if extra != nil && extra.inTx != nil {
			if err := extra.inTx(tx, h); err != nil {
				return err
			}
		} else if err := tx.WithdrawReviews(ctx, ws.ID); err != nil {
			return err // landed without review: a pending one is moot
		}
		// A move that landed keeps its claim a while, so changes still made
		// at the old location are warned about.
		if err := tx.KeepMovingClaims(ctx, ws.ID, s.Clock().Add(MovedClaimTTL)); err != nil {
			return err
		}
		if _, err := s.releaseWorkspaceClaims(ctx, tx, actor, v.Repo.ID, ws.ID, nil, s.Clock()); err != nil {
			return err
		}
		if res.OverlappingClaims, err = s.landedOverClaims(ctx, tx, actor, eff.ID, files); err != nil {
			return err
		}
		res.Changeset = store.ChangesetRow{Hash: h, RepoID: v.Repo.ID, EffortID: &eff.ID, Changeset: cs, Warnings: storeWarnings(res.Warnings)}
		return tx.LogOp(ctx, &v.Repo.ID, &actor.ID, "land", eff.Slug,
			map[string]string{"head": eff.Head.String()},
			map[string]any{"head": h.String(), "workspace": ws.ID.String(), "rebased": res.Rebased})
	})
	if err != nil {
		return LandResult{}, classify(err)
	}
	res.Landed = true
	res.Files = files
	res.EffortHead = res.Changeset.Hash
	return res, nil
}

// treeWarnings runs the semantic checks on the packages a change touches.
// They only warn, so a failing check becomes a warning instead of an error.
func (s *Service) treeWarnings(ctx context.Context, tree hash.Hash, changes []object.Change) []engine.Warning {
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	ws, err := engine.CheckGo(ctx, s.Blobs, tree, paths)
	if err != nil {
		return []engine.Warning{{Kind: engine.WarningCheck, Detail: fmt.Sprintf("could not check Go packages for duplicate declarations: %v", err)}}
	}
	return ws
}

// sideWarnings runs the checks that need both sides of a merge: a
// signature one side changed that the other side's new code calls the old
// way, a name one side removed that the other side's new code uses, and
// unreachable code, undefined names and unused imports that only the
// merged packages have. They only warn; a failing check becomes a warning.
func (s *Service) sideWarnings(ctx context.Context, sides engine.GoSides) []engine.Warning {
	ws, err := engine.CheckGoSides(ctx, s.Blobs, sides)
	if err != nil {
		return []engine.Warning{{Kind: engine.WarningCheck, Detail: fmt.Sprintf("could not check Go signatures across the merge: %v", err)}}
	}
	tws, err := engine.CheckGoMerge(ctx, s.Blobs, sides)
	if err != nil {
		return append(ws, engine.Warning{Kind: engine.WarningCheck, Detail: fmt.Sprintf("could not type-check the merged Go packages: %v", err)})
	}
	return append(ws, engine.WithoutCovered(ws, tws)...)
}

// storeWarnings converts engine warnings to the form kept with a changeset.
func storeWarnings(ws []engine.Warning) []store.Warning {
	if len(ws) == 0 {
		return nil
	}
	out := make([]store.Warning, 0, len(ws))
	for _, w := range ws {
		out = append(out, store.Warning{Path: w.Path, Kind: string(w.Kind), Name: w.Name, Decl: w.Decl, Files: w.Files, Detail: w.Detail})
	}
	return out
}

// lineStats counts added and deleted lines for each change.
func (s *Service) lineStats(ctx context.Context, changes []object.Change) ([]store.FileChange, error) {
	out := make([]store.FileChange, 0, len(changes))
	read := func(f object.File) ([]byte, error) {
		if f.Hash.IsZero() {
			return nil, nil
		}
		return blob.GetKind(ctx, s.Blobs, hash.KindBlob, f.Hash)
	}
	for _, c := range changes {
		fc := store.FileChange{Change: c}
		if c.Old.Hash != c.New.Hash {
			a, err := read(c.Old)
			if err != nil {
				return nil, err
			}
			b, err := read(c.New)
			if err != nil {
				return nil, err
			}
			fc.Added, fc.Deleted = merge.LineStats(a, b)
		}
		out = append(out, fc)
	}
	return out, nil
}
