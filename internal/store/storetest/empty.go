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
)

// NewDatabase creates a fresh, empty database (no migrations applied) and
// returns its URL; it is dropped when the test ends. Tests of data
// migrations use it with store.MigrateTo.
func NewDatabase(t testing.TB) string {
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
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := conn.Exec(ctx, "drop database if exists "+name+" with (force)")
		if err != nil && !strings.Contains(err.Error(), "does not exist") {
			t.Logf("drop test database: %v", err)
		}
		conn.Close(ctx)
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
