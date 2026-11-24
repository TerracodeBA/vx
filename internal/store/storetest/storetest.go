// Package storetest gives tests an isolated, migrated Postgres database.
//
// Each call to New creates a fresh database on the server named by
// VX_TEST_DATABASE_URL (default: the docker-compose database) and drops it
// when the test ends. Tests are skipped when no server is reachable, unless
// VX_REQUIRE_DB is set, which CI uses to make a missing database fail.
package storetest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TerracodeBA/vx/internal/store"
)

// DefaultURL is the docker-compose development database.
const DefaultURL = "postgres://vx:vx@127.0.0.1:54329/vx?sslmode=disable"

// New returns a migrated store on a fresh database.
func New(t testing.TB) *store.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := os.Getenv("VX_TEST_DATABASE_URL")
	if admin == "" {
		admin = DefaultURL
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		if os.Getenv("VX_REQUIRE_DB") != "" {
			t.Fatalf("test database not reachable: %v", err)
		}
		t.Skipf("skipping: no test database (%v). Run `make db`.", err)
	}
	name := fmt.Sprintf("vx_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "create database "+name); err != nil {
		conn.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	s, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		ctx := context.Background()
		_, err := conn.Exec(ctx, "drop database if exists "+name+" with (force)")
		if err != nil && !strings.Contains(err.Error(), "does not exist") {
			t.Logf("drop test database: %v", err)
		}
		conn.Close(ctx)
	})
	return s
}
