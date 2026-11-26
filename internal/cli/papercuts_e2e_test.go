package cli_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentPapercuts covers small things agents asked for (#48): vx
// --version, vx log --effort for another effort, and vx context without
// a target for the whole effort.
func TestAgentPapercuts(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)

	version := alice.must("version")
	if flag := alice.must("--version"); flag != version || !strings.HasPrefix(flag, "vx ") {
		t.Fatalf("vx --version %q, vx version %q", flag, version)
	}

	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	alice.must("effort", "new", "Other", "--repo", "svc")
	alice.must("clone", "svc", "feature", "a")
	alice.dir = filepath.Join(work, "a")
	alice.must("new", "Add limiter")
	alice.write("limit.go", "package svc\n")
	alice.must("land")

	// vx log --effort reads another effort of the repository.
	if out := alice.must("log", "--effort", "other"); !strings.Contains(out, "Nothing landed in other yet.") {
		t.Fatalf("log --effort other:\n%s", out)
	}
	if out := alice.must("log", "--effort", "feature", "--files"); !strings.Contains(out, "Add limiter") || !strings.Contains(out, "limit.go") {
		t.Fatalf("log --effort feature:\n%s", out)
	}
	if out, code := alice.run("log", "other", "--effort", "feature"); code == 0 || !strings.Contains(out, "two efforts given") {
		t.Fatalf("log with two efforts: exit %d\n%s", code, out)
	}

	// Several targets after one --on are not taken for the note's text.
	if out, code := alice.run("post", "plan", "Limiter next", "--on", "limit.go", "other.go"); code == 0 || !strings.Contains(out, "--on a.go,b.go") {
		t.Fatalf("post with a stray target: exit %d\n%s", code, out)
	}

	// vx context without targets covers the whole effort.
	alice.must("post", "plan", "Limiter next", "--on", "limit.go")
	alice.must("new", "Tune limiter")
	out := alice.must("context")
	for _, want := range []string{"Context for the whole effort feature", "Limiter next", "Recent changes (", "Add limiter", "Working in this effort now (1):", "Tune limiter"} {
		if !strings.Contains(out, want) {
			t.Fatalf("context without targets lacks %q:\n%s", want, out)
		}
	}
}
