package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PushKey is the server's VAPID key (push_keys).
type PushKey struct {
	PrivateKeyEnc []byte // sealed 32-byte P-256 scalar
	PublicKey     string // base64url uncompressed point
	CreatedAt     time.Time
}

// PushKey returns the server's VAPID key, or ErrNotFound before the first
// one is made.
func (q Q) PushKey(ctx context.Context) (PushKey, error) {
	var k PushKey
	err := q.db.QueryRow(ctx, `select private_key_enc, public_key, created_at from push_keys where id = 1`).
		Scan(&k.PrivateKeyEnc, &k.PublicKey, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PushKey{}, ErrNotFound
	}
	return k, err
}

// InitPushKey stores k unless a key exists, and returns the stored one:
// two servers making a key at once agree on the first.
func (q Q) InitPushKey(ctx context.Context, k PushKey) (PushKey, error) {
	if _, err := q.db.Exec(ctx, `insert into push_keys (id, private_key_enc, public_key) values (1, $1, $2)
		on conflict (id) do nothing`, k.PrivateKeyEnc, k.PublicKey); err != nil {
		return PushKey{}, err
	}
	return q.PushKey(ctx)
}

// PushSubscription is a browser that receives a person's notifications.
type PushSubscription struct {
	ID          uuid.UUID
	PrincipalID uuid.UUID
	Endpoint    string
	P256dh      string
	Auth        string
	Device      string
	CreatedAt   time.Time
	LastSentAt  *time.Time
	LastError   string
}

const pushSubCols = `id, principal_id, endpoint, p256dh, auth, device, created_at, last_sent_at, last_error`

func scanPushSubs(rows pgx.Rows) ([]PushSubscription, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PushSubscription, error) {
		var s PushSubscription
		err := r.Scan(&s.ID, &s.PrincipalID, &s.Endpoint, &s.P256dh, &s.Auth, &s.Device, &s.CreatedAt, &s.LastSentAt, &s.LastError)
		return s, err
	})
}

// SavePushSubscription adds a subscription, or updates the one with the
// same endpoint: a browser subscribes once, so the endpoint moves to
// whoever signed in on it last.
func (q Q) SavePushSubscription(ctx context.Context, s PushSubscription) (PushSubscription, error) {
	rows, err := q.db.Query(ctx, `insert into push_subscriptions (principal_id, endpoint, p256dh, auth, device, created_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (endpoint) do update set principal_id = excluded.principal_id, p256dh = excluded.p256dh,
			auth = excluded.auth, device = excluded.device, last_error = '',
			created_at = case when push_subscriptions.principal_id = excluded.principal_id then push_subscriptions.created_at else excluded.created_at end
		returning `+pushSubCols, s.PrincipalID, s.Endpoint, s.P256dh, s.Auth, s.Device, s.CreatedAt)
	if err != nil {
		return PushSubscription{}, err
	}
	out, err := scanPushSubs(rows)
	if err != nil || len(out) == 0 {
		return PushSubscription{}, err
	}
	return out[0], nil
}

// DeletePushSubscription removes a principal's subscription by endpoint;
// it reports whether one was there.
func (q Q) DeletePushSubscription(ctx context.Context, principalID uuid.UUID, endpoint string) (bool, error) {
	tag, err := q.db.Exec(ctx, `delete from push_subscriptions where principal_id = $1 and endpoint = $2`, principalID, endpoint)
	return tag.RowsAffected() > 0, err
}

// DeletePushSubscriptionByID removes a subscription the push service
// reported gone.
func (q Q) DeletePushSubscriptionByID(ctx context.Context, id uuid.UUID) error {
	_, err := q.db.Exec(ctx, `delete from push_subscriptions where id = $1`, id)
	return err
}

// PushSubscriptions lists a principal's subscriptions, oldest first.
func (q Q) PushSubscriptions(ctx context.Context, principalID uuid.UUID) ([]PushSubscription, error) {
	rows, err := q.db.Query(ctx, `select `+pushSubCols+` from push_subscriptions where principal_id = $1 order by created_at, id`, principalID)
	if err != nil {
		return nil, err
	}
	return scanPushSubs(rows)
}

// PushRecipients lists the principals with at least one subscription.
func (q Q) PushRecipients(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `select distinct principal_id from push_subscriptions order by principal_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// MarkPushResult records the outcome of a send: when it went out, or why
// it did not.
func (q Q) MarkPushResult(ctx context.Context, id uuid.UUID, at time.Time, failure string) error {
	if failure != "" {
		_, err := q.db.Exec(ctx, `update push_subscriptions set last_error = $2 where id = $1`, id, failure)
		return err
	}
	_, err := q.db.Exec(ctx, `update push_subscriptions set last_sent_at = $2, last_error = '' where id = $1`, id, at)
	return err
}

// ClaimPush records that item is pushed to a principal and reports whether
// this caller is the first: only the first sends.
func (q Q) ClaimPush(ctx context.Context, principalID uuid.UUID, item string, at time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `insert into push_sent (principal_id, item, sent_at) values ($1, $2, $3)
		on conflict do nothing`, principalID, item, at)
	return tag.RowsAffected() == 1, err
}

// PrunePushSent forgets pushes older than before: the items they were
// about are long decided or answered.
func (q Q) PrunePushSent(ctx context.Context, before time.Time) error {
	_, err := q.db.Exec(ctx, `delete from push_sent where sent_at < $1`, before)
	return err
}
