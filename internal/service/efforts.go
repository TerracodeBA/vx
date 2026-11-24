package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/store"
)

// CreateEffortInput describes a new effort.
type CreateEffortInput struct {
	Repo       string
	Name       string
	Goal       string
	BaseBranch string // empty: the repo's default branch
}

// CreateEffort starts an effort on the current head of its base branch
// (write).
func (s *Service) CreateEffort(ctx context.Context, actor Actor, in CreateEffortInput) (store.Effort, error) {
	if strings.TrimSpace(in.Name) == "" {
		return store.Effort{}, errf(CodeInvalid, "an effort needs a name")
	}
	repo, _, err := s.Authorize(ctx, actor, in.Repo, RoleWrite)
	if err != nil {
		return store.Effort{}, err
	}
	if in.BaseBranch == "" {
		in.BaseBranch = repo.DefaultBranch
	}
	branch, err := s.Branch(ctx, actor, repo, in.BaseBranch)
	if err != nil {
		return store.Effort{}, err
	}
	var eff store.Effort
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		eff, err = tx.CreateEffort(ctx, store.Effort{
			RepoID: repo.ID, Name: strings.TrimSpace(in.Name), Goal: strings.TrimSpace(in.Goal),
			BaseBranch: in.BaseBranch, Base: branch.Head, OwnerID: actor.Human().ID,
		})
		if err != nil {
			return err
		}
		return tx.LogOp(ctx, &repo.ID, &actor.ID, "effort.create", eff.Slug, nil,
			map[string]string{"base_branch": eff.BaseBranch, "base": eff.Base.String()})
	})
	return eff, classify(err)
}

// EffortView is an effort with its repo and open workspaces.
type EffortView struct {
	Repo       store.Repo
	Effort     store.Effort
	Workspaces []store.Workspace
	// Rebase is the effort's rebase in progress (running or stopped), if any.
	Rebase *store.Rebase
}

// GetEffort loads an effort by repo name and slug (read), with its rebase
// in progress if there is one.
func (s *Service) GetEffort(ctx context.Context, actor Actor, repoName, slug string) (EffortView, error) {
	v, err := s.effortFor(ctx, actor, repoName, slug, RoleRead)
	if err != nil {
		return EffortView{}, err
	}
	if r, err := s.Store.ActiveRebase(ctx, v.Effort.ID); err == nil {
		v.Rebase = &r
	} else if !errors.Is(err, store.ErrNotFound) {
		return EffortView{}, err
	}
	return v, nil
}

// ListEfforts lists a repo's efforts (read).
func (s *Service) ListEfforts(ctx context.Context, actor Actor, repoName string, state store.EffortState) (store.Repo, []store.Effort, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, RoleRead)
	if err != nil {
		return store.Repo{}, nil, err
	}
	effs, err := s.Store.ListEfforts(ctx, repo.ID, state)
	return repo, effs, err
}

// LogEntry is a landed changeset with its file list.
type LogEntry struct {
	store.ChangesetRow
	Files []store.FileChange
}

// Log lists changes landed in an effort since its base, newest first
// (read).
func (s *Service) Log(ctx context.Context, actor Actor, repoName, slug string, limit int, withFiles bool) ([]LogEntry, error) {
	v, err := s.GetEffort(ctx, actor, repoName, slug)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.Store.Chain(ctx, v.Repo.ID, v.Effort.Head, v.Effort.Base, limit)
	if err != nil {
		return nil, err
	}
	out := make([]LogEntry, 0, len(rows))
	for _, r := range rows {
		e := LogEntry{ChangesetRow: r}
		if withFiles {
			if e.Files, err = s.Store.FileChanges(ctx, v.Repo.ID, r.Hash); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// GetChangeset resolves a changeset by hash, prefix or change ID (read).
func (s *Service) GetChangeset(ctx context.Context, actor Actor, repoName, ref string) (store.Repo, LogEntry, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, RoleRead)
	if err != nil {
		return store.Repo{}, LogEntry{}, err
	}
	row, err := s.Store.ResolveChangeset(ctx, repo.ID, ref)
	if err != nil {
		return repo, LogEntry{}, classify(err)
	}
	files, err := s.Store.FileChanges(ctx, repo.ID, row.Hash)
	return repo, LogEntry{ChangesetRow: row, Files: files}, err
}

// WorkspaceView is a workspace with its effort and repo.
type WorkspaceView struct {
	Repo      store.Repo
	Effort    store.Effort
	Workspace store.Workspace
}

// OpenWorkspace starts a task in an effort, based on the effort's head
// (write).
func (s *Service) OpenWorkspace(ctx context.Context, actor Actor, repoName, slug, intent string) (WorkspaceView, error) {
	if strings.TrimSpace(intent) == "" {
		return WorkspaceView{}, errf(CodeInvalid, "say what this workspace is for: vx new \"<intent>\"")
	}
	v, err := s.effortFor(ctx, actor, repoName, slug, RoleWrite)
	if err != nil {
		return WorkspaceView{}, err
	}
	if v.Effort.State != store.EffortOpen {
		return WorkspaceView{}, errf(CodeFailedPrecondition, "effort %s is %s", slug, v.Effort.State)
	}
	var ws store.Workspace
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		if err := s.checkOpenWorkspaces(ctx, tx, actor); err != nil {
			return err
		}
		var err error
		ws, err = tx.CreateWorkspace(ctx, store.Workspace{EffortID: v.Effort.ID, OwnerID: actor.ID, Intent: strings.TrimSpace(intent), Base: v.Effort.Head})
		if err != nil {
			return err
		}
		return tx.LogOp(ctx, &v.Repo.ID, &actor.ID, "workspace.open", ws.ID.String(), nil, map[string]string{"effort": slug, "intent": ws.Intent})
	})
	if err != nil {
		return WorkspaceView{}, classify(err)
	}
	return WorkspaceView{Repo: v.Repo, Effort: v.Effort, Workspace: ws}, nil
}

