package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreateLoginCode stores a one-time web sign-in code by its hash and drops
// expired ones.
func (q Q) CreateLoginCode(ctx context.Context, codeHash []byte, principal uuid.UUID, expires, now time.Time) error {
	if _, err := q.db.Exec(ctx, `delete from login_codes where expires_at <= $1`, now); err != nil {
		return err
	}
	_, err := q.db.Exec(ctx, `insert into login_codes (code_hash, principal_id, expires_at) values ($1, $2, $3)`,
		codeHash, principal, expires)
	return err
}

// PeekLoginCode returns who an unexpired code signs in, or ErrNotFound.
func (q Q) PeekLoginCode(ctx context.Context, codeHash []byte, now time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.db.QueryRow(ctx, `select principal_id from login_codes where code_hash = $1 and expires_at > $2`,
		codeHash, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}

// UseLoginCode deletes a code and returns who it signs in. Of two servers
// redeeming one code at once, only one gets it; an expired or unknown code
// is ErrNotFound.
func (q Q) UseLoginCode(ctx context.Context, codeHash []byte, now time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	var expires time.Time
	err := q.db.QueryRow(ctx, `delete from login_codes where code_hash = $1 returning principal_id, expires_at`,
		codeHash).Scan(&id, &expires)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !expires.After(now)) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}
