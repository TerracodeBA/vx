package cli_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// --new-test names must be test funcs in the _test.go files a land adds or
// changes; --run notes Go test packages that came from the test cache.
func TestLandVerifiesNewTestsAndNotesTestCache(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	a := h.user("a", aliceToken, work)
	a.must("clone", "svc", "feature", "a")
	a.dir = filepath.Join(work, "a")

	a.must("new", "Add limiter")
	a.write("limit/limit.go", "package limit\n\nfunc Allow() bool { return true }\n")
	a.write("limit/limit_test.go", "package limit\n\nimport \"testing\"\n\nfunc TestAllow(t *testing.T) {}\n\nfunc TestDeny(t *testing.T) {}\n\nfunc helper() {}\n")

	// A name that is not in the changed test files refuses the land.
	out, code := a.run("land", "--new-test", "TestAllow,TestBurst")
	if code == 0 || !strings.Contains(out, "--new-test TestBurst: no such test func in the _test.go files this land adds or changes (limit/limit_test.go)") ||
		!strings.Contains(out, "limit/limit_test.go") || !strings.Contains(out, "Test functions in those files: TestAllow, TestDeny.") ||
		!strings.Contains(out, "--allow-unverified-tests") {
		t.Fatalf("unknown --new-test should refuse the land: %d %s", code, out)
	}
	if out := a.must("log"); strings.Contains(out, "Add limiter") {
		t.Fatalf("refused land should land nothing: %s", out)
	}

	// Real names land, comma-separated or repeated, and the --run output's
	// cached packages are noted with a hint.
	run := `printf 'ok  \texample.com/a\t(cached)\nok  \texample.com/b\t0.1s\n?   \texample.com/c\t[no test files]\n'`
	out = a.must("land", "--new-test", "TestAllow,TestDeny", "--new-test", "TestAllow", "--run", run)
	for _, want := range []string{"1 of 2 packages from the Go test cache", "Hint: add -count=1 to run every test instead of using cached results.", "New tests: TestAllow, TestDeny"} {
		if !strings.Contains(out, want) {
			t.Fatalf("land output lacks %q:\n%s", want, out)
		}
	}
	logOut := a.must("log", "-n", "1")
	if !strings.Contains(logOut, "run by vx") || !strings.Contains(logOut, ", 1 of 2 packages from the Go test cache)") ||
		!strings.Contains(logOut, "new tests: TestAllow, TestDeny") {
		t.Fatalf("log = %s", logOut)
	}
	var lj struct {
		Changesets []struct {
			Evidence struct {
				Checks []struct {
					Notes string `json:"notes"`
				} `json:"checks"`
			} `json:"evidence"`
		} `json:"changesets"`
	}
	if err := json.Unmarshal([]byte(a.must("log", "--json", "-n", "1")), &lj); err != nil {
		t.Fatal(err)
	}
	if len(lj.Changesets) != 1 || len(lj.Changesets[0].Evidence.Checks) != 1 || lj.Changesets[0].Evidence.Checks[0].Notes != "1 of 2 packages from the Go test cache" {
		t.Fatalf("log --json = %+v", lj)
	}

	// A fresh run has no note and no hint.
	a.must("new", "Deny by default")
	a.write("limit/limit.go", "package limit\n\nfunc Allow() bool { return false }\n")
	out = a.must("land", "--run", `printf 'ok  \texample.com/a\t0.2s\n'`)
	if strings.Contains(out, "Go test cache") || strings.Contains(out, "Hint:") || strings.Contains(out, "New tests") {
		t.Fatalf("fresh run should not note the cache: %s", out)
	}

	// No changed test files at all: refused, unless --allow-unverified-tests.
	a.must("new", "Docs")
	a.write("README.md", "limit\n")
	out, code = a.run("land", "--new-test", "TestAllow")
	if code == 0 || !strings.Contains(out, "this land adds or changes no _test.go files") {
		t.Fatalf("--new-test without test files should refuse: %d %s", code, out)
	}
	out = a.must("land", "--new-test", "TestAllow", "--allow-unverified-tests")
	if !strings.Contains(out, "Landed") || !strings.Contains(out, "New tests: TestAllow") {
		t.Fatalf("--allow-unverified-tests should land: %s", out)
	}
}
