package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Reservation is a value of a vx.toml [[unique]] rule handed out by vx
// reserve (rule_reservations). EffortSlug and ReservedBy are read from the
// effort and principal; empty when those were deleted.
type Reservation struct {
	RepoID     uuid.UUID
	Rule       string
	Value      string
	EffortID   *uuid.UUID
	EffortSlug string
	EffortOpen bool // the effort exists and is open
	ReservedBy string
	CreatedAt  time.Time
}

// InsertReservation records value for rule. It reports false, and no
// error, when that value is already reserved (another call won).
func (q Q) InsertReservation(ctx context.Context, repoID uuid.UUID, rule, value string, effortID, by *uuid.UUID) (bool, error) {
	_, err := q.db.Exec(ctx, `insert into rule_reservations (repo_id, rule, value, effort_id, reserved_by)
		values ($1, $2, $3, $4, $5)`, repoID, rule, value, effortID, by)
	if isUniqueViolation(err) {
		return false, nil
	}
	return err == nil, err
}

// ListReservations lists a repository's reservations, newest first; an
// empty rule lists every rule's.
func (q Q) ListReservations(ctx context.Context, repoID uuid.UUID, rule string) ([]Reservation, error) {
	rows, err := q.db.Query(ctx, `select r.repo_id, r.rule, r.value, r.effort_id, coalesce(e.slug, ''),
			coalesce(e.state = 'open', false), coalesce((case p.kind when 'human' then 'user' else p.kind end) || ':' || p.handle, ''), r.created_at
		from rule_reservations r
		left join efforts e on e.id = r.effort_id
		left join principals p on p.id = r.reserved_by
		where r.repo_id = $1 and ($2 = '' or r.rule = $2)
		order by r.created_at desc, r.rule, r.value desc`, repoID, rule)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.RepoID, &r.Rule, &r.Value, &r.EffortID, &r.EffortSlug, &r.EffortOpen, &r.ReservedBy, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
