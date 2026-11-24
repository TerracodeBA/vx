package store

import (
	"context"
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
