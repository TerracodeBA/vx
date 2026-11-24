package cli_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Two agents change different declarations of one Go file so that the
// lines conflict: the land (on the server) and the sync (locally) merge it
// by declaration and say so, in --json too. Then one agent changes a
// signature while the other adds a caller with the old shape in another
// package: the land warns.
func TestMergeByDeclarationAndSignatureWarnings(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop-api")
	alice.must("effort", "new", "Auth", "--repo", "shop-api")
	tokenOf := func(name string) string {
		var resp struct{ Token string }
		if err := json.Unmarshal([]byte(alice.must("agent", "add", name, "--json")), &resp); err != nil || resp.Token == "" {
			t.Fatalf("agent add %s: %v", name, err)
		}
		return resp.Token
	}
	a := h.user("a", tokenOf("a"), work)
	b := h.user("b", tokenOf("b"), work)
	a.must("clone", "shop-api", "auth", "a")
	b.must("clone", "shop-api", "auth", "b")
	a.dir, b.dir = filepath.Join(work, "a"), filepath.Join(work, "b")

	const auth = "package auth\n\n// Login signs in.\nfunc Login(user string) error {\n\treturn nil\n}\n\n// Logout signs out.\nfunc Logout() {\n}\n"
	a.must("new", "Seed")
	a.write("go.mod", "module example.com/shop\n\ngo 1.22\n")
	a.write("auth/auth.go", auth)
	a.must("land", "--check", "lint=pass")
	b.must("sync")

	// Round 1, on the server: a deletes Login, b documents Logout right
	// below it.
	a.must("new", "Drop Login")
	a.write("auth/auth.go", "package auth\n\n// Logout signs out.\nfunc Logout() {\n}\n")
	a.must("land", "--check", "lint=pass")
	b.must("new", "Document Logout", "--no-sync")
	b.write("auth/auth.go", strings.Replace(auth, "// Logout signs out.", "// Logout signs the user out\n// on every device.", 1))
	out := b.must("land", "--check", "lint=pass")
	if !strings.Contains(out, "Merged by declaration (the lines conflicted, the Go declarations did not; check the result): auth/auth.go\n") {
		t.Fatalf("land = %q", out)
	}
	want := "package auth\n\n// Logout signs the user out\n// on every device.\nfunc Logout() {\n}\n"
	if got := b.read("auth/auth.go"); got != want {
		t.Fatalf("auth/auth.go = %q", got)
	}

	// Round 2, locally: vx sync merges by declaration and lists the file.
	a.must("sync")
	a.must("new", "Add Refresh")
	a.write("auth/auth.go", "package auth\n\n// Refresh renews a session.\nfunc Refresh() {\n}\n\n"+strings.TrimPrefix(want, "package auth\n\n"))
	a.must("land", "--check", "lint=pass")
	b.must("new", "Logout clears", "--no-sync")
	b.write("auth/auth.go", strings.Replace(want, "// Logout signs the user out\n", "// Logout signs the user out, clearing cookies,\n", 1))
	var res struct {
		MergedByDecl []string `json:"merged_by_decl"`
		Conflicts    []struct{ Path string }
	}
	raw := b.must("sync", "--json")
	if err := json.Unmarshal([]byte(raw), &res); err != nil || !reflect.DeepEqual(res.MergedByDecl, []string{"auth/auth.go"}) || len(res.Conflicts) != 0 {
		t.Fatalf("sync --json = %s, %v", raw, err)
	}
	if got := b.read("auth/auth.go"); !strings.Contains(got, "func Refresh()") || !strings.Contains(got, "clearing cookies") {
		t.Fatalf("synced auth/auth.go = %q", got)
	}
	b.must("land", "--check", "lint=pass")

	// Round 3: a changes Logout's signature, b adds a caller in another
	// package with the old shape. b's land warns, in --json too.
	a.must("sync")
	b.must("sync")
	a.must("new", "Logout takes a reason")
	a.write("auth/auth.go", strings.Replace(a.read("auth/auth.go"), "func Logout() {", "func Logout(reason string) {", 1))
	a.must("land", "--check", "lint=pass")
	b.must("new", "Admin kicks users", "--no-sync")
	b.write("admin/admin.go", "package admin\n\nimport \"example.com/shop/auth\"\n\n// Kick signs a user out.\nfunc Kick() {\n\tauth.Logout()\n}\n")
	raw = b.must("land", "--check", "lint=pass", "--json")
	var landed struct {
		Warnings []struct{ Path, Kind, Detail string }
	}
	if err := json.Unmarshal([]byte(raw), &landed); err != nil || len(landed.Warnings) != 1 || landed.Warnings[0].Kind != "signature" || landed.Warnings[0].Path != "admin/admin.go" {
		t.Fatalf("land --json = %s, %v", raw, err)
	}
	wantW := "auth.Logout now takes (reason string) (changed in the effort); admin/admin.go:7 calls auth.Logout() (added by this land)"
	if landed.Warnings[0].Detail != wantW {
		t.Fatalf("warning = %q, want %q", landed.Warnings[0].Detail, wantW)
	}

	// The same declaration changed differently on both sides still conflicts.
	a.must("sync")
	b.must("sync")
	a.must("new", "Refresh A")
	a.write("auth/auth.go", strings.Replace(a.read("auth/auth.go"), "// Refresh renews a session.", "// Refresh renews a session, says A.", 1))
	a.must("land", "--check", "lint=pass")
	b.must("new", "Refresh B", "--no-sync")
	b.write("auth/auth.go", strings.Replace(b.read("auth/auth.go"), "// Refresh renews a session.", "// Refresh renews a session, says B.", 1))
	if out, code := b.run("land", "--check", "lint=pass"); code != 2 || strings.Contains(out, "Merged by declaration") {
		t.Fatalf("same-declaration land: code %d, %q", code, out)
	}
}
