package cli_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Two agents add a different section after the same Markdown heading and a
// different function at the end of the same Go file. Both land without a
// conflict; the merge keeps both (what landed first comes first) and every
// path that merges says which files kept both.
func TestKeepBothInsertions(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop-api")
	alice.must("effort", "new", "Docs", "--repo", "shop-api")
	tokenOf := func(name string) string {
		var resp struct{ Token string }
		if err := json.Unmarshal([]byte(alice.must("agent", "add", name, "--json")), &resp); err != nil || resp.Token == "" {
			t.Fatalf("agent add %s: %v", name, err)
		}
		return resp.Token
	}
	a := h.user("a", tokenOf("a"), work)
	b := h.user("b", tokenOf("b"), work)
	a.must("clone", "shop-api", "docs", "a")
	b.must("clone", "shop-api", "docs", "b")
	a.dir, b.dir = filepath.Join(work, "a"), filepath.Join(work, "b")

	const guide = "# Guide\n\n## Install\n\nRun make.\n"
	const tools = "package tools\n\nfunc A() {\n}\n"
	a.must("new", "Seed")
	a.write("guide.md", guide)
	a.write("tools.go", tools)
	a.write("other.txt", "x\n")
	a.must("land", "--check", "lint=pass")
	b.must("sync")

	// Round 1: the server merges b's land onto a's.
	a.must("new", "Alpha")
	a.write("guide.md", "# Guide\n\n## Alpha\n\nAlpha text.\n\n## Install\n\nRun make.\n")
	a.write("tools.go", tools+"\nfunc Alpha() {\n}\n")
	a.must("land", "--check", "lint=pass")
	b.must("new", "Beta")
	b.write("guide.md", "# Guide\n\n## Beta\n\nBeta text.\n\n## Install\n\nRun make.\n")
	b.write("tools.go", tools+"\nfunc Beta() {\n}\n")
	b.write("other.txt", "x\ny\n")
	out := b.must("land", "--check", "lint=pass")
	if !strings.Contains(out, "Kept both insertions (check the order): guide.md, tools.go\n") {
		t.Fatalf("land = %q", out)
	}
	wantGuide := "# Guide\n\n## Alpha\n\nAlpha text.\n\n## Beta\n\nBeta text.\n\n## Install\n\nRun make.\n"
	wantTools := tools + "\nfunc Alpha() {\n}\n\nfunc Beta() {\n}\n"
	if got := b.read("guide.md"); got != wantGuide {
		t.Fatalf("guide.md = %q", got)
	}
	if got := b.read("tools.go"); got != wantTools {
		t.Fatalf("tools.go = %q", got)
	}

	// Round 2: vx sync merges locally and lists the file, in --json too.
	a.must("sync")
	a.must("new", "Gamma")
	a.write("tools.go", wantTools+"\nfunc Gamma() {\n}\n")
	a.must("land", "--check", "lint=pass")
	b.must("new", "Delta")
	b.write("tools.go", wantTools+"\nfunc Delta() {\n}\n")
	var res struct {
		Merged    []string
		KeptBoth  []string `json:"kept_both"`
		Conflicts []struct{ Path string }
	}
	raw := b.must("sync", "--json")
	if err := json.Unmarshal([]byte(raw), &res); err != nil || !reflect.DeepEqual(res.KeptBoth, []string{"tools.go"}) || len(res.Conflicts) != 0 {
		t.Fatalf("sync --json = %s, %v", raw, err)
	}
	if got := b.read("tools.go"); got != wantTools+"\nfunc Gamma() {\n}\n\nfunc Delta() {\n}\n" {
		t.Fatalf("synced tools.go = %q", got)
	}
	b.must("land", "--check", "lint=pass")

	// Round 3: land --run syncs first; the land output still lists the file.
	a.must("sync")
	a.must("new", "Epsilon")
	a.write("guide.md", strings.Replace(wantGuide, "## Install\n", "## Epsilon\n\n## Install\n", 1))
	a.must("land", "--check", "lint=pass")
	b.must("new", "Zeta")
	b.write("guide.md", strings.Replace(wantGuide, "## Install\n", "## Zeta\n\n## Install\n", 1))
	out = b.must("land", "--run", "true")
	if !strings.Contains(out, "Kept both insertions (check the order): guide.md\n") {
		t.Fatalf("land --run = %q", out)
	}
	if got := b.read("guide.md"); !strings.Contains(got, "## Epsilon\n\n## Zeta\n\n## Install\n") {
		t.Fatalf("guide.md after land --run = %q", got)
	}

	// The same line changed differently still conflicts.
	a.must("sync")
	a.must("new", "Rename A")
	a.write("other.txt", "x\nya\n")
	a.must("land", "--check", "lint=pass")
	b.must("new", "Rename B")
	b.write("other.txt", "x\nyb\n")
	if out, code := b.run("land", "--check", "lint=pass"); code != 2 || strings.Contains(out, "Kept both") {
		t.Fatalf("same-line land: code %d, %q", code, out)
	}
}
