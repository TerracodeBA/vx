// Package store is the Postgres metadata store: principals, repos, branches,
// efforts, workspaces, changesets and the operation log. File contents and
// trees live in the blob store; this package holds everything else.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when a unique row already exists.
var ErrExists = errors.New("already exists")

// db is satisfied by *pgxpool.Pool and pgx.Tx.
type db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Q holds the query methods. Store and Tx both embed it, so every query can
// run on its own or inside a transaction.
type Q struct{ db db }

// Store is the connection pool plus queries.
type Store struct {
	Q
	pool *pgxpool.Pool
}

// Tx is a transaction plus queries.
type Tx struct {
	Q
	tx pgx.Tx
}

// Open connects to Postgres.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{Q: Q{db: pool}, pool: pool}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// InTx runs fn in a transaction. The transaction commits if fn returns nil.
func (s *Store) InTx(ctx context.Context, fn func(tx *Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(t pgx.Tx) error {
		return fn(&Tx{Q: Q{db: t}, tx: t})
	})
}

func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// NewID returns a new random UUID.
func NewID() uuid.UUID { return uuid.New() }
