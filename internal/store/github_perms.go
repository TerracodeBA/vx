package store

// Store side of GitHub-backed repositories: links of repositories and
// organizations to GitHub, and the caches of what GitHub said about a
// person's role on a repository and membership in an organization.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SetRepoGitHub links a repository to a GitHub repository through an
// installation of the App (both nil: unlink). A linked repository is
// GitHub-backed. Cached roles on it are dropped.
func (q Q) SetRepoGitHub(ctx context.Context, repo uuid.UUID, githubRepoID, installationID *int64) error {
	tag, err := q.db.Exec(ctx, `update repos set github_repo_id = $2, github_installation_id = $3 where id = $1`,
		repo, githubRepoID, installationID)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("repo: %w", ErrNotFound)
	}
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `delete from github_role_cache where repo_id = $1`, repo)
	return err
}

// SetOrgGitHub links an organization to a GitHub organization (nil:
// unlink). Its membership then follows GitHub.
func (q Q) SetOrgGitHub(ctx context.Context, org uuid.UUID, githubOrgID *int64, login string) error {
	var l *string
	if githubOrgID != nil {
		l = &login
	}
	tag, err := q.db.Exec(ctx, `update orgs set github_org_id = $2, github_login = $3 where id = $1`, org, githubOrgID, l)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("organization: %w", ErrNotFound)
	}
	return err
}

// OrgByGitHubID looks up the organization linked to a GitHub organization.
func (q Q) OrgByGitHubID(ctx context.Context, githubOrgID int64) (Org, error) {
	o, err := scanOrg(q.db.QueryRow(ctx, `select `+orgCols+` from orgs o where o.github_org_id = $1`, githubOrgID))
	return o, notFound(err, fmt.Sprintf("organization linked to GitHub organization %d", githubOrgID))
}

// CachedRole is what GitHub said, and when.
type CachedRole struct {
	Role      string
	CheckedAt time.Time
}

