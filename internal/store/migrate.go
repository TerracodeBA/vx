package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLock is the advisory lock key that serializes migrations when
// several servers start at once.
const migrationLock = 0x76785f6d6967 // "vx_mig"

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("migration %q must be named NNN_description.sql", name)
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", name, err)
		}
		data, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := range out {
		if out[i].version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1, 2, 3...; found %d at position %d", out[i].version, i+1)
		}
	}
	return out, nil
}

// Migrate applies pending migrations in order, each in its own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, migrationLock); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `select pg_advisory_unlock($1)`, migrationLock) //nolint:errcheck
	if _, err := conn.Exec(ctx, `create table if not exists schema_migrations (
		version int primary key, name text not null, applied_at timestamptz not null default now())`); err != nil {
		return err
	}
	var current int
	if err := conn.QueryRow(ctx, `select coalesce(max(version), 0) from schema_migrations`).Scan(&current); err != nil {
		return err
	}
	for _, m := range migs {
		if m.version <= current {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `insert into schema_migrations (version, name) values ($1, $2)`, m.version, m.name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}
