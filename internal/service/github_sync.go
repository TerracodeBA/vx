package service

// SyncInstallation: an installation of the App becomes a vx organization
// (linked by github_org_id) with one vx repo per GitHub repository it can
// reach (linked by github_repo_id). Repositories removed from the
// installation stay in vx but are unlinked from it, so GitHub-backed roles
// no longer reach them (they fail closed).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// SyncInstallation reads an installation's account and repositories from
// GitHub and creates or updates the matching vx organization and repos.
func (s *Service) SyncInstallation(ctx context.Context, installationID int64) error {
	api, err := s.GitHubClient(ctx)
	if err != nil {
		return err
	}
	in, err := api.Installation(ctx, installationID)
	if err != nil {
		return errf(CodeUnavailable, "read GitHub installation %d: %v", installationID, err)
	}
	org, err := s.githubInstallationOrg(ctx, in)
	if err != nil {
		return err
	}
	row := store.GitHubInstallationRow{
		ID: in.ID, AccountID: in.AccountID, AccountLogin: in.AccountLogin, AccountType: in.AccountType,
		RepositorySelection: in.RepositorySelection, SuspendedAt: in.SuspendedAt,
	}
	if row.RepositorySelection == "" {
		row.RepositorySelection = "selected"
	}
	if org != nil {
		row.OrgID = &org.ID
	}
	if err := s.Store.UpsertGitHubInstallation(ctx, row); err != nil {
		return err
	}
	if in.SuspendedAt != nil {
		// A suspended installation gets no tokens: keep everything, drop
		// cached roles so they are asked again (and fail) right away.
		return s.DropGitHubRoles(ctx, GitHubRoleScope{InstallationID: in.ID, Reason: "installation suspended"})
	}
	if org == nil {
		// A personal account whose person has not signed in yet: their
		// repositories come in when they do (SyncGitHubAccount).
		return nil
	}
	repos, err := api.InstallationRepos(ctx, installationID)
	if err != nil {
		return errf(CodeUnavailable, "list repositories of GitHub installation %d: %v", installationID, err)
	}
	var errs []error
	reached := map[uuid.UUID]bool{}
	for _, gr := range repos {
		r, err := s.syncGitHubRepo(ctx, *org, in.ID, gr)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", gr.FullName, err))
			continue
		}
		reached[r.ID] = true
	}
	linked, err := s.Store.ReposOfGitHubInstallation(ctx, in.ID)
	if err != nil {
		return err
	}
	for _, r := range linked {
		if !reached[r.ID] {
			if err := s.unlinkGitHubRepo(ctx, r, "removed from the installation"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// SyncGitHubAccount syncs the installations on a GitHub account that have
// no vx organization yet: call it when the person of a GitHub user account
// signs in for the first time, so their personal installation comes in.
func (s *Service) SyncGitHubAccount(ctx context.Context, githubAccountID int64) error {
	ins, err := s.Store.ListGitHubInstallations(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, in := range ins {
		if in.AccountID == githubAccountID && in.OrgID == nil {
			errs = append(errs, s.SyncInstallation(ctx, in.ID))
		}
	}
	return errors.Join(errs...)
}

// githubInstallationOrg finds or creates the vx organization of an
// installation's account: a GitHub organization becomes an organization
// with handle = login lowercased; a user account is that person's personal
// organization once they exist (nil before).
func (s *Service) githubInstallationOrg(ctx context.Context, in GitHubInstallation) (*store.Org, error) {
	if o, err := s.Store.OrgByGitHubID(ctx, in.AccountID); err == nil {
		if o.GitHubLogin != in.AccountLogin {
			if err := s.Store.SetOrgGitHub(ctx, o.ID, &in.AccountID, in.AccountLogin); err != nil {
				return nil, err
			}
		}
		return &o, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	var org store.Org
	switch in.AccountType {
	case "User":
		p, err := s.Store.PrincipalByGitHubID(ctx, in.AccountID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if org, err = s.Store.OrgByHandle(ctx, p.Handle); err != nil {
			return nil, err
		}
	case "Organization":
		handle := strings.ToLower(in.AccountLogin)
		o, err := s.Store.OrgByHandle(ctx, handle)
		switch {
		case err == nil && !o.Personal && o.GitHubOrgID == nil:
			org = o // an organization made in vx before the App came: link it
		case err == nil || errors.Is(err, store.ErrNotFound):
			if err == nil {
				handle += "-github" // the handle belongs to a person or another GitHub account
			}
			org, err = s.Store.CreateOrgWithoutOwner(ctx, handle, in.AccountLogin)
			if errors.Is(err, store.ErrExists) {
				if org, err = s.Store.OrgByHandle(ctx, handle); err == nil && (org.Personal || (org.GitHubOrgID != nil && *org.GitHubOrgID != in.AccountID)) {
					err = errf(CodeExists, "the vx organization %s belongs to someone else; rename it to bring in GitHub %s", handle, in.AccountLogin)
				}
			}
			if err != nil {
				return nil, fmt.Errorf("create the organization for GitHub %s: %w", in.AccountLogin, err)
			}
		default:
			return nil, err
		}
	default:
		return nil, errf(CodeInvalid, "GitHub installation %d is on a %q account", in.ID, in.AccountType)
	}
	if err := s.Store.SetOrgGitHub(ctx, org.ID, &in.AccountID, in.AccountLogin); err != nil {
		return nil, err
	}
	_ = s.Store.LogOp(ctx, nil, nil, "github.org.link", org.Handle, nil, map[string]any{"github_org_id": in.AccountID, "login": in.AccountLogin})
	org.GitHubOrgID, org.GitHubLogin = &in.AccountID, in.AccountLogin
	return &org, nil
}

// syncGitHubRepo creates or updates the vx repo of a GitHub repository
// and links it to the installation. A new repo imports its default branch.
func (s *Service) syncGitHubRepo(ctx context.Context, org store.Org, installationID int64, gr GitHubRepo) (store.Repo, error) {
	branch := gr.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	r, err := s.Store.RepoByGitHubID(ctx, gr.ID)
	if err == nil {
		if r.Remote != gr.FullName || r.DefaultBranch != branch || r.Provider != store.ProviderGitHub {
			if err := s.Store.SetRepoUpstream(ctx, r.ID, gr.FullName, branch); err != nil {
				return r, err
			}
			r.Remote, r.DefaultBranch = gr.FullName, branch
		}
		if r.GitHubInstallationID == nil || *r.GitHubInstallationID != installationID {
			if err := s.Store.SetRepoGitHub(ctx, r.ID, &gr.ID, &installationID); err != nil {
				return r, err
			}
			_ = s.DropGitHubRoles(ctx, GitHubRoleScope{RepoID: &r.ID, Reason: "linked to an installation"})
		}
		return r, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return r, err
	}
	_, name, _ := strings.Cut(gr.FullName, "/")
	vxName := org.Handle + "/" + strings.ToLower(name)
	created := false
	r, err = s.Store.RepoByName(ctx, vxName)
	switch {
	case err == nil:
		// A repo made in vx for the same GitHub repository: link it.
		if r.GitHubRepoID != nil || r.Provider != store.ProviderGitHub || !strings.EqualFold(r.Remote, gr.FullName) {
			return r, errf(CodeExists, "vx already has a repository %s that is not %s on GitHub; rename one of them", vxName, gr.FullName)
		}
		if err := s.Store.SetRepoUpstream(ctx, r.ID, gr.FullName, r.DefaultBranch); err != nil {
			return r, err
		}
	case errors.Is(err, store.ErrNotFound):
		r, err = s.Store.CreateRepo(ctx, store.Repo{Name: vxName, Provider: store.ProviderGitHub, Remote: gr.FullName, DefaultBranch: branch, OrgID: org.ID})
		if errors.Is(err, store.ErrExists) { // created meanwhile by a concurrent sync
			return s.syncGitHubRepo(ctx, org, installationID, gr)
		}
		if err != nil {
			return r, err
		}
		created = true
	default:
		return r, err
	}
	if err := s.Store.SetRepoGitHub(ctx, r.ID, &gr.ID, &installationID); err != nil {
		return r, err
	}
	r.GitHubRepoID, r.GitHubInstallationID = &gr.ID, &installationID
	_ = s.Store.LogOp(ctx, &r.ID, nil, "github.repo.link", r.Name, nil, map[string]any{"github_repo_id": gr.ID, "full_name": gr.FullName, "installation": installationID})
	if created && s.Upstream != nil {
		// The first import; a failure leaves the repo, and the next push
		// or `vx repo sync` imports it.
		_, _, _ = s.Upstream.SyncBranch(ctx, r, branch)
	}
	return r, nil
}

// unlinkGitHubRepo keeps a repo but takes it out of its installation.
func (s *Service) unlinkGitHubRepo(ctx context.Context, r store.Repo, why string) error {
	if err := s.Store.SetRepoGitHub(ctx, r.ID, r.GitHubRepoID, nil); err != nil {
		return err
	}
	_ = s.Store.LogOp(ctx, &r.ID, nil, "github.repo.unlink", r.Name, nil, map[string]any{"reason": why})
	return s.DropGitHubRoles(ctx, GitHubRoleScope{RepoID: &r.ID, Reason: why})
}
