package store

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Provider is where a repo's upstream lives.
type Provider string

const (
	ProviderGitHub Provider = "github" // remote is "owner/name"
	ProviderGit    Provider = "git"    // remote is a git URL or path
	ProviderNone   Provider = "none"   // vx only, nothing to sync or ship to
	// Hosts with a pull request API besides GitHub; remote is the path on
	// the host ("group/project", "owner/repo", "workspace/repo") and
	// BaseURL the host's web address.
	ProviderGitLab    Provider = "gitlab"
	ProviderGitea     Provider = "gitea"
	ProviderForgejo   Provider = "forgejo"
	ProviderBitbucket Provider = "bitbucket"
)

// Hosted reports whether p is a host with a pull request API other than
// GitHub: GitLab, Gitea, Forgejo or Bitbucket Cloud.
func (p Provider) Hosted() bool {
	switch p {
	case ProviderGitLab, ProviderGitea, ProviderForgejo, ProviderBitbucket:
		return true
	}
	return false
}

// HostName is the provider as people call it: "GitHub", "GitLab", ...
func (p Provider) HostName() string {
	switch p {
	case ProviderGitHub:
		return "GitHub"
	case ProviderGitLab:
		return "GitLab"
	case ProviderGitea:
		return "Gitea"
	case ProviderForgejo:
		return "Forgejo"
	case ProviderBitbucket:
		return "Bitbucket"
	case ProviderGit:
		return "git"
	}
	return "vx"
}

// PRName is what the host calls a pull request.
func (p Provider) PRName() string {
	if p == ProviderGitLab {
		return "merge request"
	}
	return "pull request"
}

// Repo is a repository known to vx and its upstream.
type Repo struct {
	ID            uuid.UUID
	Name          string
	Provider      Provider
	Remote        string
	DefaultBranch string
	// BaseURL is the web address of a GitLab, Gitea, Forgejo or Bitbucket
	// host, e.g. https://gitlab.example.com; empty for other providers.
	BaseURL   string
	OrgID     uuid.UUID // the owning organization
	Org       string    // its handle
	CreatedAt time.Time
	// GitHubRepoID is set for a repository linked to GitHub, and
	// GitHubInstallationID while an installation of the App reaches it.
	// Such a repository is GitHub-backed: roles on it come from GitHub,
	// never from vx grants. Without an installation nobody but service
	// principals and support grants has a role on it (it stays GitHub-backed, so access fails closed).
	GitHubRepoID         *int64
	GitHubInstallationID *int64
}

// HostName is the repository's git host as people call it.
func (r Repo) HostName() string { return r.Provider.HostName() }

// PRName is what the repository's host calls a pull request.
func (r Repo) PRName() string { return r.Provider.PRName() }

// WebURL is the page of a GitLab, Gitea, Forgejo or Bitbucket repository
// on its host, or "" for other providers (a GitHub repository may be on
// GitHub Enterprise, which only the service knows).
func (r Repo) WebURL() string {
	if r.Provider.Hosted() && r.BaseURL != "" {
		return r.BaseURL + "/" + r.Remote
	}
	return ""
}

