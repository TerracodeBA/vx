package service

// Access checks for records that belong to a repository. Every service
// function that takes a repository name, or a record that lives in one
// (effort, workspace, changeset, shipment, note), checks the caller's role
// through Authorize or AuthorizeID before it reads or changes anything.
// The role each operation needs is listed in docs/organizations.md and
// tested in authz_test.go.

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// RepoView is a repository with the caller's role on it.
type RepoView struct {
	store.Repo
	Role RepoRole
}

// GetRepo returns a repository the caller can read, with its branches.
func (s *Service) GetRepo(ctx context.Context, actor Actor, name string) (RepoView, []store.Branch, error) {
	repo, role, err := s.Authorize(ctx, actor, name, RoleRead)
	if err != nil {
		return RepoView{}, nil, err
	}
	branches, err := s.Store.ListBranches(ctx, repo.ID)
	if err != nil {
		return RepoView{}, nil, err
	}
	return RepoView{Repo: repo, Role: role}, branches, nil
}

// ListRepos lists the repositories the caller can read, with the caller's
// role on each.
func (s *Service) ListRepos(ctx context.Context, actor Actor) ([]RepoView, error) {
	repos, roles, err := s.ReadableRepos(ctx, actor, nil)
	if err != nil {
		return nil, err
	}
	out := make([]RepoView, len(repos))
	for i, r := range repos {
		out[i] = RepoView{Repo: r, Role: roles[i]}
	}
	return out, nil
}

// readableFilter returns a predicate for repository ids the actor can read.
func (s *Service) readableFilter(ctx context.Context, actor Actor) (func(uuid.UUID) bool, error) {
	ids, all, err := s.ReadableRepoIDs(ctx, actor)
	if err != nil {
		return nil, err
	}
	if all {
		return func(uuid.UUID) bool { return true }, nil
	}
	set := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return func(id uuid.UUID) bool { return set[id] }, nil
}

// isHidden reports whether err means the caller may not see the record at
// all (as opposed to seeing it without the role an operation needs).
func isHidden(err error) bool {
	var se *Error
	return errors.As(err, &se) && se.Code == CodeNotFound
}

// effortFor authorizes need on the repository and loads the effort.
func (s *Service) effortFor(ctx context.Context, actor Actor, repoName, slug string, need RepoRole) (EffortView, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, need)
	if err != nil {
		return EffortView{}, err
	}
	eff, err := s.Store.GetEffort(ctx, repo.ID, slug)
	if err != nil {
		return EffortView{}, classify(err)
	}
	ws, err := s.Store.ListWorkspaces(ctx, eff.ID, store.WorkspaceOpen)
	if err != nil {
		return EffortView{}, err
	}
	return EffortView{Repo: repo, Effort: eff, Workspaces: ws}, nil
}

// workspaceFor loads a workspace and checks need on its repository. A
// workspace in a repository the caller cannot read is reported as not
// found.
func (s *Service) workspaceFor(ctx context.Context, actor Actor, id uuid.UUID, need RepoRole) (WorkspaceView, error) {
	v, err := s.loadWorkspace(ctx, id)
	if err != nil {
		return WorkspaceView{}, err
	}
	if _, err := s.AuthorizeID(ctx, actor, v.Repo.ID, need); err != nil {
		if isHidden(err) {
			return WorkspaceView{}, errf(CodeNotFound, "workspace %s not found", id)
		}
		return WorkspaceView{}, err
	}
	return v, nil
}

// noteFor checks need on the repository a note belongs to. A note in a
// repository the caller cannot read is reported as not found.
func (s *Service) noteFor(ctx context.Context, actor Actor, n store.Note, need RepoRole) error {
	if _, err := s.AuthorizeID(ctx, actor, n.RepoID, need); err != nil {
		if isHidden(err) {
			return errf(CodeNotFound, "no note %s; `vx board` lists the effort's notes", shortID(n.ID))
		}
		return err
	}
	return nil
}

// filterNotes keeps the notes in repositories the caller can read.
func filterNotes(rows []store.Note, readable func(uuid.UUID) bool) []store.Note {
	out := rows[:0]
	for _, r := range rows {
		if readable(r.RepoID) {
			out = append(out, r)
		}
	}
	return out
}
