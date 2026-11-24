package cli_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// vx sync names the files that both sides changed and that merged cleanly,
// and vx status points at vx land --run as the other way to sync.
func TestSyncListsMergedFiles(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop-api")
	alice.must("effort", "new", "Styles", "--repo", "shop-api")
	tokenOf := func(name string) string {
		var resp struct{ Token string }
		if err := json.Unmarshal([]byte(alice.must("agent", "add", name, "--json")), &resp); err != nil || resp.Token == "" {
			t.Fatalf("agent add %s: %v", name, err)
		}
		return resp.Token
	}
	a := h.user("a", tokenOf("a"), work)
	b := h.user("b", tokenOf("b"), work)
	a.must("clone", "shop-api", "styles", "a")
	b.must("clone", "shop-api", "styles", "b")
	a.dir, b.dir = filepath.Join(work, "a"), filepath.Join(work, "b")

	a.must("new", "Base files")
	a.write("static/vx.css", "a {}\n\nb {}\n\nc {}\n")
	a.write("static/vx.js", "one()\n\ntwo()\n")
	a.must("land", "--check", "lint=pass")
	b.must("sync")

	// A changes the top of both files and adds one; B changes the bottom of
	// both, and one more file A never touches.
	a.must("new", "Top")
	a.write("static/vx.css", "a { color: red }\n\nb {}\n\nc {}\n")
	a.write("static/vx.js", "one(1)\n\ntwo()\n")
	a.write("static/new.txt", "new\n")
	a.must("land", "--check", "lint=pass")
	b.must("new", "Bottom")
	b.write("static/vx.css", "a {}\n\nb {}\n\nc { color: blue }\n")
	b.write("static/vx.js", "one()\n\ntwo(2)\n")
	b.write("README", "b\n")

	out := b.must("status")
	if !strings.Contains(out, "The effort moved on") || !strings.Contains(out, "Run `vx sync`, or `vx land --run` syncs for you.") {
		t.Fatalf("status = %q", out)
	}
	out = b.must("sync")
	if !strings.Contains(out, "Merged with your edits:\n  static/vx.css\n  static/vx.js\n") || strings.Contains(out, "new.txt") || strings.Contains(out, "README") {
		t.Fatalf("sync = %q", out)
	}
	if got := b.read("static/vx.css"); got != "a { color: red }\n\nb {}\n\nc { color: blue }\n" {
		t.Fatalf("merged css = %q", got)
	}

	// One merged file is said on one line, and --json lists them.
	a.must("sync")
	a.must("new", "Top again")
	a.write("static/vx.css", "a { color: green }\n\nb {}\n\nc {}\n")
	a.must("land", "--check", "lint=pass")
	b.write("static/vx.css", "a { color: red }\n\nb {}\n\nc { color: navy }\n")
	var res struct {
		Merged    []string
		Conflicts []struct{ Path string }
	}
	raw := b.must("sync", "--json")
	if err := json.Unmarshal([]byte(raw), &res); err != nil || !reflect.DeepEqual(res.Merged, []string{"static/vx.css"}) {
		t.Fatalf("sync --json = %s, %v", raw, err)
	}

	// Conflicts are not reported as merged; an up-to-date sync merges nothing.
	a.must("sync")
	a.must("new", "Bottom clash")
	a.write("static/vx.js", "one(1)\n\ntwo(3)\n")
	a.must("land", "--check", "lint=pass")
	b.write("static/vx.js", "one(1)\n\ntwo(4)\n")
	raw, _ = b.run("sync", "--json")
	res.Merged = nil
	if err := json.Unmarshal([]byte(raw[:strings.LastIndex(raw, "}")+1]), &res); err != nil || len(res.Merged) != 0 || len(res.Conflicts) != 1 {
		t.Fatalf("sync with a conflict = %s, %v", raw, err)
	}
	if out := a.must("sync"); strings.Contains(out, "Merged with your edits") {
		t.Fatalf("up-to-date sync = %q", out)
	}
}