// GitHubRoleCache returns principal's cached roles on repos, by repo id.
// Repositories without a cached role are missing from the map.
func (q Q) GitHubRoleCache(ctx context.Context, principal uuid.UUID, repos []uuid.UUID) (map[uuid.UUID]CachedRole, error) {
	rows, err := q.db.Query(ctx, `select repo_id, role, checked_at from github_role_cache
		where principal_id = $1 and repo_id = any($2)`, principal, repos)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]CachedRole{}
	for rows.Next() {
		var id uuid.UUID
		var c CachedRole
		if err := rows.Scan(&id, &c.Role, &c.CheckedAt); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// PutGitHubRole caches principal's role on repo ("" for none).
func (q Q) PutGitHubRole(ctx context.Context, repo, principal uuid.UUID, role string, checkedAt time.Time) error {
	_, err := q.db.Exec(ctx, `insert into github_role_cache (repo_id, principal_id, role, checked_at) values ($1, $2, $3, $4)
		on conflict (repo_id, principal_id) do update set role = excluded.role, checked_at = excluded.checked_at`,
		repo, principal, role, checkedAt)
	return err
}

// GitHubMembershipCache returns principal's cached memberships in GitHub
// organizations, by GitHub organization id.
func (q Q) GitHubMembershipCache(ctx context.Context, principal uuid.UUID) (map[int64]CachedRole, error) {
	rows, err := q.db.Query(ctx, `select github_org_id, role, checked_at from github_org_membership_cache
		where principal_id = $1`, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]CachedRole{}
	for rows.Next() {
		var id int64
		var c CachedRole
		if err := rows.Scan(&id, &c.Role, &c.CheckedAt); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// PutGitHubMembership caches principal's membership in a GitHub
// organization ("" for none).
func (q Q) PutGitHubMembership(ctx context.Context, githubOrgID int64, principal uuid.UUID, role string, checkedAt time.Time) error {
	_, err := q.db.Exec(ctx, `insert into github_org_membership_cache (github_org_id, principal_id, role, checked_at) values ($1, $2, $3, $4)
		on conflict (github_org_id, principal_id) do update set role = excluded.role, checked_at = excluded.checked_at`,
		githubOrgID, principal, role, checkedAt)
	return err
}

// GitHubCacheScope selects cached GitHub answers to drop. Every set field
// adds its own rows (the fields are OR-ed): RepoID or GitHubRepoID that
// repository's roles; GitHubOrgID the roles on every repository of
// installations on that account and memberships in it; GitHubUserID every
// row of the person linked to that GitHub user; InstallationID the roles
// on every repository of that installation and memberships in its account.
type GitHubCacheScope struct {
	RepoID         *uuid.UUID
	GitHubRepoID   int64
	GitHubOrgID    int64
	GitHubUserID   int64
	InstallationID int64
}

// DropGitHubCache deletes the cached roles and memberships in scope and
// reports how many rows went.
func (q Q) DropGitHubCache(ctx context.Context, s GitHubCacheScope) (int64, error) {
	var n int64
	exec := func(sql string, args ...any) error {
		tag, err := q.db.Exec(ctx, sql, args...)
		n += tag.RowsAffected()
		return err
	}
	if s.RepoID != nil {
		if err := exec(`delete from github_role_cache where repo_id = $1`, *s.RepoID); err != nil {
			return n, err
		}
	}
	if s.GitHubRepoID != 0 {
		if err := exec(`delete from github_role_cache c using repos r where r.id = c.repo_id and r.github_repo_id = $1`, s.GitHubRepoID); err != nil {
			return n, err
		}
	}
	if s.GitHubOrgID != 0 {
		if err := exec(`delete from github_role_cache c using repos r, github_installations i
			where r.id = c.repo_id and i.id = r.github_installation_id and i.account_id = $1`, s.GitHubOrgID); err != nil {
			return n, err
		}
		if err := exec(`delete from github_org_membership_cache where github_org_id = $1`, s.GitHubOrgID); err != nil {
			return n, err
		}
	}
	if s.GitHubUserID != 0 {
		// The person's agents never have rows: roles are cached for people.
		if err := exec(`delete from github_role_cache c using principals p where p.id = c.principal_id and p.github_user_id = $1`, s.GitHubUserID); err != nil {
			return n, err
		}
		if err := exec(`delete from github_org_membership_cache c using principals p where p.id = c.principal_id and p.github_user_id = $1`, s.GitHubUserID); err != nil {
			return n, err
		}
	}
	if s.InstallationID != 0 {
		if err := exec(`delete from github_role_cache c using repos r where r.id = c.repo_id and r.github_installation_id = $1`, s.InstallationID); err != nil {
			return n, err
		}
		if err := exec(`delete from github_org_membership_cache c using github_installations i
			where i.id = $1 and c.github_org_id = i.account_id`, s.InstallationID); err != nil {
			return n, err
		}
	}
	return n, nil
}

// GitHubInstallationRow is an installation of the App as vx records it.
type GitHubInstallationRow struct {
	ID                  int64
	AccountID           int64
	AccountLogin        string
	AccountType         string // Organization or User
	OrgID               *uuid.UUID
	RepositorySelection string
	SuspendedAt         *time.Time
}

const installationCols = `i.id, i.account_id, i.account_login, i.account_type, i.org_id, i.repository_selection, i.suspended_at`

func scanInstallation(row interface{ Scan(...any) error }) (GitHubInstallationRow, error) {
	var i GitHubInstallationRow
	err := row.Scan(&i.ID, &i.AccountID, &i.AccountLogin, &i.AccountType, &i.OrgID, &i.RepositorySelection, &i.SuspendedAt)
	return i, err
}

// GitHubInstallationByID returns one installation.
func (q Q) GitHubInstallationByID(ctx context.Context, id int64) (GitHubInstallationRow, error) {
	i, err := scanInstallation(q.db.QueryRow(ctx, `select `+installationCols+` from github_installations i where i.id = $1`, id))
	return i, notFound(err, fmt.Sprintf("GitHub installation %d", id))
}

// ListGitHubInstallations returns every installation, by account login.
func (q Q) ListGitHubInstallations(ctx context.Context) ([]GitHubInstallationRow, error) {
	rows, err := q.db.Query(ctx, `select `+installationCols+` from github_installations i order by i.account_login, i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitHubInstallationRow
	for rows.Next() {
		i, err := scanInstallation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// GitHubBackedRepos returns the GitHub-backed repositories of the given
// installations, by name; org limits them to one organization.
func (q Q) GitHubBackedRepos(ctx context.Context, installations []int64, org *uuid.UUID) ([]Repo, error) {
	rows, err := q.db.Query(ctx, repoSelect+` where r.github_repo_id is not null and r.github_installation_id = any($1)
		and ($2::uuid is null or r.org_id = $2) order by r.name`, installations, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GitHubAccessFacts is RepoAccessFacts for a GitHub-backed repository:
// the roles GitHub reported for people checked since since (via "GitHub"),
// and direct grants of service principals, by handle. Support grants are
// listed by ListSupportGrants.
func (q Q) GitHubAccessFacts(ctx context.Context, repo uuid.UUID, since time.Time) ([]AccessFact, error) {
	cols := prefixed("p", principalCols)
	rows, err := q.db.Query(ctx, `select * from (
		select `+cols+`, c.role as role, 'GitHub' as via from github_role_cache c join principals p on p.id = c.principal_id
		where c.repo_id = $1 and c.role <> '' and c.checked_at >= $2
		union all
		select `+cols+`, g.role, 'service:' || p.handle from repo_grants g join principals p on p.id = g.person_id
		where g.repo_id = $1 and p.kind = 'service'
		) x order by handle, via`, repo, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessFact
	for rows.Next() {
		var f AccessFact
		p := &f.Principal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Handle, &p.ParentID, &p.DisplayName, &p.Email, &p.SiteAdmin, &p.CreatedAt, &f.Role, &f.Via); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpsertGitHubInstallation records an installation of the App or updates
// it (account, linked organization, selection, suspension).
func (q Q) UpsertGitHubInstallation(ctx context.Context, in GitHubInstallationRow) error {
	if in.RepositorySelection == "" {
		in.RepositorySelection = "selected"
	}
	_, err := q.db.Exec(ctx, `insert into github_installations (id, account_id, account_login, account_type, org_id, repository_selection, suspended_at)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (id) do update set account_id = excluded.account_id, account_login = excluded.account_login,
			account_type = excluded.account_type, org_id = excluded.org_id,
			repository_selection = excluded.repository_selection, suspended_at = excluded.suspended_at, updated_at = now()`,
		in.ID, in.AccountID, in.AccountLogin, in.AccountType, in.OrgID, in.RepositorySelection, in.SuspendedAt)
	return err
}

// DeleteGitHubInstallation forgets an installation (removed from GitHub).
// Its repositories stay linked to it, so they stay GitHub-backed and nobody
// but service principals and support grants has a role on them until they
// are linked again.
func (q Q) DeleteGitHubInstallation(ctx context.Context, id int64) error {
	_, err := q.db.Exec(ctx, `delete from github_installations where id = $1`, id)
	return err
}
