package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Test databases are named vx_test_<unix seconds>_<random hex>: the time
// lets a later run drop what a killed run left behind, and the random part
// keeps parallel runs on one server (several agents, several packages)
// from picking the same name. Older runs used vx_test_<unix nanoseconds>.
const dbPrefix = "vx_test_"

// staleAfter is how old a leftover test database must be before a new run
// drops it. A day is far longer than any test run.
const staleAfter = 24 * time.Hour

// createAttempts bounds the retries when a name is taken.
const createAttempts = 5

// newDBName returns a fresh database name; tests replace it to force a
// collision.
var newDBName = func(now time.Time) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%s%d_%s", dbPrefix, now.Unix(), hex.EncodeToString(b[:]))
}

var (
	newName = regexp.MustCompile(`^vx_test_(\d{9,11})_[0-9a-f]+$`)
	oldName = regexp.MustCompile(`^vx_test_(\d{18,20})$`)
)

// dbCreated returns when a test database was created, from its name, and
// false for names this package did not make.
func dbCreated(name string) (time.Time, bool) {
	if m := newName.FindStringSubmatch(name); m != nil {
		sec, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(sec, 0), true
	}
	if m := oldName.FindStringSubmatch(name); m != nil {
		ns, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(0, ns), true
	}
	return time.Time{}, false
}

var sweepOnce sync.Once

// dropStale drops test databases older than staleAfter, which runs that
// were killed before their cleanup left behind. Failures are logged and
// otherwise ignored: another run may be dropping the same database.
func dropStale(ctx context.Context, t testing.TB, conn *pgx.Conn, now time.Time) []string {
	rows, err := conn.Query(ctx, `select datname from pg_database where datname like 'vx\_test\_%'`)
	if err != nil {
		t.Logf("list leftover test databases: %v", err)
		return nil
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Logf("list leftover test databases: %v", err)
		return nil
	}
	var dropped []string
	for _, name := range names {
		created, ok := dbCreated(name)
		if !ok || now.Sub(created) < staleAfter {
			continue
		}
		if _, err := conn.Exec(ctx, "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			t.Logf("drop leftover test database %s: %v", name, err)
			continue
		}
		dropped = append(dropped, name)
	}
	return dropped
}

// createDatabase creates a database with a fresh name, trying another name
// when one is taken.
func createDatabase(ctx context.Context, conn *pgx.Conn) (string, error) {
	var err error
	for range createAttempts {
		name := newDBName(time.Now())
		if _, err = conn.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err == nil {
			return name, nil
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42P04" { // duplicate_database
			return "", err
		}
	}
	return "", fmt.Errorf("%d names were taken: %w", createAttempts, err)
}

// freshDatabase creates an empty database on the test server, drops it
// when the test ends and returns its URL. The first call of a process also
// drops leftovers older than a day.
func freshDatabase(t testing.TB) string {
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
	sweepOnce.Do(func() { dropStale(ctx, t, conn, time.Now()) })
	name, err := createDatabase(ctx, conn)
	if err != nil {
		conn.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := conn.Exec(ctx, "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)")
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
