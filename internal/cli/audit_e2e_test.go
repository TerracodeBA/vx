package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/store"
)

// TestAdminAuditCLI: an org owner exports their organization's audit log
// as JSONL or CSV; a member is refused with what to do.
func TestAdminAuditCLI(t *testing.T) {
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
	alice.must("org", "member", "acme", "bob", "--role", "member")

	out := alice.must("admin", "audit", "--org", "acme", "--since", "1d")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var ops []string
	for _, l := range lines {
		var row struct {
			Op, Org, Repo, Actor string
		}
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			t.Fatalf("not JSONL: %q (%v)", l, err)
		}
		if row.Org != "acme" {
			t.Errorf("row of org %q", row.Org)
		}
		ops = append(ops, row.Op)
	}
	if got := strings.Join(ops, " "); !strings.Contains(got, "org.create") || !strings.Contains(got, "repo.create") || !strings.Contains(got, "org.member") {
		t.Errorf("ops = %s", got)
	}

	alice.must("admin", "audit", "--org", "acme", "--repo", "acme/api", "--format", "csv", "-o", "audit.csv")
	data, err := os.ReadFile(filepath.Join(work, "audit.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "id,at,op,org,repo,actor,target,before,after\n") || !strings.Contains(string(data), "repo.create") || strings.Contains(string(data), "org.member") {
		t.Errorf("csv =\n%s", data)
	}
	if fi, _ := os.Stat(filepath.Join(work, "audit.csv")); fi.Mode().Perm() != 0o600 {
		t.Errorf("audit.csv mode %v", fi.Mode().Perm())
	}

	out, code := bob.run("admin", "audit", "--org", "acme")
	if code == 0 || !strings.Contains(out, "only owners and admins of acme export its audit log") {
		t.Errorf("member export = %d %s", code, out)
	}
	out, code = alice.run("admin", "audit", "--format", "xml")
	if code == 0 || !strings.Contains(out, "use jsonl or csv") {
		t.Errorf("bad format = %d %s", code, out)
	}
}
