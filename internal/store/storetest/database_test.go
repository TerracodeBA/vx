package storetest

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestTestDatabaseNames(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	a, b := newDBName(now), newDBName(now)
	if a == b {
		t.Fatalf("two names in the same second are equal: %s", a)
	}
	if got, ok := dbCreated(a); !ok || !got.Equal(now) {
		t.Fatalf("dbCreated(%s) = %v %v", a, got, ok)
	}
	old := "vx_test_1790000000123456789"
	if got, ok := dbCreated(old); !ok || got.Unix() != 1_790_000_000 {
		t.Fatalf("dbCreated(%s) = %v %v", old, got, ok)
	}
	for _, other := range []string{"vx", "vx_test_", "vx_test_abc", "vx_test_12_xyz", "vx_testing_1790000000_ab"} {
		if _, ok := dbCreated(other); ok {
			t.Fatalf("dbCreated(%s) claims it", other)
		}
	}
}

func adminConn(t *testing.T) *pgx.Conn {
	t.Helper()
	freshDatabase(t) // skips without a server
	admin := os.Getenv("VX_TEST_DATABASE_URL")
	if admin == "" {
		admin = DefaultURL
	}
	conn, err := pgx.Connect(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// A name that is taken (another run made it in the same instant) is
// retried with a new one.
func TestTestDatabaseRetriesTakenName(t *testing.T) {
	conn := adminConn(t)
	ctx := context.Background()
	taken := newDBName(time.Now())
	if _, err := conn.Exec(ctx, "create database "+taken); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Exec(ctx, "drop database if exists "+taken+" with (force)") })

	orig := newDBName
	t.Cleanup(func() { newDBName = orig })
	calls := 0
	newDBName = func(now time.Time) string {
		calls++
		if calls == 1 {
			return taken
		}
		return orig(now)
	}
	name, err := createDatabase(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Exec(ctx, "drop database if exists "+name+" with (force)") })
	if name == taken || calls != 2 {
		t.Fatalf("created %s after %d names", name, calls)
	}

	// Every name taken: a clear error, not a loop.
	newDBName = func(time.Time) string { return taken }
	if _, err := createDatabase(ctx, conn); err == nil || !strings.Contains(err.Error(), "names were taken") {
		t.Fatalf("all taken: %v", err)
	}
}

// Leftovers older than a day are dropped; fresh ones and other databases
// are kept.
func TestTestDatabaseDropsStale(t *testing.T) {
	conn := adminConn(t)
	ctx := context.Background()
	now := time.Now()
	stale := newDBName(now.Add(-25 * time.Hour))
	staleOld := "vx_test_" + strconv.FormatInt(now.Add(-48*time.Hour).UnixNano(), 10)
	fresh := newDBName(now.Add(-time.Hour))
	for _, n := range []string{stale, staleOld, fresh} {
		if _, err := conn.Exec(ctx, "create database "+n); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Exec(ctx, "drop database if exists "+n+" with (force)") })
	}
	dropped := dropStale(ctx, t, conn, now)
	if !slices.Contains(dropped, stale) || !slices.Contains(dropped, staleOld) || slices.Contains(dropped, fresh) {
		t.Fatalf("dropped %v; stale %s, old-style %s, fresh %s", dropped, stale, staleOld, fresh)
	}
	var left []string
	rows, err := conn.Query(ctx, `select datname from pg_database where datname = any($1)`, []string{stale, staleOld, fresh})
	if err != nil {
		t.Fatal(err)
	}
	if left, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != fresh {
		t.Fatalf("left %v, want only %s", left, fresh)
	}
}
