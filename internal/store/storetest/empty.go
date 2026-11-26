package storetest

import (
	"testing"
)

// NewDatabase creates a fresh, empty database (no migrations applied) and
// returns its URL; it is dropped when the test ends. Tests of data
// migrations use it with store.MigrateTo.
func NewDatabase(t testing.TB) string {
	t.Helper()
	return freshDatabase(t)
}
