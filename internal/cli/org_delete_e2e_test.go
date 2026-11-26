package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/store"
)

// TestOrgDeleteCLI: alice deletes acme (after confirming), its repository
// is gone for bob and for her, she keeps the confirmation and restores it.
func TestOrgDeleteCLI(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	ctx := context.Background()
	bobP, err := h.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	bobToken, _ := h.st.CreateToken(ctx, bobP.ID, "test")
	bob := h.user("bob", bobToken, work)

	alice.must("org", "create", "acme")
	alice.must("repo", "create", "acme/api")
	alice.must("org", "member", "acme", "bob", "--role", "admin")

	if out, code := alice.run("org", "delete", "acme"); code == 0 || !strings.Contains(out, "--confirm acme") {
		t.Errorf("delete without --confirm = %d %q", code, out)
	}
	if out, code := bob.run("org", "delete", "acme", "--confirm", "acme"); code == 0 || !strings.Contains(out, "needs owner") {
		t.Errorf("an admin deletes = %d %q", code, out)
	}
	if out, code := alice.run("org", "delete", "alice", "--confirm", "alice"); code == 0 || !strings.Contains(out, "personal organization") {
		t.Errorf("deleting the personal organization = %d %q", code, out)
	}
	out := alice.must("org", "delete", "acme", "--confirm", "acme")
	if !strings.Contains(out, "Deleted organization acme") || !strings.Contains(out, "vx org restore acme") {
		t.Errorf("delete = %q", out)
	}
	for _, u := range []*user{alice, bob} {
		if out := u.must("repo", "list"); strings.Contains(out, "acme/api") {
			t.Errorf("repo list after the deletion = %q", out)
		}
		if out, code := u.run("effort", "new", "Late", "--repo", "acme/api"); code == 0 || !strings.Contains(out, "repo acme/api: not found") {
			t.Errorf("effort new after the deletion = %d %q", code, out)
		}
		if out := u.must("org", "list"); strings.Contains(out, "acme ") {
			t.Errorf("org list after the deletion = %q", out)
		}
	}
	if out := alice.must("org", "deletion"); !strings.Contains(out, "acme") || !strings.Contains(out, "purge on") {
		t.Errorf("org deletion (list) = %q", out)
	}
	if out, code := bob.run("org", "deletion", "acme"); code == 0 || !strings.Contains(out, "not found") {
		t.Errorf("bob reads the confirmation = %d %q", code, out)
	}
	alice.must("org", "deletion", "acme", "-o", "acme-deletion.txt")
	text, err := os.ReadFile(filepath.Join(alice.dir, "acme-deletion.txt"))
	if err != nil || !strings.Contains(string(text), "Deletion confirmation") || !strings.Contains(string(text), "scheduled for") || strings.Contains(string(text), "acme/api") {
		t.Errorf("confirmation file = %q, %v", text, err)
	}
	if out := alice.must("org", "restore", "acme"); !strings.Contains(out, "Restored organization acme") {
		t.Errorf("restore = %q", out)
	}
	if out := bob.must("repo", "list"); !strings.Contains(out, "acme/api") {
		t.Errorf("bob after the restore = %q", out)
	}
}
