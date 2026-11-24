package store

// Review and CI feedback from the git host (v0.4, #7): finding the
// shipment a pull request event is about, CI results per shipment, the
// keys that let a system note supersede the previous one on the same
// subject, and gaps in the GitHub App's permissions.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ShipmentRef is a shipment with the effort and repository it belongs to.
type ShipmentRef struct {
	Shipment
	RepoID uuid.UUID
}

// ShipmentByPR returns the newest shipment of the repository for a pull
// request: by its number on the host (provider_ref) when number is set,
// else, or when no shipment has that number, by its head branch.
func (q Q) ShipmentByPR(ctx context.Context, repoID uuid.UUID, number, branch string) (ShipmentRef, error) {
	if number != "" {
		s, err := q.shipmentWhere(ctx, `e.repo_id = $1 and s.provider_ref = $2`, repoID, number)
		if err == nil || !errors.Is(err, ErrNotFound) || branch == "" {
			return s, err
		}
	}
	if branch == "" {
		return ShipmentRef{}, notFound(pgx.ErrNoRows, "shipment")
	}
	return q.shipmentWhere(ctx, `e.repo_id = $1 and s.branch = $2`, repoID, branch)
}

// ShipmentsByHeadSHA returns the repository's shipments whose pushed head
// commit is sha, newest first.
func (q Q) ShipmentsByHeadSHA(ctx context.Context, repoID uuid.UUID, sha string) ([]ShipmentRef, error) {
	rows, err := q.db.Query(ctx, `select s.id from shipments s join efforts e on e.id = s.effort_id
		where e.repo_id = $1 and s.head_git_sha = $2 order by s.updated_at desc limit 20`, repoID, sha)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make([]ShipmentRef, 0, len(ids))
	for _, id := range ids {
		s, err := q.shipmentWhere(ctx, `s.id = $1`, id)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (q Q) shipmentWhere(ctx context.Context, where string, args ...any) (ShipmentRef, error) {
	var id, repoID uuid.UUID
	var effortID uuid.UUID
	err := q.db.QueryRow(ctx, `select s.id, s.effort_id, e.repo_id from shipments s join efforts e on e.id = s.effort_id
		where `+where+` order by s.updated_at desc limit 1`, args...).Scan(&id, &effortID, &repoID)
	if err != nil {
		return ShipmentRef{}, notFound(err, "shipment")
	}
	all, err := q.ListShipments(ctx, effortID)
	if err != nil {
		return ShipmentRef{}, err
	}
	for _, s := range all {
		if s.ID == id {
			return ShipmentRef{Shipment: s, RepoID: repoID}, nil
		}
	}
	return ShipmentRef{}, notFound(pgx.ErrNoRows, "shipment")
}

// ShipmentCheck is one CI check reported for a shipment's commit.
type ShipmentCheck struct {
	ShipmentID uuid.UUID
	HeadSHA    string
	Source     string // check_run, status, pipeline
	Name       string
	Status     string // queued, in_progress, completed
	Conclusion string // success, failure, ... once completed
	URL        string
	UpdatedAt  time.Time
}

// PutShipmentCheck records the latest report of one check.
func (q Q) PutShipmentCheck(ctx context.Context, c ShipmentCheck) error {
	_, err := q.db.Exec(ctx, `insert into shipment_checks (shipment_id, head_sha, source, name, status, conclusion, url, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, now())
		on conflict (shipment_id, head_sha, source, name) do update set
			status = excluded.status, conclusion = excluded.conclusion, url = excluded.url, updated_at = now()`,
		c.ShipmentID, c.HeadSHA, c.Source, c.Name, c.Status, c.Conclusion, c.URL)
	return err
}

// ShipmentChecks returns the checks reported for a shipment's commit, by
// name.
func (q Q) ShipmentChecks(ctx context.Context, shipmentID uuid.UUID, sha string) ([]ShipmentCheck, error) {
	rows, err := q.db.Query(ctx, `select shipment_id, head_sha, source, name, status, conclusion, url, updated_at
		from shipment_checks where shipment_id = $1 and head_sha = $2 order by name, source`, shipmentID, sha)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ShipmentCheck, error) {
		var c ShipmentCheck
		err := r.Scan(&c.ShipmentID, &c.HeadSHA, &c.Source, &c.Name, &c.Status, &c.Conclusion, &c.URL, &c.UpdatedAt)
		return c, err
	})
}

// ShipmentCI is the CI result of a shipment's head commit.
type ShipmentCI struct {
	ShipmentID uuid.UUID
	HeadSHA    string
	State      string // pending, passed, failed
	UpdatedAt  time.Time
}

