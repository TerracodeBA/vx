package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newEffort makes repo svc with effort "feature" and returns its first
// user's harness and the working directory.
func speedSetup(t *testing.T) (*harness, string, func(label string) *user) {
	t.Helper()
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	clone := func(label string) *user {
		u := h.user(label, aliceToken, work)
		u.must("clone", "svc", "feature", label)
		u.dir = filepath.Join(work, label)
		return u
	}
	return h, work, clone
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// A --run command that passed on exactly the files of this land is not run
// again when the land is retried on the same files: the result is reused
// and says so, here and in the evidence. Other files, or --no-reuse, run
// it again.
func TestLandReusesPassingChecksOnSameFiles(t *testing.T) {
	_, _, clone := speedSetup(t)
	b := clone("b")
	tmp := t.TempDir()
	count, flag := filepath.Join(tmp, "count"), filepath.Join(tmp, "flag")
	counted := "echo run >> " + count
	gate := "test -f " + flag

	b.must("new", "Add x")
	b.write("x.txt", "x\n")
	if out, code := b.run("land", "--run", counted, "--run", gate); code == 0 || !strings.Contains(out, "checks failed") {
		t.Fatalf("the gate should fail: %d\n%s", code, out)
	}
	// --no-reuse runs the passing check again on the same files
	// (--keep-going: the failing gate may now run first, as the cheapest).
	if out, code := b.run("land", "--no-reuse", "--keep-going"); code == 0 || strings.Contains(out, "reused") {
		t.Fatalf("--no-reuse should run everything again: %d\n%s", code, out)
	}
	if n := lineCount(t, count); n != 2 {
		t.Fatalf("the counted check ran %d times, want 2", n)
	}

	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := b.must("land")
	if !strings.Contains(out, "pass "+counted+" (reused from ") || !strings.Contains(out, "passed on these exact files") {
		t.Fatalf("the passing check should be reused:\n%s", out)
	}
	if !strings.Contains(out, "pass "+gate+" (") || strings.Contains(out, "pass "+gate+" (reused") {
		t.Fatalf("the failed check must run again:\n%s", out)
	}
	if n := lineCount(t, count); n != 2 {
		t.Fatalf("the reused check ran again (%d runs)", n)
	}
	if lg := b.must("log", "-n", "1"); !strings.Contains(lg, "check "+counted+": pass (run by vx") || !strings.Contains(lg, "reused from ") {
		t.Fatalf("the evidence should say the result was reused:\n%s", lg)
	}

	// Different files: everything runs.
	b.must("new", "Add y")
	b.write("y.txt", "y\n")
	if out := b.must("land", "--run", counted); strings.Contains(out, "reused") {
		t.Fatalf("a check on other files must run:\n%s", out)
	}
	if n := lineCount(t, count); n != 3 {
		t.Fatalf("the counted check ran %d times, want 3", n)
	}
}

// When someone lands while the --run checks run, vx stops them at once,
// syncs and runs them on the merged files, instead of finishing a run on
// files that will not land.
func TestLandStopsChecksWhenEffortMoves(t *testing.T) {
	_, _, clone := speedSetup(t)
	a, b := clone("a"), clone("b")
	started := filepath.Join(t.TempDir(), "started")
	// Slow until a.txt arrives with a's land.
	check := fmt.Sprintf("test -f a.txt || { touch %s; sleep 60; }", started)

	b.must("new", "Add b")
	b.write("b.txt", "b\n")
	type result struct {
		out  string
		code int
	}
	done := make(chan result, 1)
	begin := time.Now()
	go func() {
		out, code := b.run("land", "--run", check)
		done <- result{out, code}
	}()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the check never started")
		}
	}
	a.must("new", "Add a")
	a.write("a.txt", "a\n")
	a.must("land")

	select {
	case r := <-done:
		if r.code != 0 || !strings.Contains(r.out, "stopped them to sync and run them on the merged files") || !strings.Contains(r.out, "Landed") || strings.Contains(r.out, "merged your changes onto theirs") {
			t.Fatalf("land should stop the checks, sync and land verified: %d\n%s", r.code, r.out)
		}
		if took := time.Since(begin); took > 30*time.Second {
			t.Fatalf("the land took %s: the stale check was not stopped", took)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("the land kept running the check on stale files")
	}
	if got := b.read("a.txt"); got != "a\n" {
		t.Fatalf("b should have a's file: %q", got)
	}
}

