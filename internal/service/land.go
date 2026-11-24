package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
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
}

// ErrNothingToLand is returned when a snapshot has no changes.
var ErrNothingToLand = errors.New("nothing to land")

var errLostRace = errors.New("effort head moved")

// Land merges a workspace snapshot into its effort.
//
// When the effort has not moved since the snapshot's base, the snapshot
// becomes the new head. When it has, the snapshot is merged onto the current
// head with a three-way merge, and only real conflicts are sent back. The
// head moves with a compare-and-swap, so concurrent lands never lose work:
// the loser merges again on the new head and retries.
func (s *Service) Land(ctx context.Context, actor Actor, in LandInput) (LandResult, error) {
	v, err := s.ownWorkspace(ctx, actor, in.WorkspaceID)
	if err != nil {
		return LandResult{}, err
	}
	if v.Workspace.State != store.WorkspaceOpen {
		return LandResult{}, errf(CodeFailedPrecondition, "workspace is already %s", v.Workspace.State)
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
	if err := s.checkUploaded(ctx, base.Tree, in.Tree); err != nil {
		return LandResult{}, err
	}
	intent := strings.TrimSpace(in.Intent)
	if intent == "" {
		intent = v.Workspace.Intent
	}

	attempts := s.MaxLandAttempts
	if attempts <= 0 {
		attempts = 25
	}
	for attempt := 0; attempt < attempts; attempt++ {
		res, err := s.landOnce(ctx, actor, v, base, in, intent)
		if errors.Is(err, errLostRace) {
			continue
		}
		return res, err
	}
	return LandResult{}, errf(CodeUnavailable, "effort %s is too busy: gave up after %d attempts, try again", v.Effort.Slug, attempts)
}

func (s *Service) landOnce(ctx context.Context, actor Actor, v WorkspaceView, base store.ChangesetRow, in LandInput, intent string) (LandResult, error) {
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
	if eff.Head != base.Hash {
		mr, err := engine.MergeTrees(ctx, s.Blobs, base.Tree, in.Tree, head.Tree, merge.Labels{
			Base:   "base " + base.Hash.Short(),
			Ours:   "workspace",
			Theirs: "effort " + eff.Head.Short(),
		})
		if err != nil {
			return LandResult{}, err
		}
		if !mr.Clean() {
			res.Conflicts = mr.Conflicts
			return res, nil
		}
		newTree = mr.Tree
		res.Rebased = true
		res.AutoMerged = mr.AutoMerged
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
		CreatedAt: s.Now(),
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
		h, err := tx.InsertChangeset(ctx, v.Repo.ID, &eff.ID, cs, files)
		if err != nil {
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
		res.Changeset = store.ChangesetRow{Hash: h, RepoID: v.Repo.ID, EffortID: &eff.ID, Changeset: cs}
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

// checkUploaded verifies every tree and blob the snapshot adds over base is
// in the blob store, so a land never references missing content.
func (s *Service) checkUploaded(ctx context.Context, baseTree, tree hash.Hash) error {
	if tree.IsZero() {
		return errf(CodeInvalid, "land needs a snapshot tree")
	}
	changes, err := object.DiffTrees(ctx, s.Blobs, baseTree, tree)
	if errors.Is(err, blob.ErrNotFound) {
		return errf(CodeFailedPrecondition, "snapshot trees are missing on the server: upload objects first (%v)", err)
	}
	if err != nil {
		return err
	}
	var missing []string
	for _, c := range changes {
		if c.Op == object.OpDelete {
			continue
		}
		ok, err := s.Blobs.Has(ctx, c.New.Hash)
		if err != nil {
			return err
		}
		if !ok {
			missing = append(missing, c.Path)
		}
	}
	if len(missing) > 0 {
		if len(missing) > 5 {
			missing = append(missing[:5], fmt.Sprintf("and %d more", len(missing)-5))
		}
		return errf(CodeFailedPrecondition, "file contents missing on the server: %s", strings.Join(missing, ", "))
	}
	return nil
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
