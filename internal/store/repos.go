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
)

// Repo is a repository known to vx and its upstream.
type Repo struct {
	ID            uuid.UUID
	Name          string
	Provider      Provider
	Remote        string
	DefaultBranch string
	CreatedAt     time.Time
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}(/[A-Za-z0-9][A-Za-z0-9._-]{0,99})?$`)

// ValidateRepoName checks names like "shop-api" or "acme/shop-api".
func ValidateRepoName(name string) error {
	if !repoName.MatchString(name) {
		return fmt.Errorf("repo name %q: use letters, digits, dot, dash or underscore, optionally owner/name", name)
	}
	return nil
}

// CreateRepo inserts a repo and its upstream.
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
	err := q.db.QueryRow(ctx, `insert into repos (name) values ($1) returning id, created_at`, r.Name).Scan(&r.ID, &r.CreatedAt)
	if isUniqueViolation(err) {
		return Repo{}, fmt.Errorf("repo %q: %w", r.Name, ErrExists)
	}
	if err != nil {
		return Repo{}, err
	}
	_, err = q.db.Exec(ctx, `insert into upstreams (repo_id, provider, remote, default_branch) values ($1, $2, $3, $4)`,
		r.ID, r.Provider, r.Remote, r.DefaultBranch)
	return r, err
}

const repoSelect = `select r.id, r.name, u.provider, u.remote, u.default_branch, r.created_at
	from repos r join upstreams u on u.repo_id = r.id`

func scanRepo(row interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	err := row.Scan(&r.ID, &r.Name, &r.Provider, &r.Remote, &r.DefaultBranch, &r.CreatedAt)
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