// GetWorkspace loads a workspace with its effort and repo (read).
func (s *Service) GetWorkspace(ctx context.Context, actor Actor, id uuid.UUID) (WorkspaceView, error) {
	return s.workspaceFor(ctx, actor, id, RoleRead)
}

// loadWorkspace loads a workspace without an access check.
func (s *Service) loadWorkspace(ctx context.Context, id uuid.UUID) (WorkspaceView, error) {
	ws, err := s.Store.GetWorkspace(ctx, id)
	if err != nil {
		return WorkspaceView{}, classify(err)
	}
	eff, err := s.Store.EffortByID(ctx, ws.EffortID)
	if err != nil {
		return WorkspaceView{}, classify(err)
	}
	repo, err := s.Store.RepoByID(ctx, eff.RepoID)
	if err != nil {
		return WorkspaceView{}, classify(err)
	}
	return WorkspaceView{Repo: repo, Effort: eff, Workspace: ws}, nil
}

// ownWorkspace loads a workspace the actor owns and may still write to.
func (s *Service) ownWorkspace(ctx context.Context, actor Actor, id uuid.UUID) (WorkspaceView, error) {
	v, err := s.workspaceFor(ctx, actor, id, RoleWrite)
	if err != nil {
		return v, err
	}
	if v.Workspace.OwnerID != actor.ID {
		return v, errf(CodePermission, "workspace %s belongs to another principal", id)
	}
	// Its owner is working in it: its claims hold (see WorkspaceIdleAfter).
	if v.Workspace.State == store.WorkspaceOpen {
		now := s.Now()
		if err := s.Store.TouchWorkspace(ctx, id, now); err != nil {
			return v, err
		}
		v.Workspace.ActiveAt = &now
	}
	return v, nil
}

// SetBase records that the workspace's files now sit on base, which must be
// on the effort's chain (write).
func (s *Service) SetBase(ctx context.Context, actor Actor, id uuid.UUID, base hash.Hash) (WorkspaceView, error) {
	v, err := s.ownWorkspace(ctx, actor, id)
	if err != nil {
		return v, err
	}
	if v.Workspace.State != store.WorkspaceOpen {
		return v, errf(CodeFailedPrecondition, "workspace is %s", v.Workspace.State)
	}
	if err := s.checkOnEffort(ctx, v.Repo, v.Effort, base); err != nil {
		return v, err
	}
	if err := s.Store.SetWorkspaceBase(ctx, id, base); err != nil {
		return v, err
	}
	v.Workspace.Base = base
	return v, nil
}

// Abandon closes a workspace without landing it (write).
func (s *Service) Abandon(ctx context.Context, actor Actor, id uuid.UUID) (WorkspaceView, error) {
	v, err := s.ownWorkspace(ctx, actor, id)
	if err != nil {
		return v, err
	}
	if v.Workspace.State != store.WorkspaceOpen {
		return v, errf(CodeFailedPrecondition, "workspace is already %s", v.Workspace.State)
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.FinishWorkspace(ctx, id, store.WorkspaceAbandoned, hash.Zero); err != nil {
			return err
		}
		if err := tx.WithdrawReviews(ctx, id); err != nil {
			return err
		}
		if _, err := s.releaseWorkspaceClaims(ctx, tx, actor, v.Repo.ID, id, nil, s.Now()); err != nil {
			return err
		}
		return tx.LogOp(ctx, &v.Repo.ID, &actor.ID, "workspace.abandon", id.String(), nil, nil)
	})
	v.Workspace.State = store.WorkspaceAbandoned
	return v, err
}

// checkOnEffort verifies base is the effort's head or an earlier point of it.
func (s *Service) checkOnEffort(ctx context.Context, repo store.Repo, eff store.Effort, base hash.Hash) error {
	if base == eff.Head {
		return nil
	}
	ok, err := s.Store.OnChain(ctx, repo.ID, eff.Head, base, 100000)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	// A workspace opened before a rebase is based on the old chain, which
	// is still a valid merge base: its land merges onto the rebased head.
	heads, err := s.Store.FinishedRebaseHeads(ctx, eff.ID)
	if err != nil {
		return err
	}
	for _, h := range heads {
		if ok, err := s.Store.OnChain(ctx, repo.ID, h, base, 100000); err != nil {
			return err
		} else if ok {
			return nil
		}
	}
	return errf(CodeInvalid, "snapshot base %s is not part of effort %s", base.Short(), eff.Slug)
}
