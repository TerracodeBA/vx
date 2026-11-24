package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
)

// ObjectSize is the bytes one object takes in the blob store.
type ObjectSize struct {
	Hash  hash.Hash
	Bytes int64
}

// RecordObjectSizes stores object sizes; a size recorded before is kept
// (objects are immutable, so it cannot differ).
func (q Q) RecordObjectSizes(ctx context.Context, sizes []ObjectSize) error {
	for start := 0; start < len(sizes); start += repoObjectBatch {
		batch := sizes[start:min(start+repoObjectBatch, len(sizes))]
		hs := make([][]byte, 0, len(batch))
		ns := make([]int64, 0, len(batch))
		for _, s := range batch {
			if s.Hash.IsZero() {
				continue
			}
			hs = append(hs, s.Hash.Bytes())
			ns = append(ns, max(s.Bytes, 0))
		}
		_, err := q.db.Exec(ctx, `insert into object_sizes (hash, bytes)
			select h, n from unnest($1::bytea[], $2::bigint[]) as u(h, n)
			on conflict do nothing`, hs, ns)
		if err != nil {
			return err
		}
	}
	return nil
}

// ObjectsWithoutSize returns up to limit objects that are part of some
// repository but have no recorded size yet.
func (q Q) ObjectsWithoutSize(ctx context.Context, limit int) ([]hash.Hash, error) {
	rows, err := q.db.Query(ctx, `select distinct ro.hash from repo_objects ro
		where not exists (select 1 from object_sizes s where s.hash = ro.hash)
		limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hash.Hash
	for rows.Next() {
		var h hash.Hash
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CountObjectsWithoutSize counts objects of repositories whose size is
// not recorded yet.
func (q Q) CountObjectsWithoutSize(ctx context.Context) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx, `select count(distinct ro.hash) from repo_objects ro
		where not exists (select 1 from object_sizes s where s.hash = ro.hash)`).Scan(&n)
	return n, err
}

// RepoStoredBytes is the blob store bytes of the repository's objects.
// Objects whose size is not recorded yet count 0.
func (q Q) RepoStoredBytes(ctx context.Context, repoID uuid.UUID) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx, `select coalesce(sum(s.bytes), 0)::bigint from repo_objects ro
		join object_sizes s on s.hash = ro.hash where ro.repo_id = $1`, repoID).Scan(&n)
	return n, err
}

// OrgStoredBytes is the blob store bytes of the objects of the
// organization's repositories, each object counted once even when several
// of its repositories use it. Objects whose size is not recorded yet count 0.
func (q Q) OrgStoredBytes(ctx context.Context, orgID uuid.UUID) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx, `select coalesce(sum(s.bytes), 0)::bigint from object_sizes s
		where s.hash in (select ro.hash from repo_objects ro join repos r on r.id = ro.repo_id where r.org_id = $1)`, orgID).Scan(&n)
	return n, err
}

// ObjectSizesKnown tells which of hs have a recorded size.
func (q Q) ObjectSizesKnown(ctx context.Context, hs []hash.Hash) (map[hash.Hash]bool, error) {
	out := make(map[hash.Hash]bool, len(hs))
	for start := 0; start < len(hs); start += repoObjectBatch {
		rows, err := q.db.Query(ctx, `select hash from object_sizes where hash = any($1::bytea[])`,
			hashBytes(hs[start:min(start+repoObjectBatch, len(hs))]))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h hash.Hash
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return nil, err
			}
			out[h] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
