package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

// repoObjectBatch bounds how many hashes one repo_objects query carries.
const repoObjectBatch = 5000

func hashBytes(hs []hash.Hash) [][]byte {
	out := make([][]byte, 0, len(hs))
	for _, h := range hs {
		if !h.IsZero() {
			out = append(out, h.Bytes())
		}
	}
	return out
}

// AddRepoObjects records that the objects are part of the repo. Recording
// an object twice is a no-op. Only membership is per repo: the bytes are
// stored once in the blob store.
func (q Q) AddRepoObjects(ctx context.Context, repoID uuid.UUID, hashes []hash.Hash) error {
	for start := 0; start < len(hashes); start += repoObjectBatch {
		end := min(start+repoObjectBatch, len(hashes))
		_, err := q.db.Exec(ctx, `insert into repo_objects (repo_id, hash)
			select $1, h from unnest($2::bytea[]) as u(h)
			on conflict do nothing`, repoID, hashBytes(hashes[start:end]))
		if err != nil {
			return err
		}
	}
	return nil
}

// RepoObjectsMissing returns the hashes that are not part of the repo, in
// the order given. Objects stored only for other repos count as missing.
func (q Q) RepoObjectsMissing(ctx context.Context, repoID uuid.UUID, hashes []hash.Hash) ([]hash.Hash, error) {
	var missing []hash.Hash
	for start := 0; start < len(hashes); start += repoObjectBatch {
		batch := hashes[start:min(start+repoObjectBatch, len(hashes))]
		rows, err := q.db.Query(ctx, `select hash from repo_objects where repo_id = $1 and hash = any($2::bytea[])`,
			repoID, hashBytes(batch))
		if err != nil {
			return nil, err
		}
		have := make(map[hash.Hash]bool, len(batch))
		for rows.Next() {
			var h hash.Hash
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return nil, err
			}
			have[h] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, h := range batch {
			if !have[h] {
				missing = append(missing, h)
			}
		}
	}
	return missing, nil
}

// CountRepoObjects returns how many objects are recorded for the repo.
func (q Q) CountRepoObjects(ctx context.Context, repoID uuid.UUID) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `select count(*) from repo_objects where repo_id = $1`, repoID).Scan(&n)
	return n, err
}

// QueueObjectBackfills queues every repo that has changesets but no
// recorded objects, such as repos created by a server older than
// repo_objects. It returns how many were newly queued.
func (q Q) QueueObjectBackfills(ctx context.Context) (int, error) {
	tag, err := q.db.Exec(ctx, `insert into repo_objects_backfill (repo_id)
		select r.id from repos r
		where exists (select 1 from changesets c where c.repo_id = r.id)
		  and not exists (select 1 from repo_objects o where o.repo_id = r.id)
		on conflict do nothing`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// QueueObjectBackfill queues one repo for a backfill.
func (q Q) QueueObjectBackfill(ctx context.Context, repoID uuid.UUID) error {
	_, err := q.db.Exec(ctx, `insert into repo_objects_backfill (repo_id) values ($1) on conflict do nothing`, repoID)
	return err
}

// ObjectBackfillQueue lists the repos waiting for a backfill, oldest first.
func (q Q) ObjectBackfillQueue(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `select repo_id from repo_objects_backfill order by queued_at, repo_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ObjectBackfillPending reports whether the repo still waits for a backfill.
func (q Q) ObjectBackfillPending(ctx context.Context, repoID uuid.UUID) (bool, error) {
	var one int
	err := q.db.QueryRow(ctx, `select 1 from repo_objects_backfill where repo_id = $1`, repoID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// FinishObjectBackfill removes the repo from the backfill queue.
func (q Q) FinishObjectBackfill(ctx context.Context, repoID uuid.UUID) error {
	_, err := q.db.Exec(ctx, `delete from repo_objects_backfill where repo_id = $1`, repoID)
	return err
}

// ChangesetTrees is the tree information of one changeset, for walking
// every tree in a repo.
type ChangesetTrees struct {
	Hash         hash.Hash
	FirstParent  hash.Hash
	Tree         hash.Hash
	SnapshotTree hash.Hash // the uploaded snapshot of a merged land, else zero
}

// ListChangesetTrees returns the trees of every changeset in the repo, in
// the order they were created.
func (q Q) ListChangesetTrees(ctx context.Context, repoID uuid.UUID) ([]ChangesetTrees, error) {
	rows, err := q.db.Query(ctx, `select hash, first_parent, tree, raw from changesets
		where repo_id = $1 order by created_at, hash`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangesetTrees
	for rows.Next() {
		var c ChangesetTrees
		var raw []byte
		if err := rows.Scan(&c.Hash, &c.FirstParent, &c.Tree, &raw); err != nil {
			return nil, err
		}
		if cs, err := object.DecodeChangeset(raw); err == nil && cs.Merged() {
			c.SnapshotTree = cs.SnapshotTree
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResetObjectIndex forgets which objects are part of the repo and queues it
// for a backfill, which rebuilds the index from its changesets. It is for
// repairs and for tests that recreate data from before repo_objects.
func (q Q) ResetObjectIndex(ctx context.Context, repoID uuid.UUID) error {
	if _, err := q.db.Exec(ctx, `delete from repo_objects where repo_id = $1`, repoID); err != nil {
		return err
	}
	return q.QueueObjectBackfill(ctx, repoID)
}
