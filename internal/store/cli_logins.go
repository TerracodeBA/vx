package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CLILogin is a browser login of the CLI (table cli_logins). The device
// code is known only by its sha256.
type CLILogin struct {
	DeviceHash   []byte
	UserCode     string
	ClientName   string
	Status       string // pending, approved, denied, used
	PrincipalID  *uuid.UUID
	ClientIP     string
	ExpiresAt    time.Time
	CreatedAt    time.Time
	LastPolledAt *time.Time
	DecidedAt    *time.Time
	// HasSecret is true when token_secret_enc is set; vx never sets it (the
	// token is created when the CLI collects it).
	HasSecret bool
}

const cliLoginCols = `device_hash, user_code, client_name, status, principal_id, client_ip, expires_at, created_at, last_polled_at, decided_at, token_secret_enc is not null`

func scanCLILogin(row interface{ Scan(...any) error }) (CLILogin, error) {
	var l CLILogin
	err := row.Scan(&l.DeviceHash, &l.UserCode, &l.ClientName, &l.Status, &l.PrincipalID, &l.ClientIP, &l.ExpiresAt, &l.CreatedAt, &l.LastPolledAt, &l.DecidedAt, &l.HasSecret)
	return l, err
}

// CreateCLILogin stores a new pending login. ErrExists means the user code
// is taken; the caller picks another.
func (q Q) CreateCLILogin(ctx context.Context, l CLILogin) error {
	_, err := q.db.Exec(ctx, `insert into cli_logins (device_hash, user_code, client_name, client_ip, expires_at)
		values ($1, $2, $3, $4, $5)`, l.DeviceHash, l.UserCode, l.ClientName, l.ClientIP, l.ExpiresAt)
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

// CountCLILoginsSince counts logins started from ip since t.
func (q Q) CountCLILoginsSince(ctx context.Context, ip string, since time.Time) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `select count(*) from cli_logins where client_ip = $1 and created_at >= $2`, ip, since).Scan(&n)
	return n, err
}

// CountPendingCLILogins counts logins nobody decided yet that have not
// expired, across all addresses.
func (q Q) CountPendingCLILogins(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `select count(*) from cli_logins where status = 'pending' and expires_at > $1`, now).Scan(&n)
	return n, err
}

// DeleteCLILoginsBefore removes logins that expired before t.
func (q Q) DeleteCLILoginsBefore(ctx context.Context, t time.Time) error {
	_, err := q.db.Exec(ctx, `delete from cli_logins where expires_at < $1`, t)
	return err
}

// CLILoginByDevice returns a login by device hash, locked for update when
// run in a transaction.
func (q Q) CLILoginByDevice(ctx context.Context, deviceHash []byte) (CLILogin, error) {
	l, err := scanCLILogin(q.db.QueryRow(ctx, `select `+cliLoginCols+` from cli_logins where device_hash = $1 for update`, deviceHash))
	return l, notFound(err, "cli login")
}

// CLILoginByUserCode returns a login by user code, locked for update when
// run in a transaction.
func (q Q) CLILoginByUserCode(ctx context.Context, userCode string) (CLILogin, error) {
	l, err := scanCLILogin(q.db.QueryRow(ctx, `select `+cliLoginCols+` from cli_logins where user_code = $1 for update`, userCode))
	return l, notFound(err, "cli login")
}

// TouchCLILoginPoll records a poll.
func (q Q) TouchCLILoginPoll(ctx context.Context, deviceHash []byte, at time.Time) error {
	_, err := q.db.Exec(ctx, `update cli_logins set last_polled_at = $2 where device_hash = $1`, deviceHash, at)
	return err
}

// SetCLILoginStatus moves a login from one status to another and reports
// whether it was in the from status. principalID is recorded when set.
func (q Q) SetCLILoginStatus(ctx context.Context, deviceHash []byte, from, to string, principalID *uuid.UUID, at time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `update cli_logins set status = $3, principal_id = coalesce($4, principal_id),
		decided_at = case when $3 in ('approved', 'denied') then $5 else decided_at end
		where device_hash = $1 and status = $2`, deviceHash, from, to, principalID, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
