package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/secret"
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
	// BaseURL is the host's web address for GitLab, Gitea, Forgejo and
	// Bitbucket (empty: the server's default for the provider).
	BaseURL string
	// Token is a token for this repository only (GitLab, Gitea, Forgejo,
	// Bitbucket), kept sealed with VX_SECRET_KEY. Empty: the server's
	// token for the host.
	Token string
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
	case store.ProviderGitLab, store.ProviderGitea, store.ProviderForgejo, store.ProviderBitbucket:
		if s.Upstream == nil {
			return store.Repo{}, errf(CodeFailedPrecondition, "this server has no git bridge configured")
		}
		remote, err := normalizeHostRemote(in.Provider, in.Remote)
		if err != nil {
			return store.Repo{}, err
		}
		base, err := s.hostBaseURL(in.Provider, in.BaseURL)
		if err != nil {
			return store.Repo{}, err
		}
		in.Remote, in.BaseURL = remote, base
		if in.Token = strings.TrimSpace(in.Token); in.Token != "" {
			if _, err := s.secretKey(); err != nil {
				return store.Repo{}, errf(CodeFailedPrecondition, "vx keeps a repository's token sealed with the server key, and this server has none: set VX_SECRET_KEY, or give the server a token for the whole host (VX_%s_TOKEN)", strings.ToUpper(string(in.Provider)))
			}
			ctx = withHostToken(ctx, in.Token)
		}
	default:
		return store.Repo{}, errf(CodeInvalid, "unknown provider %q", in.Provider)
	}
	if !in.Provider.Hosted() && (in.BaseURL != "" || in.Token != "") {
		return store.Repo{}, errf(CodeInvalid, "a host address and token are for GitLab, Gitea, Forgejo and Bitbucket repositories, not %s ones", in.Provider)
	}
	if err := store.ValidateRepoName(in.Name); err != nil {
		return store.Repo{}, errf(CodeInvalid, "%v", err)
	}
	org, err := s.repoOrg(ctx, actor, in.Org, in.Name)
	if err != nil {
		return store.Repo{}, err
	}
	repo := store.Repo{Name: in.Name, Provider: in.Provider, Remote: in.Remote, DefaultBranch: in.DefaultBranch, BaseURL: in.BaseURL, OrgID: org.ID, Org: org.Handle}
	if repo.DefaultBranch == "" && in.Provider != store.ProviderNone {
		b, err := s.Upstream.DefaultBranch(ctx, repo)
		if err != nil {
			hint := ""
			if in.Provider.Hosted() && in.Token == "" {
				hint = fmt.Sprintf("; for a private repository give vx a token: --token-file FILE (or VX_%s_TOKEN on the server)", strings.ToUpper(string(in.Provider)))
			}
			return store.Repo{}, errf(CodeUnavailable, "could not reach %s: %v%s", in.Remote, err, hint)
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
		if in.Token != "" {
			key, err := s.secretKey()
			if err != nil {
				return err
			}
			sealed, err := secret.SealString(key, tokenPurpose(created), in.Token)
			if err != nil {
				return err
			}
			if err := tx.SetUpstreamToken(ctx, created.ID, sealed); err != nil {
				return err
			}
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
	b, n, err := s.syncBranch(ctx, actor, repo, branch)
	if err == nil && repo.Provider != store.ProviderNone {
		if branch == "" {
			branch = repo.DefaultBranch
		}
		s.autoRebase(ctx, repo, branch, "vx repo sync by "+actor.Ref())
	}
	return b, n, err
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
	// On every sync, not only when commits came in: an effort's changes may
	// have reached the branch through an import vx did earlier.
	s.markMergedEfforts(ctx, repo, branch)
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
