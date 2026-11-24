package cli_test

import (
	"strings"
	"testing"
)

// vx context names who is changing the files asked about first, and keeps
// the rest of the effort apart, with unknown files said as such.
func TestContextWorkingOnFilesFirst(t *testing.T) {
	alice, a, b := boardPair(t)
	a.must("new", "Change x")
	a.write("x.go", "package x\n")
	a.must("status") // reports x.go
	b.must("new", "Something else")
	out := alice.must("context", "x.go", "--repo", "shop-api", "--effort", "rate-limiting")
	on, rest := strings.Index(out, "Working on these now (1):"), strings.Index(out, "Others working in this effort, not known to touch these (1):")
	if on < 0 || rest < on || !strings.Contains(out[on:rest], "agent:alice/backend") || !strings.Contains(out[on:rest], "changing x.go") ||
		!strings.Contains(out[rest:], "agent:bob/audit") || !strings.Contains(out[rest:], "in this effort, files unknown") {
		t.Fatalf("context:\n%s", out)
	}
}
