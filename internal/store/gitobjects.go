package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
)

// GitObject maps a git object to the vx object with the same content.
type GitObject struct {
	GitHash string
	VXHash  hash.Hash
	Kind    string // blob or tree
}

// LookupGitObjects returns the vx hashes known for the given git hashes.
func (q Q) LookupGitObjects(ctx context.Context, repoID uuid.UUID, gitHashes []string) (map[string]hash.Hash, error) {
	out := make(map[string]hash.Hash, len(gitHashes))
	if len(gitHashes) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx, `select git_hash, vx_hash from git_objects where repo_id = $1 and git_hash = any($2)`, repoID, gitHashes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g string
		var h hash.Hash
		if err := rows.Scan(&g, &h); err != nil {
			return nil, err
		}
		out[g] = h
	}
	return out, rows.Err()
}

// GitHashesFor returns git hashes for vx objects (the reverse mapping).
func (q Q) GitHashesFor(ctx context.Context, repoID uuid.UUID, vxHashes []hash.Hash) (map[hash.Hash]string, error) {
	out := make(map[hash.Hash]string, len(vxHashes))
	if len(vxHashes) == 0 {
		return out, nil
	}
	raw := make([][]byte, 0, len(vxHashes))
	for _, h := range vxHashes {
		raw = append(raw, h.Bytes())
	}
	rows, err := q.db.Query(ctx, `select distinct on (vx_hash) vx_hash, git_hash from git_objects where repo_id = $1 and vx_hash = any($2)`, repoID, raw)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h hash.Hash
		var g string
		if err := rows.Scan(&h, &g); err != nil {
			return nil, err
		}
		out[h] = g
	}
	return out, rows.Err()
}

// SaveGitObjects records mappings; existing ones are kept.
func (q Q) SaveGitObjects(ctx context.Context, repoID uuid.UUID, objs []GitObject) error {
	if len(objs) == 0 {
		return nil
	}
	gits := make([]string, len(objs))
	vxs := make([][]byte, len(objs))
	kinds := make([]string, len(objs))
	for i, o := range objs {
		gits[i], vxs[i], kinds[i] = o.GitHash, o.VXHash.Bytes(), o.Kind
	}
	_, err := q.db.Exec(ctx, `insert into git_objects (repo_id, git_hash, vx_hash, kind)
		select $1, g, v, k from unnest($2::text[], $3::bytea[], $4::text[]) as t(g, v, k)
		on conflict do nothing`, repoID, gits, vxs, kinds)
	return err
}
