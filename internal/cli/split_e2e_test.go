package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// vx ship plan/open --split against a local git upstream: the parts, a
// person's grouping with --group, the refusal (exit 2), and one pushed
// branch per part, stacked on each other.
func TestShipSplitCLI(t *testing.T) {
	up := newGitUpstream(t)
	main := up.commit("Initial", map[string]string{"README.md": "# shop\n"})
	h, aliceToken := newHarnessWith(t, true)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop", "--git", up.dir)
	alice.must("effort", "new", "Rate limiting", "--repo", "shop")
	alice.must("clone", "shop", "rate-limiting", "rl")
	alice.dir = work + "/rl"

	land := func(intent string, files map[string]string) string {
		t.Helper()
		alice.must("new", intent)
		for p, c := range files {
			alice.write(p, c)
		}
		alice.must("land", "--run", "true")
		var l struct {
			Changesets []struct {
				ChangeID string `json:"change_id"`
			} `json:"changesets"`
		}
		if err := json.Unmarshal([]byte(alice.must("log", "--json")), &l); err != nil {
			t.Fatal(err)
		}
		return l.Changesets[0].ChangeID
	}
	a := land("Add limiter", map[string]string{"limit.go": "package shop\n\nfunc limit() {}\n"})
	b := land("Document the limiter", map[string]string{"README.md": "# shop\n\nLimited.\n"})
	c := land("Limit per IP", map[string]string{"limit.go": "package shop\n\nfunc limit(ip string) {}\n"})

	out := alice.must("ship", "plan", "--split", "independent")
	for _, want := range []string{
		"as 2 pull requests, independent: they merge in any order (grouping suggested by vx)",
		"Part 1: Add limiter (+1 more)", "vx/rate-limiting/1-add-limiter -> main", "files: limit.go",
		"together: " + a + " and " + c + " both change limit.go",
		"Part 2: Document the limiter", "**Part 2 of 2** of vx effort `rate-limiting`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan --split independent lacks %q:\n%s", want, out)
		}
	}
	var js struct {
		Plan struct {
			Split  string `json:"split"`
			Groups []struct {
				Branch string `json:"branch"`
				Base   string `json:"base"`
				Steps  []any  `json:"steps"`
			} `json:"groups"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(alice.must("ship", "plan", "--split", "stacked", "--json")), &js); err != nil {
		t.Fatal(err)
	}
	if js.Plan.Split != "stacked" || len(js.Plan.Groups) != 3 || js.Plan.Groups[2].Base != "vx/rate-limiting/2-document-the-limiter" {
		t.Fatalf("plan --json: %+v", js.Plan)
	}

	// A person's stack that puts "Limit per IP" below the limiter: refused.
	out, code := alice.run("ship", "plan", "--split", "stacked", "--group", c, "--group", a+","+b)
	if code != 2 || !strings.Contains(out, "This split cannot ship") || !strings.Contains(out, "builds on lines that change "+a) {
		t.Fatalf("refused stack: exit %d\n%s", code, out)
	}
	if out, code := alice.run("ship", "open", "--split", "stacked", "--group", c, "--group", a+","+b); code == 0 || !strings.Contains(out, "this split cannot ship") {
		t.Fatalf("open the refused stack: exit %d\n%s", code, out)
	}
	if out, code := alice.run("ship", "plan", "--group", a); code == 0 || !strings.Contains(out, "--group needs --split") {
		t.Fatalf("--group without --split: exit %d\n%s", code, out)
	}

	out = alice.must("ship", "open", "--split", "stacked", "--group", a+","+c, "--group", b)
	for _, want := range []string{"Part 1: pushed vx/rate-limiting/1-add-limiter (2 commit(s)) for main; this host has no pull requests.",
		"Part 2: pushed vx/rate-limiting/2-document-the-limiter (1 commit(s)) for vx/rate-limiting/1-add-limiter"} {
		if !strings.Contains(out, want) {
			t.Errorf("ship open lacks %q:\n%s", want, out)
		}
	}
	r, err := git.PlainOpen(up.dir)
	if err != nil {
		t.Fatal(err)
	}
	ref1, err1 := r.Reference(plumbing.NewBranchReferenceName("vx/rate-limiting/1-add-limiter"), true)
	ref2, err2 := r.Reference(plumbing.NewBranchReferenceName("vx/rate-limiting/2-document-the-limiter"), true)
	if err1 != nil || err2 != nil {
		t.Fatalf("branches: %v %v", err1, err2)
	}
	top, _ := r.CommitObject(ref2.Hash())
	if top.ParentHashes[0] != ref1.Hash() || !strings.Contains(top.Message, "Vx-Change-Id: "+b) {
		t.Fatalf("part 2 does not sit on part 1:\n%s", top.Message)
	}
	low, _ := r.CommitObject(ref1.Hash())
	first, _ := low.Parent(0)
	if first.ParentHashes[0].String() != main {
		t.Fatal("part 1 does not sit on main")
	}
	list := alice.must("ship", "list")
	if !strings.Contains(list, "part 1, stacked") || !strings.Contains(list, "part 2, stacked") {
		t.Fatalf("ship list:\n%s", list)
	}
	// One pull request over the split is refused.
	if out, code := alice.run("ship", "open"); code == 0 || !strings.Contains(out, "vx ship open --split stacked") {
		t.Fatalf("ship open over a split: exit %d\n%s", code, out)
	}
	// Shipping again keeps the parts (vx suggests the earlier grouping).
	out = alice.must("ship", "plan", "--split", "stacked")
	if !strings.Contains(out, "as 2 pull requests") || !strings.Contains(out, "Part 1: Add limiter (+1 more)") {
		t.Fatalf("plan after shipping:\n%s", out)
	}
}