// GitHubBacked reports whether roles on r come from GitHub: it is linked
// to a GitHub repository (with or without an installation reaching it now).
func (r Repo) GitHubBacked() bool { return r.GitHubRepoID != nil }

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}(/[A-Za-z0-9][A-Za-z0-9._-]{0,99})?$`)

// ValidateRepoName checks names like "shop-api" or "acme/shop-api".
func ValidateRepoName(name string) error {
	if !repoName.MatchString(name) {
		return fmt.Errorf("repo name %q: use letters, digits, dot, dash or underscore, optionally owner/name", name)
	}
	return nil
}

// DefaultOrg is the organization repositories without an owner part were
// put in when organizations arrived, and where CreateRepo puts a repo
// whose OrgID is not set.
const DefaultOrg = "default"

// CreateRepo inserts a repo and its upstream. The repo belongs to r.OrgID;
// without one it goes to the "default" organization, created on demand
// (the service always sets OrgID; this keeps low-level callers working).
func (q Q) CreateRepo(ctx context.Context, r Repo) (Repo, error) {
	if err := ValidateRepoName(r.Name); err != nil {
		return Repo{}, err
	}
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	if r.Provider == "" {
		r.Provider = ProviderNone
	}
	if r.OrgID == uuid.Nil {
		if err := q.db.QueryRow(ctx, `with ins as (
				insert into orgs (handle, display_name) values ($1, $1) on conflict (handle) do nothing returning id
			) select id from ins union all select id from orgs where handle = $1 limit 1`, DefaultOrg).Scan(&r.OrgID); err != nil {
			return Repo{}, err
		}
	}
	err := q.db.QueryRow(ctx, `insert into repos (name, org_id) values ($1, $2)
		returning id, created_at, (select handle from orgs where id = $2)`, r.Name, r.OrgID).Scan(&r.ID, &r.CreatedAt, &r.Org)
	if isUniqueViolation(err) {
		return Repo{}, fmt.Errorf("repo %q: %w", r.Name, ErrExists)
	}
	if err != nil {
		return Repo{}, err
	}
	if r.BaseURL == "" {
		// Without base_url, so data-migration tests can write a schema from
		// before migration 022 with this.
		_, err = q.db.Exec(ctx, `insert into upstreams (repo_id, provider, remote, default_branch) values ($1, $2, $3, $4)`,
			r.ID, r.Provider, r.Remote, r.DefaultBranch)
		return r, err
	}
	_, err = q.db.Exec(ctx, `insert into upstreams (repo_id, provider, remote, default_branch, base_url) values ($1, $2, $3, $4, $5)`,
		r.ID, r.Provider, r.Remote, r.DefaultBranch, r.BaseURL)
	return r, err
}

const repoCols = `r.id, r.name, u.provider, u.remote, u.default_branch, u.base_url, r.org_id, o.handle, r.created_at, r.github_repo_id, r.github_installation_id`

// repoFrom leaves out the repositories of deleted organizations (#40):
// from the moment an owner deletes an organization its repositories read
// like ones that never existed, to every caller of RepoByName, RepoByID,
// the role facts and the lists. The purge reads them by organization id.
const repoFrom = ` from repos r join upstreams u on u.repo_id = r.id join orgs o on o.id = r.org_id and o.deleted_at is null`

const repoSelect = `select ` + repoCols + repoFrom

func scanRepo(row interface{ Scan(...any) error }, extra ...any) (Repo, error) {
	var r Repo
	err := row.Scan(append([]any{&r.ID, &r.Name, &r.Provider, &r.Remote, &r.DefaultBranch, &r.BaseURL, &r.OrgID, &r.Org, &r.CreatedAt, &r.GitHubRepoID, &r.GitHubInstallationID}, extra...)...)
	return r, err
}

// RepoByName looks a repo up by name.
func (q Q) RepoByName(ctx context.Context, name string) (Repo, error) {
	r, err := scanRepo(q.db.QueryRow(ctx, repoSelect+` where r.name = $1`, name))
	return r, notFound(err, "repo "+name)
}

// RepoByID looks a repo up by id.
func (q Q) RepoByID(ctx context.Context, id uuid.UUID) (Repo, error) {
	r, err := scanRepo(q.db.QueryRow(ctx, repoSelect+` where r.id = $1`, id))
	return r, notFound(err, "repo")
}

// ListRepos returns all repos by name.
func (q Q) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := q.db.Query(ctx, repoSelect+` order by r.name`)
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

// Branch is an upstream branch that vx follows.
type Branch struct {
	RepoID   uuid.UUID
	Name     string
	Head     hash.Hash // changeset
	GitSHA   string
	SyncedAt time.Time
}

// UpsertBranch records the current head of an upstream branch.
func (q Q) UpsertBranch(ctx context.Context, b Branch) error {
	_, err := q.db.Exec(ctx, `insert into tracked_branches (repo_id, name, head, git_sha, synced_at)
		values ($1, $2, $3, $4, now())
		on conflict (repo_id, name) do update set head = excluded.head, git_sha = excluded.git_sha, synced_at = now()`,
		b.RepoID, b.Name, b.Head, b.GitSHA)
	return err
}

// GetBranch returns a tracked branch.
func (q Q) GetBranch(ctx context.Context, repoID uuid.UUID, name string) (Branch, error) {
	var b Branch
	err := q.db.QueryRow(ctx, `select repo_id, name, head, git_sha, synced_at from tracked_branches
		where repo_id = $1 and name = $2`, repoID, name).Scan(&b.RepoID, &b.Name, &b.Head, &b.GitSHA, &b.SyncedAt)
	return b, notFound(err, "branch "+name)
}

// ListBranches returns the tracked branches of a repo.
func (q Q) ListBranches(ctx context.Context, repoID uuid.UUID) ([]Branch, error) {
	rows, err := q.db.Query(ctx, `select repo_id, name, head, git_sha, synced_at from tracked_branches
		where repo_id = $1 order by name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Branch
	for rows.Next() {
		var b Branch
		if err := rows.Scan(&b.RepoID, &b.Name, &b.Head, &b.GitSHA, &b.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
