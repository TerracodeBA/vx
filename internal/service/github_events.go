package service

// Applying verified webhook deliveries. Events vx does not use are
// accepted and ignored. Permission changes never compute roles here: they
// drop cached roles (vx-ghperms' DropGitHubRoles), so the next request
// asks GitHub.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

type hookAccount struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type hookRepo struct {
	ID            int64       `json:"id"`
	Name          string      `json:"name"`
	FullName      string      `json:"full_name"`
	Private       bool        `json:"private"`
	DefaultBranch string      `json:"default_branch"`
	Owner         hookAccount `json:"owner"`
}

type hookPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID                  int64       `json:"id"`
		Account             hookAccount `json:"account"`
		RepositorySelection string      `json:"repository_selection"`
		SuspendedAt         *string     `json:"suspended_at"`
	} `json:"installation"`
	RepositoriesRemoved []hookRepo  `json:"repositories_removed"`
	Repository          *hookRepo   `json:"repository"`
	Organization        hookAccount `json:"organization"`
	Member              hookAccount `json:"member"`
	Membership          struct {
		User  hookAccount `json:"user"`
		Role  string      `json:"role"`
		State string      `json:"state"`
	} `json:"membership"`
	Ref     string `json:"ref"`
	Deleted bool   `json:"deleted"`
}

// applyGitHubEvent applies one verified delivery.
func (s *Service) applyGitHubEvent(ctx context.Context, event string, body []byte) error {
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return errf(CodeInvalid, "the %s delivery does not parse: %v", event, err)
	}
	inst := p.Installation.ID
	switch event {
	case "installation":
		switch p.Action {
		case "deleted":
			// The repos stay linked (fail closed); re-installing relinks them.
			if err := s.DropGitHubRoles(ctx, GitHubRoleScope{InstallationID: inst, Reason: "installation deleted"}); err != nil {
				return err
			}
			if err := s.Store.DeleteGitHubInstallation(ctx, inst); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			return s.Store.LogOp(ctx, nil, nil, "github.installation.delete", p.Installation.Account.Login, nil, map[string]any{"installation": inst})
		case "suspend":
			if err := s.DropGitHubRoles(ctx, GitHubRoleScope{InstallationID: inst, Reason: "installation suspended"}); err != nil {
				return err
			}
			return s.SyncInstallation(ctx, inst)
		default: // created, unsuspend, new_permissions_accepted
			return s.SyncInstallation(ctx, inst)
		}

	case "installation_repositories":
		if p.Action == "removed" {
			for _, gr := range p.RepositoriesRemoved {
				r, err := s.Store.RepoByGitHubID(ctx, gr.ID)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if err := s.unlinkGitHubRepo(ctx, r, "removed from the installation"); err != nil {
					return err
				}
			}
			return nil
		}
		return s.SyncInstallation(ctx, inst)

	case "repository":
		return s.applyGitHubRepositoryEvent(ctx, p)

	case "push":
		return s.applyGitHubPush(ctx, p)

	case "member": // a repository collaborator added, removed or changed
		if p.Repository == nil {
			return nil
		}
		r, err := s.Store.RepoByGitHubID(ctx, p.Repository.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.DropGitHubRoles(ctx, GitHubRoleScope{RepoID: &r.ID, Reason: "collaborator " + p.Action})

	case "membership": // team membership
		return s.DropGitHubRoles(ctx, GitHubRoleScope{GitHubUserID: p.Member.ID, Reason: "team membership " + p.Action})

	case "organization":
		switch p.Action {
		case "member_added":
			return s.ApplyGitHubOrgMembership(ctx, p.Organization.ID, p.Membership.User.ID, p.Membership.Role)
		case "member_removed":
			return s.ApplyGitHubOrgMembership(ctx, p.Organization.ID, p.Membership.User.ID, "")
		case "renamed":
			o, err := s.Store.OrgByGitHubID(ctx, p.Organization.ID)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			id := p.Organization.ID
			return s.Store.SetOrgGitHub(ctx, o.ID, &id, p.Organization.Login)
		}
		return s.DropGitHubRoles(ctx, GitHubRoleScope{GitHubOrgID: p.Organization.ID, Reason: "organization " + p.Action})

	case "team", "team_add":
		return s.DropGitHubRoles(ctx, GitHubRoleScope{GitHubOrgID: p.Organization.ID, Reason: event + " " + p.Action})
	}
	// ping, pull_request, check runs, ...: nothing to do.
	return nil
}

func (s *Service) applyGitHubRepositoryEvent(ctx context.Context, p hookPayload) error {
	if p.Repository == nil {
		return nil
	}
	gr := *p.Repository
	r, err := s.Store.RepoByGitHubID(ctx, gr.ID)
	if errors.Is(err, store.ErrNotFound) {
		if p.Action == "transferred" && p.Installation.ID != 0 {
			// Moved into an account the App is installed on.
			return s.SyncInstallation(ctx, p.Installation.ID)
		}
		return nil
	}
	if err != nil {
		return err
	}
	switch p.Action {
	case "deleted":
		return s.unlinkGitHubRepo(ctx, r, "deleted on GitHub")
	case "renamed", "transferred", "edited":
		branch := gr.DefaultBranch
		if branch == "" {
			branch = r.DefaultBranch
		}
		if gr.FullName != "" && (gr.FullName != r.Remote || branch != r.DefaultBranch) {
			if err := s.Store.SetRepoUpstream(ctx, r.ID, gr.FullName, branch); err != nil {
				return err
			}
			_ = s.Store.LogOp(ctx, &r.ID, nil, "github.repo."+p.Action, r.Name, map[string]any{"remote": r.Remote, "default_branch": r.DefaultBranch}, map[string]any{"remote": gr.FullName, "default_branch": branch})
		}
		if err := s.DropGitHubRoles(ctx, GitHubRoleScope{RepoID: &r.ID, Reason: "repository " + p.Action}); err != nil {
			return err
		}
		if p.Action == "transferred" && p.Installation.ID != 0 {
			// The new owner may have another installation, or none.
			return s.SyncInstallation(ctx, p.Installation.ID)
		}
		return nil
	case "privatized", "publicized", "archived", "unarchived":
		return s.DropGitHubRoles(ctx, GitHubRoleScope{RepoID: &r.ID, Reason: "repository " + p.Action})
	}
	return nil
}

// applyGitHubPush imports a push to the default branch, or to another
// branch vx follows, like `vx repo sync`.
func (s *Service) applyGitHubPush(ctx context.Context, p hookPayload) error {
	if p.Repository == nil || p.Deleted || s.Upstream == nil {
		return nil
	}
	branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	if !ok {
		return nil
	}
	r, err := s.Store.RepoByGitHubID(ctx, p.Repository.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if branch != r.DefaultBranch {
		if _, err := s.Store.GetBranch(ctx, r.ID, branch); err != nil {
			return nil // a branch vx does not follow
		}
	}
	b, n, err := s.Upstream.SyncBranch(ctx, r, branch)
	if err != nil {
		return errf(CodeUnavailable, "import the push to %s of %s: %v", branch, r.Name, err)
	}
	if n > 0 {
		_ = s.Store.LogOp(ctx, &r.ID, nil, "branch.sync", branch, nil, map[string]any{"head": b.Head.String(), "git_sha": b.GitSHA, "imported": n, "by": "github push"})
	}
	return nil
}
