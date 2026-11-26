package store

// Deleting and purging organizations (#40). See migration 033 and
// internal/service/org_delete.go for the rules; this file has the queries.
//
// Deletion only marks the organization (orgs.deleted_at): every query that
// reads repositories or organizations for callers leaves deleted ones out
// (repoFrom, OrgByHandle, ListOrgs, ...). The purge then removes the rows
// in small batches, each in its own short transaction, so it never holds
// locks on a busy table for long and a crash loses at most one batch:
// running it again continues where it stopped.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TerracodeBA/vx/internal/hash"
)

// ErrPurgeStarted is returned when restoring an organization whose purge
// has begun.
var ErrPurgeStarted = errors.New("its purge has started")

// OrgDeletion is the record of an organization's deletion. It names no
// repository and holds no content.
type OrgDeletion struct {
	OrgID        uuid.UUID
	Handle       string
	RequestedBy  string // principal ref
	DeletedAt    time.Time
	PurgeAfter   time.Time
	PurgeStarted *time.Time
	PurgedAt     *time.Time
	PurgedBy     string // "schedule" or "console"
	Step         string
	Counts       map[string]int64
}

const deletionCols = `d.org_id, d.handle, d.requested_by, d.deleted_at, d.purge_after, d.purge_started, d.purged_at, d.purged_by, d.step, d.counts`

func scanDeletion(row interface{ Scan(...any) error }) (OrgDeletion, error) {
	var d OrgDeletion
	var counts []byte
	err := row.Scan(&d.OrgID, &d.Handle, &d.RequestedBy, &d.DeletedAt, &d.PurgeAfter, &d.PurgeStarted, &d.PurgedAt, &d.PurgedBy, &d.Step, &counts)
	if err != nil {
		return d, err
	}
	d.Counts = map[string]int64{}
	if len(counts) > 0 {
		if err := json.Unmarshal(counts, &d.Counts); err != nil {
			return d, err
		}
	}
	return d, nil
}

// MarkOrgDeleted hides an organization from now on and records its
// deletion. It fails with ErrNotFound when the organization is already
// deleted.
func (q Q) MarkOrgDeleted(ctx context.Context, org Org, by Principal, at, purgeAfter time.Time) (OrgDeletion, error) {
	tag, err := q.db.Exec(ctx, `update orgs set deleted_at = $2, deleted_by = $3, purge_after = $4
		where id = $1 and deleted_at is null`, org.ID, at, by.ID, purgeAfter)
	if err != nil {
		return OrgDeletion{}, err
	}
	if tag.RowsAffected() == 0 {
		return OrgDeletion{}, fmt.Errorf("organization %s: %w", org.Handle, ErrNotFound)
	}
	return scanDeletion(q.db.QueryRow(ctx, `insert into org_deletions as d (org_id, handle, requested_by, deleted_at, purge_after)
		values ($1, $2, $3, $4, $5)
		on conflict (org_id) do update set requested_by = excluded.requested_by, deleted_at = excluded.deleted_at,
			purge_after = excluded.purge_after, purge_started = null, purged_at = null, purged_by = '', step = '', counts = '{}'
		returning `+deletionCols, org.ID, org.Handle, by.Ref(), at, purgeAfter))
}

// RestoreDeletedOrg brings a deleted organization back as it was and drops
// its deletion record. It fails with ErrPurgeStarted once a purge began.
func (q Q) RestoreDeletedOrg(ctx context.Context, orgID uuid.UUID) error {
	var started *time.Time
	err := q.db.QueryRow(ctx, `select purge_started from org_deletions where org_id = $1 for update`, orgID).Scan(&started)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if started != nil {
		return ErrPurgeStarted
	}
	tag, err := q.db.Exec(ctx, `update orgs set deleted_at = null, deleted_by = null, purge_after = null
		where id = $1 and deleted_at is not null`, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("deleted organization: %w", ErrNotFound)
	}
	_, err = q.db.Exec(ctx, `delete from org_deletions where org_id = $1`, orgID)
	return err
}

