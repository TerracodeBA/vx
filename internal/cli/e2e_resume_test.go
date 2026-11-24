package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/cli"
)

// A land that stops on conflicts keeps its evidence flags: after fixing the
// markers, a bare `vx land` runs the same checks instead of landing without
// evidence.
func TestInterruptedLandKeepsEvidence(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	a := h.user("a", aliceToken, work)
	b := h.user("b", aliceToken, work)
	a.must("clone", "svc", "feature", "a")
	b.must("clone", "svc", "feature", "b")
	a.dir, b.dir = filepath.Join(work, "a"), filepath.Join(work, "b")

	a.must("new", "Shared file")
	a.write("shared.txt", "one\n")
	a.must("land")
	b.must("sync")

	a.must("new", "A edits")
	a.write("shared.txt", "from a\n")
	b.must("new", "B edits")
	b.write("shared.txt", "from b\n")
	a.must("land")

	check := "grep -q 'from' shared.txt"
	out, code := b.run("land", "--run", check, "--check", "unit=pass:1")
	if code != cli.ExitConflict {
		t.Fatalf("land should stop on the conflict: %d %s", code, out)
	}
	want := "it reuses your evidence: vx land --run 'grep -q '\\''from'\\'' shared.txt' --check unit=pass:1"
	if !strings.Contains(out, want) {
		t.Fatalf("conflict output should show the saved command:\n%s", out)
	}

	b.write("shared.txt", "from a and b\n")
	out = b.must("land")
	if !strings.Contains(out, "Reusing the evidence of your interrupted land") || !strings.Contains(out, "pass "+check) {
		t.Fatalf("bare land should reuse the checks:\n%s", out)
	}
	if lg := b.must("log", "-n", "1"); !strings.Contains(lg, "run by vx") || !strings.Contains(lg, "check unit: pass (reported") {
		t.Fatalf("log = %s", lg)
	}

	// After a successful land nothing is reused.
	b.must("new", "Next")
	b.write("next.txt", "n\n")
	if out := b.must("land"); strings.Contains(out, "Reusing") {
		t.Fatalf("a finished land's evidence must not carry over:\n%s", out)
	}

	// --fresh ignores saved flags.
	b.must("new", "Failing")
	b.write("f.txt", "f\n")
	if out, code := b.run("land", "--run", "exit 3"); code == 0 || !strings.Contains(out, "checks failed") {
		t.Fatalf("failing check: %d %s", code, out)
	}
	if out := b.must("land", "--fresh"); strings.Contains(out, "Reusing") || strings.Contains(out, "exit 3") {
		t.Fatalf("--fresh should land without the saved run:\n%s", out)
	}
}
