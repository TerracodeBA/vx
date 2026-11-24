package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// UpstreamSecrets are a repository's sealed host secrets (internal/secret):
// nil fields mean "use the server's".
type UpstreamSecrets struct {
	TokenEnc         []byte
	WebhookSecretEnc []byte
}

// GetUpstreamSecrets returns the sealed token and webhook secret of a
// repository.
func (q Q) GetUpstreamSecrets(ctx context.Context, repoID uuid.UUID) (UpstreamSecrets, error) {
	var s UpstreamSecrets
	err := q.db.QueryRow(ctx, `select token_enc, webhook_secret_enc from upstreams where repo_id = $1`, repoID).Scan(&s.TokenEnc, &s.WebhookSecretEnc)
	return s, notFound(err, "repo")
}

// SetUpstreamToken stores a sealed token for a repository; nil clears it.
func (q Q) SetUpstreamToken(ctx context.Context, repoID uuid.UUID, sealed []byte) error {
	return q.setUpstreamSecret(ctx, repoID, "token_enc", sealed)
}

// SetUpstreamWebhookSecret stores a sealed webhook secret; nil clears it.
func (q Q) SetUpstreamWebhookSecret(ctx context.Context, repoID uuid.UUID, sealed []byte) error {
	return q.setUpstreamSecret(ctx, repoID, "webhook_secret_enc", sealed)
}

func (q Q) setUpstreamSecret(ctx context.Context, repoID uuid.UUID, col string, sealed []byte) error {
	tag, err := q.db.Exec(ctx, `update upstreams set `+col+` = $2 where repo_id = $1`, repoID, sealed)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("repo %s: %w", repoID, ErrNotFound)
	}
	return err
}

// ReposByRemote lists the repositories following remote (compared without
// case) on one of providers, by name. Several vx repositories may follow
// the same upstream.
func (q Q) ReposByRemote(ctx context.Context, remote string, providers ...Provider) ([]Repo, error) {
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = string(p)
	}
	rows, err := q.db.Query(ctx, repoSelect+` where u.provider = any($1) and lower(u.remote) = lower($2) order by r.name`, names, remote)
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
