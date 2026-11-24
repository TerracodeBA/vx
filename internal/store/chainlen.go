package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
)

// ChainLength counts the changesets Chain would return, without loading
// them: from head back along first parents, stopping before stop or after
// limit.
func (q Q) ChainLength(ctx context.Context, repoID uuid.UUID, head, stop hash.Hash, limit int) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `with recursive chain(hash, first_parent, depth) as (
			select hash, first_parent, 1 from changesets where repo_id = $1 and hash = $2 and hash <> coalesce($3, ''::bytea)
			union all
			select c.hash, c.first_parent, chain.depth + 1
			from changesets c join chain on c.repo_id = $1 and c.hash = chain.first_parent
			where c.hash <> coalesce($3, ''::bytea) and chain.depth < $4
		)
		select count(*) from chain`, repoID, head, stop, limit).Scan(&n)
	return n, err
}
