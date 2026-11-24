package store

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EffortOverlap is a stored forecast of what happens when two open efforts
// of one repository both ship. EffortA < EffortB (byte order of the ids).
type EffortOverlap struct {
	RepoID      uuid.UUID
	EffortA     uuid.UUID
	EffortB     uuid.UUID
	Status      string // clean, overlap, conflict
	SharedFiles []string
	Conflicts   []string
	Warnings    []Warning
	AHead       string // changeset hashes the forecast was computed for
	BHead       string
	TargetHead  string // the base branch head both were replayed onto
	ANote       *uuid.UUID
	BNote       *uuid.UUID
	CheckedAt   time.Time
}

// OrderPair returns a and b in the order effort_overlaps stores them.
func OrderPair(a, b uuid.UUID) (uuid.UUID, uuid.UUID, bool) {
	if bytes.Compare(a[:], b[:]) <= 0 {
		return a, b, false
	}
	return b, a, true
}

const overlapCols = `o.repo_id, o.effort_a, o.effort_b, o.status, o.shared_files, o.conflicts, o.warnings,
	o.a_head, o.b_head, o.target_head, o.a_note, o.b_note, o.checked_at`

func scanOverlap(row interface{ Scan(...any) error }) (EffortOverlap, error) {
	var o EffortOverlap
	var warnings []byte
	err := row.Scan(&o.RepoID, &o.EffortA, &o.EffortB, &o.Status, &o.SharedFiles, &o.Conflicts, &warnings,
		&o.AHead, &o.BHead, &o.TargetHead, &o.ANote, &o.BNote, &o.CheckedAt)
	if err != nil {
		return o, err
	}
	if len(warnings) > 0 {
		if err := json.Unmarshal(warnings, &o.Warnings); err != nil {
			return o, err
		}
	}
	if o.SharedFiles == nil {
		o.SharedFiles = []string{}
	}
	if o.Conflicts == nil {
		o.Conflicts = []string{}
	}
	return o, nil
}

func (q Q) queryOverlaps(ctx context.Context, sql string, args ...any) ([]EffortOverlap, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EffortOverlap
	for rows.Next() {
		o, err := scanOverlap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetOverlap loads the forecast of a pair, in either order.
func (q Q) GetOverlap(ctx context.Context, a, b uuid.UUID) (EffortOverlap, error) {
	a, b, _ = OrderPair(a, b)
	o, err := scanOverlap(q.db.QueryRow(ctx, `select `+overlapCols+` from effort_overlaps o where o.effort_a = $1 and o.effort_b = $2`, a, b))
	return o, notFound(err, "overlap")
}

// LockOverlapPair serializes writers of one pair's forecast and notes until
// the transaction ends, also across servers.
func (q Q) LockOverlapPair(ctx context.Context, a, b uuid.UUID) error {
	a, b, _ = OrderPair(a, b)
	_, err := q.db.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('vx_overlap:' || $1::text || ':' || $2::text, 0))`, a, b)
	return err
}

// SaveOverlap inserts or replaces a pair's forecast. EffortA and EffortB
// must be ordered (OrderPair).
func (q Q) SaveOverlap(ctx context.Context, o EffortOverlap) error {
	warnings := []byte("[]")
	if len(o.Warnings) > 0 {
		var err error
		if warnings, err = json.Marshal(o.Warnings); err != nil {
			return err
		}
	}
	if o.SharedFiles == nil {
		o.SharedFiles = []string{}
	}
	if o.Conflicts == nil {
		o.Conflicts = []string{}
	}
	_, err := q.db.Exec(ctx, `insert into effort_overlaps (repo_id, effort_a, effort_b, status, shared_files, conflicts, warnings,
			a_head, b_head, target_head, a_note, b_note, checked_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, coalesce($13, now()))
		on conflict (effort_a, effort_b) do update set status = excluded.status, shared_files = excluded.shared_files,
			conflicts = excluded.conflicts, warnings = excluded.warnings, a_head = excluded.a_head, b_head = excluded.b_head,
			target_head = excluded.target_head, a_note = excluded.a_note, b_note = excluded.b_note, checked_at = excluded.checked_at`,
		o.RepoID, o.EffortA, o.EffortB, o.Status, o.SharedFiles, o.Conflicts, warnings,
		o.AHead, o.BHead, o.TargetHead, o.ANote, o.BNote, nullTime(o.CheckedAt))
	return err
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ListEffortOverlaps returns the stored forecasts of an effort with every
// other effort that is still open, oldest pair first.
func (q Q) ListEffortOverlaps(ctx context.Context, effortID uuid.UUID) ([]EffortOverlap, error) {
	return q.queryOverlaps(ctx, `select `+overlapCols+` from effort_overlaps o
		join efforts ea on ea.id = o.effort_a join efforts eb on eb.id = o.effort_b
		where (o.effort_a = $1 or o.effort_b = $1) and ea.state = 'open' and eb.state = 'open'
		order by o.checked_at, o.effort_a, o.effort_b`, effortID)
}

// ListRepoOverlaps returns the stored forecasts of every pair of open
// efforts of a repository.
func (q Q) ListRepoOverlaps(ctx context.Context, repoID uuid.UUID) ([]EffortOverlap, error) {
	return q.queryOverlaps(ctx, `select `+overlapCols+` from effort_overlaps o
		join efforts ea on ea.id = o.effort_a join efforts eb on eb.id = o.effort_b
		where o.repo_id = $1 and ea.state = 'open' and eb.state = 'open'
		order by o.checked_at, o.effort_a, o.effort_b`, repoID)
}

// OverlapCounts returns, for each open effort of a repository, how many
// other open efforts it overlaps or conflicts with (clean pairs are not
// counted), and how many of those conflict.
func (q Q) OverlapCounts(ctx context.Context, repoID uuid.UUID) (map[uuid.UUID][2]int, error) {
	rows, err := q.db.Query(ctx, `select e, count(*), count(*) filter (where status = 'conflict') from (
			select o.effort_a as e, o.status from effort_overlaps o
				join efforts ea on ea.id = o.effort_a join efforts eb on eb.id = o.effort_b
				where o.repo_id = $1 and o.status <> 'clean' and ea.state = 'open' and eb.state = 'open'
			union all
			select o.effort_b, o.status from effort_overlaps o
				join efforts ea on ea.id = o.effort_a join efforts eb on eb.id = o.effort_b
				where o.repo_id = $1 and o.status <> 'clean' and ea.state = 'open' and eb.state = 'open'
		) x group by e`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][2]int{}
	for rows.Next() {
		var id uuid.UUID
		var n, c int
		if err := rows.Scan(&id, &n, &c); err != nil {
			return nil, err
		}
		out[id] = [2]int{n, c}
	}
	return out, rows.Err()
}

// DeleteClosedOverlaps removes the forecasts of an effort with efforts that
// are no longer open (or of the effort itself when it closed).
func (q Q) DeleteClosedOverlaps(ctx context.Context, effortID uuid.UUID) error {
	_, err := q.db.Exec(ctx, `delete from effort_overlaps o using efforts ea, efforts eb
		where ea.id = o.effort_a and eb.id = o.effort_b and (o.effort_a = $1 or o.effort_b = $1)
		and (ea.state <> 'open' or eb.state <> 'open')`, effortID)
	return err
}