// DeletedOrgByHandle returns a deleted organization that is not purged
// yet, locking it inside transactions (forUpdate).
func (q Q) DeletedOrgByHandle(ctx context.Context, handle string, forUpdate bool) (Org, error) {
	lock := ""
	if forUpdate {
		lock = " for update"
	}
	o, err := scanOrg(q.db.QueryRow(ctx, `select `+orgCols+` from orgs o where o.handle = $1 and o.deleted_at is not null`+lock, handle))
	return o, notFound(err, "deleted organization "+handle)
}

// OrgDeletionByID returns the deletion record of an organization.
func (q Q) OrgDeletionByID(ctx context.Context, orgID uuid.UUID) (OrgDeletion, error) {
	d, err := scanDeletion(q.db.QueryRow(ctx, `select `+deletionCols+` from org_deletions d where d.org_id = $1`, orgID))
	return d, notFound(err, "deletion record")
}

// OrgDeletionsByHandle returns the deletion records of organizations that
// had handle, newest first (a handle is free again after a purge, so it
// may have been deleted more than once).
func (q Q) OrgDeletionsByHandle(ctx context.Context, handle string) ([]OrgDeletion, error) {
	rows, err := q.db.Query(ctx, `select `+deletionCols+` from org_deletions d where d.handle = $1 order by d.deleted_at desc`, handle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgDeletion
	for rows.Next() {
		d, err := scanDeletion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeletedOrgsOwnedBy lists the deleted, not yet purged organizations
// person owns, with their deletion records, soonest purge first.
func (q Q) DeletedOrgsOwnedBy(ctx context.Context, person uuid.UUID) ([]Org, []OrgDeletion, error) {
	rows, err := q.db.Query(ctx, `select `+orgCols+`, `+deletionCols+` from orgs o
		join org_members m on m.org_id = o.id and m.person_id = $1 and m.role = 'owner'
		join org_deletions d on d.org_id = o.id
		where o.deleted_at is not null order by d.purge_after, o.handle`, person)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var orgs []Org
	var dels []OrgDeletion
	for rows.Next() {
		var o Org
		var d OrgDeletion
		var counts []byte
		if err := rows.Scan(&o.ID, &o.Handle, &o.DisplayName, &o.DefaultRepoRole, &o.Personal, &o.CreatedAt, &o.GitHubOrgID, &o.GitHubLogin,
			&d.OrgID, &d.Handle, &d.RequestedBy, &d.DeletedAt, &d.PurgeAfter, &d.PurgeStarted, &d.PurgedAt, &d.PurgedBy, &d.Step, &counts); err != nil {
			return nil, nil, err
		}
		d.Counts = map[string]int64{}
		_ = json.Unmarshal(counts, &d.Counts)
		orgs, dels = append(orgs, o), append(dels, d)
	}
	return orgs, dels, rows.Err()
}

// OrgsDueForPurge returns the ids of deleted organizations whose grace
// period ended by at, and of purges that started and did not finish,
// oldest first.
func (q Q) OrgsDueForPurge(ctx context.Context, at time.Time) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `select d.org_id from org_deletions d
		where d.purged_at is null and (d.purge_after <= $1 or d.purge_started is not null)
		order by d.purge_after, d.org_id`, at)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// CountOrgsPendingPurge counts deleted organizations not purged yet.
func (q Q) CountOrgsPendingPurge(ctx context.Context) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx, `select count(*) from org_deletions where purged_at is null`).Scan(&n)
	return n, err
}

