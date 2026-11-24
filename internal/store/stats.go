package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolStat returns the database pool's statistics, for /metrics.
func (s *Store) PoolStat() *pgxpool.Stat { return s.pool.Stat() }

// CountObjectBackfills is how many repositories wait for their objects to
// be indexed.
func (q Q) CountObjectBackfills(ctx context.Context) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx, `select count(*) from repo_objects_backfill`).Scan(&n)
	return n, err
}
