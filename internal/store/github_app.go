package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// GitHubApp is the row of this server's GitHub App. The _Enc fields hold
// ciphertext sealed with the server key (internal/secret), never plaintext.
type GitHubApp struct {
	AppID            int64
	Slug             string
	HTMLURL          string
	ClientID         string
	ClientSecretEnc  []byte
	PrivateKeyEnc    []byte
	WebhookSecretEnc []byte
	CreatedBy        *uuid.UUID
	CreatedAt        time.Time
}

// GitHubApp returns the server's GitHub App, or ErrNotFound.
func (q Q) GitHubApp(ctx context.Context) (GitHubApp, error) {
	var a GitHubApp
	err := q.db.QueryRow(ctx, `select app_id, slug, html_url, client_id, client_secret_enc, private_key_enc, webhook_secret_enc, created_by, created_at
		from github_app where id = 1`).Scan(&a.AppID, &a.Slug, &a.HTMLURL, &a.ClientID, &a.ClientSecretEnc, &a.PrivateKeyEnc, &a.WebhookSecretEnc, &a.CreatedBy, &a.CreatedAt)
	return a, notFound(err, "GitHub App")
}

// PutGitHubApp stores the server's GitHub App, replacing an earlier one.
func (q Q) PutGitHubApp(ctx context.Context, a GitHubApp) error {
	_, err := q.db.Exec(ctx, `insert into github_app (id, app_id, slug, html_url, client_id, client_secret_enc, private_key_enc, webhook_secret_enc, created_by)
		values (1, $1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (id) do update set app_id = excluded.app_id, slug = excluded.slug, html_url = excluded.html_url,
		  client_id = excluded.client_id, client_secret_enc = excluded.client_secret_enc, private_key_enc = excluded.private_key_enc,
		  webhook_secret_enc = excluded.webhook_secret_enc, created_by = excluded.created_by, created_at = now()`,
		a.AppID, a.Slug, a.HTMLURL, a.ClientID, a.ClientSecretEnc, a.PrivateKeyEnc, a.WebhookSecretEnc, a.CreatedBy)
	return err
}

// RecordGitHubDelivery records a webhook delivery id. fresh is false when
// the delivery was recorded before (a replay or redelivery). Deliveries
// older than keep are pruned on the way.
func (q Q) RecordGitHubDelivery(ctx context.Context, id, event string, keep time.Duration) (fresh bool, err error) {
	if _, err := q.db.Exec(ctx, `delete from github_deliveries where received_at < now() - make_interval(secs => $1)`, keep.Seconds()); err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `insert into github_deliveries (id, event) values ($1, $2) on conflict (id) do nothing`, id, event)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetGitHubDelivery removes a recorded delivery, so GitHub's
// redelivery of a delivery that failed to apply is applied.
func (q Q) ForgetGitHubDelivery(ctx context.Context, id string) error {
	_, err := q.db.Exec(ctx, `delete from github_deliveries where id = $1`, id)
	return err
}

// RepoByGitHubID looks a repo up by its GitHub repository id.
func (q Q) RepoByGitHubID(ctx context.Context, githubRepoID int64) (Repo, error) {
	r, err := scanRepo(q.db.QueryRow(ctx, repoSelect+` where r.github_repo_id = $1`, githubRepoID))
	return r, notFound(err, fmt.Sprintf("repo with GitHub id %d", githubRepoID))
}

// ReposOfGitHubInstallation lists the repos linked to an installation.
func (q Q) ReposOfGitHubInstallation(ctx context.Context, installationID int64) ([]Repo, error) {
	rows, err := q.db.Query(ctx, repoSelect+` where r.github_installation_id = $1 order by r.name`, installationID)
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

// SetRepoUpstream changes a repo's remote and default branch, for example
// after the repository was renamed or transferred on GitHub.
func (q Q) SetRepoUpstream(ctx context.Context, repoID uuid.UUID, remote, defaultBranch string) error {
	tag, err := q.db.Exec(ctx, `update upstreams set remote = $2, default_branch = $3 where repo_id = $1`, repoID, remote, defaultBranch)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("repo %s: %w", repoID, ErrNotFound)
	}
	return err
}

// CreateOrgWithoutOwner inserts an organization nobody owns yet: one that
// mirrors a GitHub organization, whose members come from GitHub.
func (q Q) CreateOrgWithoutOwner(ctx context.Context, handle, displayName string) (Org, error) {
	if err := ValidateHandle(KindHuman, handle); err != nil {
		return Org{}, fmt.Errorf("organization %s", err)
	}
	if displayName == "" {
		displayName = handle
	}
	o, err := scanOrg(q.db.QueryRow(ctx, `insert into orgs as o (handle, display_name, default_repo_role) values ($1, $2, 'none') returning `+orgCols, handle, displayName))
	if isUniqueViolation(err) {
		return Org{}, fmt.Errorf("organization %q: %w", handle, ErrExists)
	}
	return o, err
}
