package cli_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/cli"
)

// lastLine is the last non-empty line of out.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1]
}

// vx land refuses positional arguments instead of dropping them, and ends
// with one result line, after the board feed too.
func TestLandResultLine(t *testing.T) {
	_, a, b := boardPair(t)
	a.must("new", "Add x")
	a.write("x.go", "package x\n\nfunc X() int { return 1 }\n")
	out, code := a.run("land", "Add x with a better intent")
	if code == 0 || !strings.Contains(out, `did you mean -m "Add x with a better intent"`) {
		t.Fatalf("land with an argument = %d %q", code, out)
	}
	if out := a.must("log"); strings.Contains(out, "Add x") {
		t.Fatalf("the refused land landed:\n%s", out)
	}

	// The board feed prints after the file list; the result line is last.
	b.must("post", "decision", "x.go returns ints", "--on", "x.go")
	out = a.must("land")
	if !strings.Contains(out, "New on the board") || !strings.HasPrefix(lastLine(out), "Landed ") || !strings.HasSuffix(lastLine(out), ": Add x") {
		t.Fatalf("land does not end with its result:\n%s", out)
	}

	// Conflicts: the last line says so and gives the exit code.
	a.must("new", "X returns 2")
	a.write("x.go", "package x\n\nfunc X() int { return 2 }\n")
	b.must("sync")
	b.must("new", "X returns 3")
	b.write("x.go", "package x\n\nfunc X() int { return 3 }\n")
	a.must("land")
	out, code = b.run("land")
	if code != cli.ExitConflict || lastLine(out) != "Not landed: conflicts in 1 file(s) (exit 2)" {
		t.Fatalf("conflicting land = %d:\n%s", code, out)
	}
}

// vx wait exits 4 on a timeout, so `vx wait && vx sync` can tell it from
// an event.
func TestWaitTimeoutExitCode(t *testing.T) {
	_, a, _ := boardPair(t)
	out, code := a.run("wait", "--timeout", "200ms", "--interval", "50ms")
	if code != cli.ExitTimeout || cli.ExitTimeout != 4 || !strings.Contains(out, "timed out") {
		t.Fatalf("wait timeout = %s", fmt.Sprintf("exit %d: %s", code, out))
	}
}
