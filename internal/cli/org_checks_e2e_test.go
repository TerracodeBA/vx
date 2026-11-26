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

// TestOrgChecksCLI: an owner saves organization checks from a file, a
// member reads them and their history, and neither a member nor an agent
// can change them.
func TestOrgChecksCLI(t *testing.T) {
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
	alice.must("org", "member", "acme", "bob", "--role", "member")

	if out := bob.must("org", "checks", "acme"); !strings.Contains(out, "has no checks of its own") {
		t.Errorf("before = %q", out)
	}
	file := filepath.Join(work, "checks.toml")
	src := "[[check]]\nname   = \"security-review\"\nwhen   = \"ship\"\nrunner = \"human\"\n\n[[check]]\nname = \"unit\"\nrun  = \"go test ./...\"\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := bob.run("org", "checks", "acme", "--file", file); code == 0 || !strings.Contains(out, "owner or admin") {
		t.Errorf("member set = %d %q", code, out)
	}
	if out := alice.must("org", "checks", "acme", "--file", file); !strings.Contains(out, "saved version 1 with 2 check(s): security-review, unit") {
		t.Errorf("set = %q", out)
	}
	if out := alice.must("org", "checks", "acme", "--file", file); !strings.Contains(out, "already in force (version 1)") {
		t.Errorf("set again = %q", out)
	}
	if out := bob.must("org", "checks", "acme"); !strings.Contains(out, "version 1, saved by user:alice") || !strings.Contains(out, `name   = "security-review"`) {
		t.Errorf("show = %q", out)
	}
	if out := alice.must("org", "checks", "acme", "--clear"); !strings.Contains(out, "removed every organization check (version 2)") {
		t.Errorf("clear = %q", out)
	}
	var hist struct {
		History []struct {
			Version int32
			Checks  []string
		}
	}
	if err := json.Unmarshal([]byte(bob.must("org", "checks", "acme", "--history", "--json")), &hist); err != nil || len(hist.History) != 2 || hist.History[0].Version != 2 || len(hist.History[1].Checks) != 2 {
		t.Errorf("history: %v %+v", err, hist)
	}
	// An agent never changes organization checks.
	agent := h.user("alice/rules-bot", agentToken(t, alice, "rules-bot"), work)
	if out, code := agent.run("org", "checks", "acme", "--file", file); code == 0 || !strings.Contains(out, "agent") {
		t.Errorf("agent set = %d %q", code, out)
	}
	// A cycle is refused with the reason.
	if err := os.WriteFile(file, []byte("[[check]]\nname = \"a\"\nrun = \"true\"\nafter = [\"a\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := alice.run("org", "checks", "acme", "--file", file); code == 0 || !strings.Contains(out, "after names the check itself") {
		t.Errorf("cycle = %d %q", code, out)
	}
}