// A failed --run command shows the end of its output under the failure,
// and the full output is in .vx/last-check-N.log.
func TestFailedCheckShowsOutput(t *testing.T) {
	_, _, clone := speedSetup(t)
	b := clone("b")
	b.must("new", "Add x")
	b.write("x.txt", "x\n")
	loud := `i=1; while [ $i -le 100 ]; do echo line$i; i=$((i+1)); done; exit 3`
	out, code := b.run("land", "--run", "true", "--run", loud)
	if code == 0 {
		t.Fatalf("land should fail:\n%s", out)
	}
	for _, want := range []string{"FAIL " + loud, "  line71\n", "  line100\n", "Last 30 of 100 lines; full output: " + filepath.Join(".vx", "last-check-2.log")} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "line70\n") {
		t.Errorf("only the last 30 lines belong in the output:\n%s", out)
	}
	if log := b.read(".vx/last-check-2.log"); !strings.HasPrefix(log, "line1\nline2\n") || !strings.HasSuffix(log, "line100\n") {
		t.Fatalf("the log should have the full output:\n%s", log)
	}

	// Short output: all of it, and where the log is.
	out, _ = b.run("land", "--fresh", "--run", "echo bad.go; exit 1")
	if !strings.Contains(out, "  bad.go\n  Full output: "+filepath.Join(".vx", "last-check-1.log")) {
		t.Fatalf("short failure output:\n%s", out)
	}
}

const genRules = `[[generated]]
glob    = "gen/**"
command = "sh gen.sh"
`

// Generated files both sides changed are regenerated with the vx.toml
// command after vx sync and after a land the server merged; with
// --no-generate the warning stays and says what to run.
func TestRegenerateAfterMerge(t *testing.T) {
	_, _, clone := speedSetup(t)
	a := clone("a")
	a.must("new", "Generator")
	a.write("vx.toml", genRules)
	a.write("gen.sh", "mkdir -p gen && cat src/*.txt > gen/all.txt\n")
	a.write("src/a.txt", "a1\n")
	a.write("gen/all.txt", "a1\n")
	a.must("land")
	b := clone("b")

	a.must("new", "A2")
	a.write("src/a.txt", "a2\n")
	a.write("gen/all.txt", "a2\n")
	a.must("land")

	b.must("new", "B1", "--no-sync")
	b.write("src/b.txt", "b1\n")
	b.write("gen/all.txt", "a1\nb1\n")
	out := b.must("sync")
	if !strings.Contains(out, "Regenerated gen/all.txt: ran `sh gen.sh`") || strings.Contains(out, "Warning: generated file") {
		t.Fatalf("sync should regenerate:\n%s", out)
	}
	if got := b.read("gen/all.txt"); got != "a2\nb1\n" {
		t.Fatalf("gen/all.txt = %q, want the merged sources", got)
	}

	// --no-generate keeps today's warning and says what to run.
	a.must("new", "A3")
	a.write("src/a.txt", "a3\n")
	a.write("gen/all.txt", "a3\n")
	a.must("land")
	out = b.must("sync", "--no-generate")
	for _, want := range []string{"Warning: generated file gen/all.txt: regenerate with `sh gen.sh`", "Not regenerated: --no-generate. Run `sh gen.sh`"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync --no-generate lacks %q:\n%s", want, out)
		}
	}
	if got := b.read("gen/all.txt"); got != "a2\nb1\n" {
		t.Fatalf("--no-generate must keep the local file: %q", got)
	}
	b.must("land")

	// A land the server merged: the landed change has a's generated file,
	// and a's working copy regenerates it as a change to land next.
	a.must("new", "A4", "--no-sync")
	a.write("src/a.txt", "a4\n")
	a.write("gen/all.txt", "a4\n")
	out = a.must("land")
	if !strings.Contains(out, "merged your changes onto theirs") || !strings.Contains(out, "Regenerated gen/all.txt: ran `sh gen.sh`") || !strings.Contains(out, "land the regenerated ones") {
		t.Fatalf("a merged land should regenerate:\n%s", out)
	}
	if got := a.read("gen/all.txt"); got != "a4\nb1\n" {
		t.Fatalf("gen/all.txt = %q after the merged land", got)
	}
	if st := a.must("status"); !strings.Contains(st, "M gen/all.txt") {
		t.Fatalf("the regenerated file should be a local change:\n%s", st)
	}
}

