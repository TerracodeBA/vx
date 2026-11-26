// Package storetest gives tests an isolated, migrated Postgres database.
//
// Each call to New creates a fresh database on the server named by
// VX_TEST_DATABASE_URL (default: the docker-compose database) and drops it
// when the test ends. Tests are skipped when no server is reachable, unless
// VX_REQUIRE_DB is set, which CI uses to make a missing database fail.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// DefaultURL is the docker-compose development database.
const DefaultURL = "postgres://vx:vx@127.0.0.1:54329/vx?sslmode=disable"

// New returns a migrated store on a fresh database.
func New(t testing.TB) *store.Store {
	t.Helper()
	s, _ := NewWithURL(t)
	return s
}

// NewWithURL is New that also returns the database's URL, for tests that
// connect more than one server to it.
func NewWithURL(t testing.TB) (*store.Store, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbURL := freshDatabase(t)
	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close) // runs before the database is dropped
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s, dbURL
}

// AddTree records the objects of tree that are not at the same path in
// from as part of the repo, as an upload with that repo would. Tests that
// write trees straight into a blob store call it before landing them.
func AddTree(t testing.TB, s *store.Store, blobs blob.Store, repoID uuid.UUID, from, tree hash.Hash) {
	t.Helper()
	ctx := context.Background()
	trees, files, err := object.NewObjects(ctx, blobs, from, tree)
	if err != nil {
		t.Fatalf("walk tree %s: %v", tree.Short(), err)
	}
	if err := s.AddRepoObjects(ctx, repoID, append(trees, files...)); err != nil {
		t.Fatalf("record objects: %v", err)
	}
}