// DeletedOrgGitHub reports whether a deleted organization holds the GitHub
// installation installationID or the GitHub account accountID (0: do not
// check that), so syncing it stops.
func (q Q) DeletedOrgGitHub(ctx context.Context, installationID, accountID int64) (string, bool, error) {
	var handle string
	err := q.db.QueryRow(ctx, `select o.handle from orgs o where o.deleted_at is not null and (
		($2 <> 0 and o.github_org_id = $2)
		or o.id = (select i.org_id from github_installations i where i.id = $1)) limit 1`, installationID, accountID).Scan(&handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return handle, err == nil, err
}

// ---------- the purge ----------

// purgeLock is the advisory lock key held by the one server that purges.
const purgeLock = 0x76785f7075726765 // "vx_purge"

// TryPurgeLock takes the purge lock if no other server holds it. It is a
// session lock on a connection of its own: if the server dies, the
// connection closes and the lock is free again. alive reports whether the
// lock is still held (the connection works); unlock releases it.
func (s *Store) TryPurgeLock(ctx context.Context) (alive func(context.Context) bool, unlock func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, int64(purgeLock)).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, nil, false, err
	}
	alive = func(ctx context.Context) bool { return conn.Ping(ctx) == nil }
	unlock = func() {
		_, _ = conn.Exec(context.Background(), `select pg_advisory_unlock($1)`, int64(purgeLock))
		conn.Release()
	}
	return alive, unlock, true, nil
}

// PurgeOrgRow is a deleted organization as the purge sees it.
type PurgeOrgRow struct {
	Org
	Deletion OrgDeletion
	Repos    []uuid.UUID
}

// PurgeTarget reads a deleted organization for its purge and marks the
// purge started (a restore is refused from then on).
func (s *Store) PurgeTarget(ctx context.Context, orgID uuid.UUID, by string, at time.Time) (PurgeOrgRow, error) {
	var p PurgeOrgRow
	err := s.InTx(ctx, func(tx *Tx) error {
		var err error
		p.Deletion, err = scanDeletion(tx.db.QueryRow(ctx, `select `+deletionCols+` from org_deletions d where d.org_id = $1 for update`, orgID))
		if err != nil {
			return notFound(err, "deletion record")
		}
		if p.Deletion.PurgedAt != nil {
			return nil
		}
		o, err := scanOrg(tx.db.QueryRow(ctx, `select `+orgCols+` from orgs o where o.id = $1`, orgID))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			p.Org = o
			var deleted *time.Time
			if err := tx.db.QueryRow(ctx, `select deleted_at from orgs where id = $1`, orgID).Scan(&deleted); err != nil {
				return err
			}
			if deleted == nil {
				return fmt.Errorf("organization %s is not deleted: %w", o.Handle, ErrNotFound)
			}
		}
		if p.Deletion.PurgeStarted == nil {
			if _, err := tx.db.Exec(ctx, `update org_deletions set purge_started = $2, purged_by = $3, step = 'repositories' where org_id = $1`, orgID, at, by); err != nil {
				return err
			}
			p.Deletion.PurgeStarted, p.Deletion.PurgedBy, p.Deletion.Step = &at, by, "repositories"
		}
		rows, err := tx.db.Query(ctx, `select id from repos where org_id = $1 order by id`, orgID)
		if err != nil {
			return err
		}
		p.Repos, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return p, err
}

// addCounts adds to the deletion record's counts.
func (q Q) addCounts(ctx context.Context, orgID uuid.UUID, add map[string]int64) error {
	for k, v := range add {
		if v == 0 {
			delete(add, k)
		}
	}
	if len(add) == 0 {
		return nil
	}
	b, err := json.Marshal(add)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `update org_deletions set counts = (
		select coalesce(jsonb_object_agg(key, total), '{}'::jsonb) from (
			select key, sum(value::bigint) as total from (
				select * from jsonb_each_text(counts) union all select * from jsonb_each_text($2::jsonb)
			) x group by key) y)
		where org_id = $1`, orgID, b)
	return err
}

// AddPurgeCounts adds to what a purge removed.
func (q Q) AddPurgeCounts(ctx context.Context, orgID uuid.UUID, add map[string]int64) error {
	return q.addCounts(ctx, orgID, add)
}