// vx new syncs to the effort head first, in one line; --no-sync does not.
func TestNewSyncsFirst(t *testing.T) {
	_, _, clone := speedSetup(t)
	a, b := clone("a"), clone("b")
	a.must("new", "A")
	a.write("a.txt", "a\n")
	a.must("land")

	out := b.must("new", "B")
	if !strings.Contains(out, "Synced to ") || !strings.Contains(out, "1 file(s) updated") || b.read("a.txt") != "a\n" {
		t.Fatalf("vx new should sync first:\n%s", out)
	}
	b.must("abandon")
	if out := b.must("new", "B again"); !strings.Contains(out, "Up to date at ") {
		t.Fatalf("vx new on the head says so:\n%s", out)
	}
	b.must("abandon")

	a.must("new", "A2")
	a.write("a2.txt", "a\n")
	a.must("land")
	if out := b.must("new", "B3", "--no-sync"); strings.Contains(out, "Synced") || strings.Contains(out, "Up to date") {
		t.Fatalf("--no-sync must not sync:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "a2.txt")); err == nil {
		t.Fatal("--no-sync brought in a2.txt")
	}
}

// Local changes without a workspace: status, sync and land say how to
// open one; -m on an open workspace replaces its intent.
func TestLocalChangesWithoutWorkspace(t *testing.T) {
	_, _, clone := speedSetup(t)
	b := clone("b")
	b.write("x.txt", "x\n")
	hint := `You have 1 local change(s) but no workspace: vx new "INTENT" (or vx land -m "INTENT")`

	st := b.must("status")
	if !strings.Contains(st, hint) || strings.Contains(st, "No workspace open") || strings.Contains(st, "ready to land.") {
		t.Fatalf("status:\n%s", st)
	}
	if out := b.must("sync"); !strings.Contains(out, hint) {
		t.Fatalf("sync should warn:\n%s", out)
	}
	if out, code := b.run("land"); code == 0 || !strings.Contains(out, hint) {
		t.Fatalf("land should refuse with the hint: %d\n%s", code, out)
	}

	b.must("new", "First idea")
	out := b.must("land", "-m", "Add x.txt")
	if !strings.Contains(out, `is now "Add x.txt" (was "First idea")`) || !strings.Contains(out, "Add x.txt") {
		t.Fatalf("-m should replace the intent:\n%s", out)
	}
	if lg := b.must("log", "-n", "1"); !strings.Contains(lg, "Add x.txt") {
		t.Fatalf("log:\n%s", lg)
	}
}

// A rebase that stops on a source conflict regenerates the generated file
// at --continue, once the sources are resolved, before uploading.
func TestRebaseContinueRegenerates(t *testing.T) {
	up := newGitUpstream(t)
	up.commit("Initial", map[string]string{
		"vx.toml":     genRules,
		"gen.sh":      "mkdir -p gen && cp api.proto gen/all.txt\n",
		"api.proto":   "message Effort {\n  string slug = 1;\n}\n",
		"gen/all.txt": "message Effort {\n  string slug = 1;\n}\n",
		"README.md":   "# shop\n",
	})
	h, aliceToken := newHarnessWith(t, true)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop", "--git", up.dir)
	alice.must("effort", "new", "Effort A", "--repo", "shop")
	a := h.user("a", aliceToken, work)
	a.must("clone", "shop", "effort-a", "a")
	a.dir = filepath.Join(work, "a")

	a.must("new", "Comment slug")
	a.write("api.proto", "message Effort {\n  string slug = 1; // unique\n}\n")
	a.write("gen/all.txt", "message Effort {\n  string slug = 1; // unique\n}\n")
	a.must("land")
	up.commit("Rename slug", map[string]string{
		"api.proto":   "message Effort {\n  string name = 1;\n}\n",
		"gen/all.txt": "message Effort {\n  string name = 1;\n}\n",
	})

	out, code := a.run("effort", "rebase")
	if code != 2 || !strings.Contains(out, "vx effort rebase --continue regenerates gen/all.txt with `sh gen.sh` once the conflict markers are gone.") {
		t.Fatalf("rebase should stop and say it regenerates later: %d\n%s", code, out)
	}
	resolved := "message Effort {\n  string name = 1; // unique\n}\n"
	a.write("api.proto", resolved)
	out = a.must("effort", "rebase", "--continue")
	if !strings.Contains(out, "Regenerated gen/all.txt: ran `sh gen.sh`") || !strings.Contains(out, "Rebased effort effort-a") {
		t.Fatalf("continue should regenerate first:\n%s", out)
	}
	if got := a.read("gen/all.txt"); got != resolved {
		t.Fatalf("gen/all.txt = %q, want it regenerated from the resolved proto", got)
	}
	if st := a.must("status"); !strings.Contains(st, "No local changes.") {
		t.Fatalf("the regenerated file should be in the rebased head:\n%s", st)
	}
}
