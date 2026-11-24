package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OpenEffortsOwnedBy lists the open and shipping efforts the principals own, most
// recently updated first, across repositories (the For you page; callers
// filter by what the viewer may read).
func (q Q) OpenEffortsOwnedBy(ctx context.Context, owners []uuid.UUID, limit int) ([]Effort, error) {
	rows, err := q.db.Query(ctx, `select `+effortCols+` from efforts
		where owner_id = any($1) and state in ('open', 'shipping') order by updated_at desc, id limit $2`, owners, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Effort, error) { return scanEffort(r) })
}