// ExistingRepoIDs reports which of ids are repositories (of live or
// deleted organizations).
func (q Q) ExistingRepoIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := q.db.Query(ctx, `select id from repos where id = any($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(found))
	for _, id := range found {
		out[id] = true
	}
	return out, nil
}

// SetPurgeStep records where a purge is.
func (q Q) SetPurgeStep(ctx context.Context, orgID uuid.UUID, step string) error {
	_, err := q.db.Exec(ctx, `update org_deletions set step = $2 where org_id = $1`, orgID, step)
	return err
}

// PurgeBatch is how many rows one purge transaction deletes.
var PurgeBatch = 5000

// repoPurgeSteps are the large tables of a repository, deleted in batches
// before the repository row (whose delete cascades to the small rest).
// Each query deletes at most $2 rows of repository $1 and returns how many.
var repoPurgeSteps = []struct{ count, sql string }{
	{"", `delete from push_sent where (principal_id, item) in (select ps.principal_id, ps.item from push_sent ps
		where ps.item in (select 'question:' || n.id from notes n where n.repo_id = $1
			union all select 'review:' || v.id from reviews v where v.repo_id = $1) limit $2)`},
	{"git_mappings", `delete from git_objects where (repo_id, git_hash) in (select repo_id, git_hash from git_objects where repo_id = $1 limit $2)`},
	{"", `delete from file_changes where (repo_id, changeset_hash, path) in (select repo_id, changeset_hash, path from file_changes where repo_id = $1 limit $2)`},
	{"operations", `delete from op_log where id in (select id from op_log where repo_id = $1 limit $2)`},
	{"notes", `delete from notes where id in (select id from notes where repo_id = $1 and id = thread_id limit $2)`},
	{"notes", `delete from notes where id in (select id from notes where repo_id = $1 limit $2)`},
	{"changes", `delete from changesets where (repo_id, hash) in (select repo_id, hash from changesets where repo_id = $1 limit $2)`},
}

// repoCascadeCounts are counted just before the repository row goes, with
// everything its delete cascades to.
var repoCascadeCounts = []struct{ name, sql string }{
	{"efforts", `select count(*) from efforts where repo_id = $1`},
	{"workspaces", `select count(*) from workspaces w join efforts e on e.id = w.effort_id where e.repo_id = $1`},
	{"shipments", `select count(*) from shipments s join efforts e on e.id = s.effort_id where e.repo_id = $1`},
	{"claims", `select count(*) from claims where repo_id = $1`},
	{"tasks", `select count(*) from tasks where repo_id = $1`},
	{"rebases", `select count(*) from effort_rebases where repo_id = $1`},
	{"reviews", `select count(*) from reviews where repo_id = $1`},
	{"reservations", `select count(*) from rule_reservations where repo_id = $1`},
	{"grants", `select count(*) from repo_grants where repo_id = $1`},
	{"support_grants", `select count(*) from support_grants where repo_id = $1`},
	{"check_jobs", `select count(*) from check_jobs where repo_id = $1`},
	{"ci_tokens", `select count(*) from ci_tokens where repo_id = $1`},
}

// PurgeRepoObjects deletes up to PurgeBatch of a repository's object
// records, and marks for deletion the blobs among them that no repository
// outside purged (the organization's repositories) records. It returns how
// many records it deleted; 0 means none are left.
func (s *Store) PurgeRepoObjects(ctx context.Context, orgID, repoID uuid.UUID, purged []uuid.UUID) (int, error) {
	var n int
	err := s.InTx(ctx, func(tx *Tx) error {
		rows, err := tx.db.Query(ctx, `select hash from repo_objects where repo_id = $1 order by hash limit $2`, repoID, PurgeBatch)
		if err != nil {
			return err
		}
		hs, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil || len(hs) == 0 {
			return err
		}
		n = len(hs)
		if _, err := tx.db.Exec(ctx, `insert into blob_deletions (hash)
			select h from unnest($1::bytea[]) as u(h)
			where not exists (select 1 from repo_objects o where o.hash = u.h and o.repo_id <> all($2::uuid[]))
			on conflict (hash) do update set marked_at = now(), deleted_at = null`, hs, purged); err != nil {
			return err
		}
		if _, err := tx.db.Exec(ctx, `delete from repo_objects where repo_id = $1 and hash = any($2::bytea[])`, repoID, hs); err != nil {
			return err
		}
		return tx.addCounts(ctx, orgID, map[string]int64{"object_records": int64(n)})
	})
	return n, err
}

// PurgeRepoStep runs step i of repoPurgeSteps once for a repository and
// returns how many rows it deleted; ok is false past the last step.
func (s *Store) PurgeRepoStep(ctx context.Context, orgID, repoID uuid.UUID, i int) (n int64, ok bool, err error) {
	if i >= len(repoPurgeSteps) {
		return 0, false, nil
	}
	step := repoPurgeSteps[i]
	err = s.InTx(ctx, func(tx *Tx) error {
		tag, err := tx.db.Exec(ctx, step.sql, repoID, PurgeBatch)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		if step.count != "" {
			return tx.addCounts(ctx, orgID, map[string]int64{step.count: n})
		}
		return nil
	})
	return n, true, err
}

// PurgeRepoRow deletes what is left of a repository: its row and every
// small table that cascades from it, counted first.
func (s *Store) PurgeRepoRow(ctx context.Context, orgID, repoID uuid.UUID) error {
	return s.InTx(ctx, func(tx *Tx) error {
		counts := map[string]int64{}
		for _, c := range repoCascadeCounts {
			var n int64
			if err := tx.db.QueryRow(ctx, c.sql, repoID).Scan(&n); err != nil {
				return err
			}
			counts[c.name] += n
		}
		tag, err := tx.db.Exec(ctx, `delete from repos where id = $1`, repoID)
		if err != nil {
			return err
		}
		counts["repositories"] = tag.RowsAffected()
		return tx.addCounts(ctx, orgID, counts)
	})
}

// PendingBackfillsOutside reports whether a repository other than the
// given ones waits for its object backfill: until it is indexed, the
// objects it uses are not in repo_objects, so no blob may be deleted.
func (q Q) PendingBackfillsOutside(ctx context.Context, repos []uuid.UUID) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx, `select count(*) from repo_objects_backfill where repo_id <> all($1::uuid[])`, repos).Scan(&n)
	return n, err
}

// BlobLockKey is the advisory lock key of one object's hash, shared by
// the purge (exclusive, while it checks and deletes) and recording
// (shared, while it checks an object a purge marked).
func BlobLockKey(h hash.Hash) int64 { return int64(binary.BigEndian.Uint64(h[:8])) }

// SweepBlobs deletes up to limit blobs marked for deletion that no
// repository records, with del (the blob store's delete), and forgets
// their sizes. Marked blobs a repository records again are unmarked. It
// returns how many it deleted and how many marks it handled (0: none
// left).
func (s *Store) SweepBlobs(ctx context.Context, orgID *uuid.UUID, limit int, del func(context.Context, hash.Hash) error) (deleted, handled int, err error) {
	err = s.InTx(ctx, func(tx *Tx) error {
		rows, err := tx.db.Query(ctx, `select hash from blob_deletions where deleted_at is null order by hash limit $1 for update skip locked`, limit)
		if err != nil {
			return err
		}
		hs, err := pgx.CollectRows(rows, pgx.RowTo[hash.Hash])
		if err != nil {
			return err
		}
		handled = len(hs)
		sort.Slice(hs, func(i, j int) bool { return BlobLockKey(hs[i]) < BlobLockKey(hs[j]) })
		for _, h := range hs {
			if _, err := tx.db.Exec(ctx, `select pg_advisory_xact_lock($1)`, BlobLockKey(h)); err != nil {
				return err
			}
			var used bool
			if err := tx.db.QueryRow(ctx, `select exists (select 1 from repo_objects where hash = $1)`, h).Scan(&used); err != nil {
				return err
			}
			if used {
				if _, err := tx.db.Exec(ctx, `delete from blob_deletions where hash = $1`, h); err != nil {
					return err
				}
				continue
			}
			if err := del(ctx, h); err != nil {
				return err
			}
			if _, err := tx.db.Exec(ctx, `delete from object_sizes where hash = $1`, h); err != nil {
				return err
			}
			if _, err := tx.db.Exec(ctx, `update blob_deletions set deleted_at = now() where hash = $1`, h); err != nil {
				return err
			}
			deleted++
		}
		if orgID != nil {
			return tx.addCounts(ctx, *orgID, map[string]int64{"blobs": int64(deleted)})
		}
		return nil
	})
	return deleted, handled, err
}

// PruneBlobDeletions forgets marks of blobs deleted more than keep ago.
func (q Q) PruneBlobDeletions(ctx context.Context, keep time.Duration) error {
	_, err := q.db.Exec(ctx, `delete from blob_deletions where deleted_at < now() - make_interval(secs => $1)`, keep.Seconds())
	return err
}

// orgPurgeSteps delete the organization's own rows once its repositories
// are gone, counted; the last deletes the organization, which cascades to
// what is left (members, teams, quotas, single sign-on, ...). $1 is the
// organization's id, or its handle for the steps marked byHandle.
var orgPurgeSteps = []struct {
	count, sql string
	byHandle   bool
}{
	{"members", `delete from org_members where org_id = $1`, false},
	{"teams", `delete from teams where org_id = $1`, false},
	{"", `delete from org_quotas where org_id = $1`, false},
	{"", `delete from org_sso where org_id = $1`, false},
	{"", `delete from token_sso where org_id = $1`, false},
	{"github_installation_links", `update github_installations set org_id = null where org_id = $1`, false},
	{"", `delete from github_org_membership_cache where github_org_id = (select github_org_id from orgs where id = $1)`, false},
	// Acceptances of the data processing agreement (#41); the accepted texts
	// (dpa_texts) are the operator's and shared, they stay.
	{"dpa_acceptances", `delete from org_dpa_acceptances where org_id = $1`, false},
	// The organization's own entries in the operation log describe what its
	// people did; the lifecycle entries (org.delete, org.restore, org.purge)
	// stay with the deletion record.
	{"operations", `delete from op_log where repo_id is null
		and (op like 'org.%' or op like 'team.%' or op like 'github.org.%')
		and op not in ('org.delete', 'org.restore', 'org.purge')
		and (target = $1 or starts_with(target, $1 || '/'))`, true},
	{"", `delete from orgs where id = $1`, false},
}

// PurgeOrgRows deletes the organization itself, after its repositories,
// and marks the purge done. It logs org.purge with the counts.
func (s *Store) PurgeOrgRows(ctx context.Context, orgID uuid.UUID, handle string, at time.Time) (OrgDeletion, error) {
	var d OrgDeletion
	err := s.InTx(ctx, func(tx *Tx) error {
		var left int
		if err := tx.db.QueryRow(ctx, `select count(*) from repos where org_id = $1`, orgID).Scan(&left); err != nil {
			return err
		}
		if left > 0 {
			return fmt.Errorf("organization %s still has %d repositories", handle, left)
		}
		counts := map[string]int64{}
		for _, st := range orgPurgeSteps {
			var arg any = orgID
			if st.byHandle {
				arg = handle
			}
			tag, err := tx.db.Exec(ctx, st.sql, arg)
			if err != nil {
				return err
			}
			if st.count != "" {
				counts[st.count] += tag.RowsAffected()
			}
		}
		if err := tx.addCounts(ctx, orgID, counts); err != nil {
			return err
		}
		var err error
		d, err = scanDeletion(tx.db.QueryRow(ctx, `update org_deletions d set purged_at = $2, step = 'done' where org_id = $1 returning `+deletionCols, orgID, at))
		if err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, nil, "org.purge", handle, nil, map[string]any{"org_id": orgID, "by": d.PurgedBy, "counts": d.Counts})
	})
	return d, err
}

// RecordRepoObjects records objects as part of a repository, like
// AddRepoObjects, and makes sure none of them was deleted by an
// organization purge meanwhile: a blob can be deleted after the caller put
// it (the put found it there and skipped writing) and before it was
// recorded. Recording happens first; then, for the objects a purge marked,
// present checks under a shared lock on each hash that the blob is still
// in the store (the purge holds the lock exclusively while it checks and
// deletes, and sees the recording if it comes later). Objects that are
// gone are unrecorded again and returned: the caller puts them again and
// records them once more, or fails and is retried.
func (s *Store) RecordRepoObjects(ctx context.Context, repoID uuid.UUID, hashes []hash.Hash, present func(context.Context, hash.Hash) (bool, error)) ([]hash.Hash, error) {
	if err := s.AddRepoObjects(ctx, repoID, hashes); err != nil {
		return nil, err
	}
	var marked []hash.Hash
	for start := 0; start < len(hashes); start += repoObjectBatch {
		end := min(start+repoObjectBatch, len(hashes))
		rows, err := s.db.Query(ctx, `select hash from blob_deletions where hash = any($1::bytea[])`, hashBytes(hashes[start:end]))
		if err != nil {
			return nil, err
		}
		hs, err := pgx.CollectRows(rows, pgx.RowTo[hash.Hash])
		if err != nil {
			return nil, err
		}
		marked = append(marked, hs...)
	}
	if len(marked) == 0 {
		return nil, nil
	}
	sort.Slice(marked, func(i, j int) bool { return BlobLockKey(marked[i]) < BlobLockKey(marked[j]) })
	var missing []hash.Hash
	err := s.InTx(ctx, func(tx *Tx) error {
		missing = missing[:0]
		for _, h := range marked {
			if _, err := tx.db.Exec(ctx, `select pg_advisory_xact_lock_shared($1)`, BlobLockKey(h)); err != nil {
				return err
			}
			ok, err := present(ctx, h)
			if err != nil {
				return err
			}
			if !ok {
				missing = append(missing, h)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		_, err := tx.db.Exec(ctx, `delete from repo_objects where repo_id = $1 and hash = any($2::bytea[])`, repoID, hashBytes(missing))
		return err
	})
	return missing, err
}

// PurgeLeftovers walks every table and lists the columns that still hold
// any of ids (in uuid, uuid[], text and jsonb columns) or hashes (in bytea
// columns), as "table.column (N rows)". After a purge, with the ids of the
// organization and of everything that was in it, it must be empty; tests
// use it so that tables added later are checked too. Not walked: the
// deletion record (org_deletions), the lifecycle entries of the operation
// log (org.delete, org.restore, org.purge) and the marks of deleted blobs
// (blob_deletions, kept a day; hashes, not content).
func (q Q) PurgeLeftovers(ctx context.Context, ids []uuid.UUID, hashes [][]byte) ([]string, error) {
	rows, err := q.db.Query(ctx, `select c.table_name, c.column_name, c.data_type, c.udt_name
		from information_schema.columns c join information_schema.tables t using (table_schema, table_name)
		where c.table_schema = current_schema() and t.table_type = 'BASE TABLE'
		  and c.table_name not in ('org_deletions', 'blob_deletions')
		order by c.table_name, c.ordinal_position`)
	if err != nil {
		return nil, err
	}
	type col struct{ table, column, typ, udt string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.column, &c.typ, &c.udt); err != nil {
			rows.Close()
			return nil, err
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	texts := make([]string, len(ids))
	for i, id := range ids {
		texts[i] = id.String()
	}
	if hashes == nil {
		hashes = [][]byte{}
	}
	var out []string
	for _, c := range cols {
		ident := pgx.Identifier{c.column}.Sanitize()
		var cond string
		var arg any
		switch {
		case c.udt == "uuid":
			cond, arg = ident+` = any($1::uuid[])`, ids
		case c.udt == "_uuid":
			cond, arg = ident+` && $1::uuid[]`, ids
		case c.udt == "text" || c.udt == "varchar" || c.udt == "jsonb" || c.udt == "_text":
			cond, arg = `exists (select 1 from unnest($1::text[]) i where `+ident+`::text like '%' || i || '%')`, texts
		case c.udt == "bytea":
			cond, arg = ident+` = any($1::bytea[])`, hashes
		default:
			continue
		}
		if c.table == "op_log" {
			cond = `op not in ('org.delete', 'org.restore', 'org.purge') and ` + cond
		}
		var n int64
		sql := `select count(*) from ` + pgx.Identifier{c.table}.Sanitize() + ` where ` + cond
		if err := q.db.QueryRow(ctx, sql, arg).Scan(&n); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", c.table, c.column, err)
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("%s.%s (%d rows)", c.table, c.column, n))
		}
	}
	return out, nil
}

// OrgRecordIDs returns the ids of an organization and of every record in
// it: its repositories, teams, and the rows of every table with an id
// column that belong to one of its repositories (a repo_id column) or
// efforts (an effort_id column). It walks the schema, so tables added
// later are included; with PurgeLeftovers it checks a purge.
func (q Q) OrgRecordIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	ids := []uuid.UUID{orgID}
	collect := func(sql string, args ...any) error {
		rows, err := q.db.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		ids = append(ids, got...)
		return err
	}
	if err := collect(`select id from repos where org_id = $1 union all select id from teams where org_id = $1`, orgID); err != nil {
		return nil, err
	}
	rows, err := q.db.Query(ctx, `select c.table_name, bool_or(c.column_name = 'repo_id'), bool_or(c.column_name = 'effort_id')
		from information_schema.columns c join information_schema.tables t using (table_schema, table_name)
		where c.table_schema = current_schema() and t.table_type = 'BASE TABLE'
		group by c.table_name
		having bool_or(c.column_name = 'id' and c.udt_name = 'uuid') and bool_or(c.column_name in ('repo_id', 'effort_id'))
		order by c.table_name`)
	if err != nil {
		return nil, err
	}
	type tbl struct {
		name           string
		repo, effortID bool
	}
	var tables []tbl
	for rows.Next() {
		var t tbl
		if err := rows.Scan(&t.name, &t.repo, &t.effortID); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	for _, t := range tables {
		name := pgx.Identifier{t.name}.Sanitize()
		var err error
		if t.repo {
			err = collect(`select id from `+name+` where repo_id in (select id from repos where org_id = $1)`, orgID)
		} else {
			err = collect(`select id from `+name+` where effort_id in (select e.id from efforts e join repos r on r.id = e.repo_id where r.org_id = $1)`, orgID)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
	}
	return ids, nil
}

// OrgObjectHashes returns the hashes of the objects an organization's
// repositories record: those no other repository records (a purge
// deletes them) and those another repository records too (they stay).
func (q Q) OrgObjectHashes(ctx context.Context, orgID uuid.UUID) (own, shared []hash.Hash, err error) {
	rows, err := q.db.Query(ctx, `select distinct o.hash, exists (select 1 from repo_objects x join repos xr on xr.id = x.repo_id
			where x.hash = o.hash and xr.org_id <> $1)
		from repo_objects o join repos r on r.id = o.repo_id where r.org_id = $1`, orgID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h hash.Hash
		var other bool
		if err := rows.Scan(&h, &other); err != nil {
			return nil, nil, err
		}
		if other {
			shared = append(shared, h)
		} else {
			own = append(own, h)
		}
	}
	return own, shared, rows.Err()
}
