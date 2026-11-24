package cli_test

import (
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// An effort whose shipped commits reach main is marked merged when vx next
// imports main; it then leaves the lists and takes no more lands. Efforts
// that will not ship are closed by hand.
func TestEffortMergedWhenMainHasItsChanges(t *testing.T) {
	up := newGitUpstream(t)
	up.commit("Initial", map[string]string{"README.md": "shop\n"})
	h, aliceToken := newHarnessWith(t, true)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop", "--git", up.dir)
	alice.must("effort", "new", "Effort A", "--repo", "shop")
	alice.must("effort", "new", "Effort B", "--repo", "shop")
	alice.must("clone", "shop", "effort-a", "a")
	alice.dir = filepath.Join(work, "a")
	alice.must("new", "Add limiter")
	alice.write("limit.go", "package shop\n")
	alice.must("land")
	alice.must("new", "Add audit")
	alice.write("audit.go", "package shop\n")
	alice.must("land")

	// Shipped but not merged yet: still open.
	alice.must("ship", "open")
	alice.must("repo", "sync", "--repo", "shop")
	if out := alice.must("effort", "list", "--repo", "shop"); !strings.Contains(out, "effort-a") || strings.Contains(out, "hidden") {
		t.Fatalf("a shipped, unmerged effort must stay open:\n%s", out)
	}

	// The host merges the pull request (a fast-forward keeps the commits and
	// their Vx-Change-Id trailers, like a rebase merge).
	r, err := git.PlainOpen(up.dir)
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := r.Reference(plumbing.NewBranchReferenceName("vx/effort-a"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), shipped.Hash())); err != nil {
		t.Fatal(err)
	}
	alice.must("repo", "sync", "--repo", "shop")

	out := alice.must("effort", "list", "--repo", "shop")
	if strings.Contains(out, "effort-a") || !strings.Contains(out, "effort-b") || !strings.Contains(out, "1 finished effort(s) hidden") {
		t.Fatalf("effort list after the merge:\n%s", out)
	}
	if out := alice.must("effort", "list", "--repo", "shop", "--all"); !strings.Contains(out, "effort-a") || !strings.Contains(out, "merged") {
		t.Fatalf("effort list --all:\n%s", out)
	}
	if out, code := alice.run("new", "Too late"); code == 0 || !strings.Contains(strings.ToLower(out), "merged") {
		t.Fatalf("new work in a merged effort must be refused: exit %d\n%s", code, out)
	}

	// Closing by hand: abandon effort B; closing it twice is fine, changing
	// its mind is not.
	if out := alice.must("effort", "close", "effort-b", "--abandon", "--repo", "shop"); !strings.Contains(out, "is abandoned") {
		t.Fatalf("close --abandon:\n%s", out)
	}
	alice.must("effort", "close", "effort-b", "--abandon", "--repo", "shop")
	if out, code := alice.run("effort", "close", "effort-b", "--merged", "--repo", "shop"); code == 0 || !strings.Contains(out, "already abandoned") {
		t.Fatalf("close --merged after abandon: exit %d\n%s", code, out)
	}
	if out, code := alice.run("effort", "close", "effort-b", "--repo", "shop"); code == 0 || !strings.Contains(out, "--merged or --abandon") {
		t.Fatalf("close without a state: exit %d\n%s", code, out)
	}
	if out := alice.must("effort", "list", "--repo", "shop"); !strings.Contains(out, "2 finished effort(s) hidden") {
		t.Fatalf("both efforts finished:\n%s", out)
	}
}
