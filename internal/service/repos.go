package service

import (
	"context"
	"strings"

	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// CreateRepoInput describes a new repo.
type CreateRepoInput struct {
	Name          string
	Provider      store.Provider
	Remote        string
	DefaultBranch string
	// Org owns the repo. Empty: the organization named like the repo's
	// owner part when the caller may create repos there, else the
	// caller's personal organization.
	Org string
}

// CreateRepo registers a repo. A repo with an upstream imports its default
// branch right away; a repo without one starts from an empty snapshot.
func (s *Service) CreateRepo(ctx context.Context, actor Actor, in CreateRepoInput) (store.Repo, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Provider == "" {
		in.Provider = store.ProviderNone
	}
	switch in.Provider {
	case store.ProviderNone:
	case store.ProviderGitHub, store.ProviderGit:
		if s.Upstream == nil {
			return store.Repo{}, errf(CodeFailedPrecondition, "this server has no git bridge configured")
		}
		if strings.TrimSpace(in.Remote) == "" {
			return store.Repo{}, errf(CodeInvalid, "a %s repo needs a remote", in.Provider)
		}
	default:
		return store.Repo{}, errf(CodeInvalid, "unknown provider %q", in.Provider)
	}
	if err := store.ValidateRepoName(in.Name); err != nil {
		return store.Repo{}, errf(CodeInvalid, "%v", err)
	}
	org, err := s.repoOrg(ctx, actor, in.Org, in.Name)
	if err != nil {
		return store.Repo{}, err
	}
	repo := store.Repo{Name: in.Name, Provider: in.Provider, Remote: in.Remote, DefaultBranch: in.DefaultBranch, OrgID: org.ID, Org: org.Handle}
	if repo.DefaultBranch == "" && in.Provider != store.ProviderNone {
		b, err := s.Upstream.DefaultBranch(ctx, repo)
		if err != nil {
			return store.Repo{}, errf(CodeUnavailable, "could not reach %s: %v", in.Remote, err)
		}
		repo.DefaultBranch = b
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "main"
	}

	var created store.Repo
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		created, err = tx.CreateRepo(ctx, repo)
		if err != nil {
			return err
		}
		if in.Provider != store.ProviderNone {
			return tx.LogOp(ctx, &created.ID, &actor.ID, "repo.create", created.Name, nil, created)
		}
		empty, err := object.EmptyTree(ctx, s.Blobs)
		if err != nil {
			return err
		}
		root := &object.Changeset{
			Version:   object.CurrentVersion,
			ChangeID:  object.NewChangeID(),
			Tree:      empty,
			Origin:    object.OriginUpstream,
			Author:    object.Author{Principal: actor.Ref(), Name: actor.DisplayName, Email: actor.Email},
			Intent:    "Empty repository",
			CreatedAt: s.Now(),
		}
		h, err := tx.InsertChangeset(ctx, created.ID, nil, root, nil)
		if err != nil {
			return err
		}
		if err := tx.UpsertBranch(ctx, store.Branch{RepoID: created.ID, Name: created.DefaultBranch, Head: h}); err != nil {
			return err
		}
		return tx.LogOp(ctx, &created.ID, &actor.ID, "repo.create", created.Name, nil, created)
	})
	if err != nil {
		return store.Repo{}, classify(err)
	}
	if in.Provider != store.ProviderNone {
		if _, _, err := s.syncBranch(ctx, actor, created, created.DefaultBranch); err != nil {
			return created, err
		}
	}
	return created, nil
}

// SyncBranch imports the latest commits of an upstream branch (write). For
// repos without an upstream it returns the branch as it is.
func (s *Service) SyncBranch(ctx context.Context, actor Actor, repoName, branch string) (store.Branch, int, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, RoleWrite)
	if err != nil {
		return store.Branch{}, 0, err
	}
	return s.syncBranch(ctx, actor, repo, branch)
}

// syncBranch is SyncBranch for a repository the caller was authorized on.
func (s *Service) syncBranch(ctx context.Context, actor Actor, repo store.Repo, branch string) (store.Branch, int, error) {
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if repo.Provider == store.ProviderNone {
		b, err := s.Store.GetBranch(ctx, repo.ID, branch)
		return b, 0, classify(err)
	}
	if s.Upstream == nil {
		return store.Branch{}, 0, errf(CodeFailedPrecondition, "this server has no git bridge configured")
	}
	b, n, err := s.Upstream.SyncBranch(ctx, repo, branch)
	if err != nil {
		return store.Branch{}, 0, classify(err)
	}
	if n > 0 {
		_ = s.Store.LogOp(ctx, &repo.ID, &actor.ID, "branch.sync", branch, nil, map[string]any{"head": b.Head.String(), "git_sha": b.GitSHA, "imported": n})
		s.scheduleBranchForecast(repo, branch)
	}
	return b, n, nil
}

// Branch returns a tracked branch, importing it first if vx does not follow
// it yet. This is how efforts can start from any branch on the fly. The
// caller has authorized actor on repo already.
func (s *Service) Branch(ctx context.Context, actor Actor, repo store.Repo, name string) (store.Branch, error) {
	b, err := s.Store.GetBranch(ctx, repo.ID, name)
	if err == nil {
		return b, nil
	}
	if repo.Provider == store.ProviderNone {
		return store.Branch{}, errf(CodeNotFound, "branch %q does not exist in %s", name, repo.Name)
	}
	b, _, err = s.syncBranch(ctx, actor, repo, name)
	return b, err
}