// SetShipmentCI stores a shipment's CI result.
func (q Q) SetShipmentCI(ctx context.Context, c ShipmentCI) error {
	_, err := q.db.Exec(ctx, `insert into shipment_ci (shipment_id, head_sha, state, updated_at) values ($1, $2, $3, now())
		on conflict (shipment_id) do update set head_sha = excluded.head_sha, state = excluded.state, updated_at = now()`,
		c.ShipmentID, c.HeadSHA, c.State)
	return err
}

// ShipmentCIs returns the CI results of shipments, by shipment id; a
// shipment without one is missing.
func (q Q) ShipmentCIs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ShipmentCI, error) {
	out := map[uuid.UUID]ShipmentCI{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx, `select shipment_id, head_sha, state, updated_at from shipment_ci where shipment_id = any($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ShipmentCI
		if err := rows.Scan(&c.ShipmentID, &c.HeadSHA, &c.State, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out[c.ShipmentID] = c
	}
	return out, rows.Err()
}

// SystemNoteKey is the latest system note vx posted on one subject.
type SystemNoteKey struct {
	EffortID uuid.UUID
	Key      string
	NoteID   uuid.UUID
	Digest   string // what the note said, in short: an unchanged digest posts nothing
}

// SystemNoteKey returns the latest note on key in an effort.
func (q Q) SystemNoteKey(ctx context.Context, effortID uuid.UUID, key string) (SystemNoteKey, error) {
	k := SystemNoteKey{EffortID: effortID, Key: key}
	err := q.db.QueryRow(ctx, `select note_id, digest from system_note_keys where effort_id = $1 and key = $2`, effortID, key).Scan(&k.NoteID, &k.Digest)
	return k, notFound(err, "system note "+key)
}

// LockSystemNoteKey serializes writers of one key until the transaction
// ends (two deliveries of the same CI result at once post one note).
func (q Q) LockSystemNoteKey(ctx context.Context, effortID uuid.UUID, key string) error {
	_, err := q.db.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1::text || '/' || $2::text, 0))`, effortID.String(), key)
	return err
}

// PutSystemNoteKey records the latest note on key.
func (q Q) PutSystemNoteKey(ctx context.Context, k SystemNoteKey) error {
	_, err := q.db.Exec(ctx, `insert into system_note_keys (effort_id, key, note_id, digest, updated_at) values ($1, $2, $3, $4, now())
		on conflict (effort_id, key) do update set note_id = excluded.note_id, digest = excluded.digest, updated_at = now()`,
		k.EffortID, k.Key, k.NoteID, k.Digest)
	return err
}

// SystemNotesTo returns system notes addressed to one of to (review and CI
// notes for a change's author), created at or after since, that no newer
// note replaced, newest first.
func (q Q) SystemNotesTo(ctx context.Context, to []uuid.UUID, since time.Time, limit int) ([]Note, error) {
	return q.queryNotes(ctx, `select `+noteCols+noteFrom+`where n.system and n.to_id = any($1) and n.created_at >= $2 and `+notSuperseded+`
		and e.state in ('open', 'shipping')
		order by n.created_at desc, n.seq desc limit $3`, to, since, limit)
}

// GitHubAppGap is a permission or event the App lacks.
type GitHubAppGap struct {
	What   string // e.g. "checks:write"
	Detail string
	SeenAt time.Time
}

// AddGitHubAppGap records that GitHub refused a call for want of what.
func (q Q) AddGitHubAppGap(ctx context.Context, what, detail string) error {
	_, err := q.db.Exec(ctx, `insert into github_app_gaps (what, detail) values ($1, $2)
		on conflict (what) do update set detail = excluded.detail, seen_at = now()`, what, detail)
	return err
}

// ClearGitHubAppGap forgets a gap (a call that needed it succeeded); an
// empty what forgets them all (a new App).
func (q Q) ClearGitHubAppGap(ctx context.Context, what string) error {
	_, err := q.db.Exec(ctx, `delete from github_app_gaps where $1 = '' or what = $1`, what)
	return err
}

// GitHubAppGaps lists the recorded gaps.
func (q Q) GitHubAppGaps(ctx context.Context) ([]GitHubAppGap, error) {
	rows, err := q.db.Query(ctx, `select what, detail, seen_at from github_app_gaps order by what`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GitHubAppGap, error) {
		var g GitHubAppGap
		err := r.Scan(&g.What, &g.Detail, &g.SeenAt)
		return g, err
	})
}
