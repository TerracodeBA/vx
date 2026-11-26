package cli_test

import (
	"strings"
	"testing"
)

// vx sync warns when merging your edits with the effort's left code that
// can never run, and names file and line; the land of the result says it
// too.
func TestSyncWarnsUnreachableCLI(t *testing.T) {
	_, a, b := boardPair(t)
	const mux = "package web\n\nfunc register(m *Mux) {\n\tm.Handle(\"a\")\n}\n\ntype Mux struct{}\n\nfunc (m *Mux) Handle(string) {}\n"
	a.must("new", "Add the mux")
	a.write("web/mux.go", mux)
	a.must("land")
	b.must("sync")

	b.must("new", "Register c")
	b.write("web/mux.go", strings.Replace(mux, "\tm.Handle(\"a\")\n", "\tm.Handle(\"a\")\n\tm.Handle(\"c\")\n", 1))

	a.must("new", "Register b, then stop")
	a.write("web/mux.go", strings.Replace(mux, "\tm.Handle(\"a\")\n", "\tm.Handle(\"a\")\n\tm.Handle(\"b\")\n\treturn\n", 1))
	a.must("land")

	out := b.must("sync")
	if !strings.Contains(out, "Warning: web/mux.go:7: unreachable code in register after the merge (neither side had it; both changed this package)") ||
		!strings.Contains(out, "together the code no longer compiles cleanly") {
		t.Fatalf("sync:\n%s", out)
	}
	out = b.must("land")
	if !strings.Contains(out, "Landed ") {
		t.Fatalf("land after the warning:\n%s", out)
	}
}
