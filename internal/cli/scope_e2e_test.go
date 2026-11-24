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

// TestAgentScopeCLI runs agent scopes and budgets through the real CLI:
// alice scopes her agent, bob and the agent cannot, the agent sees its
// scope in whoami and status, and the server refuses what is outside it.
func TestAgentScopeCLI(t *testing.T) {
	s := newAuthzServer(t)
	work := t.TempDir()
	alice := s.user("alice", s.tokens["alice"], work)
	bob := s.user("bob", s.tokens["bob"], work)
	alice.must("repo", "create", "acme/shop")
	alice.must("effort", "new", "Limits", "--repo", "acme/shop")
	agent := s.user("backend", agentToken(t, alice, "backend"), work)

	// No scope yet: no limits.
	if out := agent.must("whoami"); !strings.Contains(out, "Scope: no limits") {
		t.Fatalf("unscoped whoami = %q", out)
	}
	out := alice.must("agent", "scope", "backend", "--allow", "api/**", "--deny", "api/gen/**",
		"--max-lands-per-day", "3", "--max-lines", "4", "--max-files", "2", "--max-workspaces", "1")
	for _, want := range []string{"Updated the scope of agent:alice/backend.", "Paths: only api/**; except api/gen/**",
		"Lands: 3 of 3 left today (0 landed)", "Workspaces: 0 of 1 open (1 left)", "Per land: at most 4 changed lines, at most 2 files", "Set by user:alice"} {
		if !strings.Contains(out, want) {
			t.Errorf("vx agent scope output misses %q:\n%s", want, out)
		}
	}
	var shown struct {
		Agent struct {
			Scope struct {
				Allow          []string
				MaxLandsPerDay int `json:"max_lands_per_day"`
			}
			LandsLeft int `json:"lands_left"`
		}
	}
	if err := json.Unmarshal([]byte(alice.must("agent", "show", "backend", "--json")), &shown); err != nil ||
		len(shown.Agent.Scope.Allow) != 1 || shown.Agent.Scope.MaxLandsPerDay != 3 || shown.Agent.LandsLeft != 3 {
		t.Fatalf("vx agent show --json = %+v %v", shown, err)
	}
	// Without flags, vx agent scope shows the scope.
	if out := alice.must("agent", "scope", "backend"); !strings.Contains(out, "Paths: only api/**") || strings.Contains(out, "Updated") {
		t.Fatalf("vx agent scope NAME = %q", out)
	}

	// Another person and the agent itself cannot change it.
	if out, code := bob.run("agent", "scope", "alice/backend", "--clear"); code == 0 || !strings.Contains(out, "permission denied: user:bob cannot change the scope of agent:alice/backend") {
		t.Fatalf("bob = %d %q", code, out)
	}
	if out, code := bob.run("agent", "show", "alice/backend"); code == 0 || !strings.Contains(out, "permission denied") {
		t.Fatalf("bob shows = %d %q", code, out)
	}
	if out, code := agent.run("agent", "scope", "backend", "--clear"); code == 0 || !strings.Contains(out, "permission denied: agent:alice/backend cannot change scopes, not even its own") {
		t.Fatalf("agent = %d %q", code, out)
	}
	// A site admin can.
	ctx := context.Background()
	root, _ := s.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "root", SiteAdmin: true})
	rootTok, _ := s.st.CreateToken(ctx, root.ID, "test")
	admin := s.user("root", rootTok, work)
	if out := admin.must("agent", "scope", "alice/backend", "--max-files", "3"); !strings.Contains(out, "at most 3 files") || !strings.Contains(out, "Set by user:root") {
		t.Fatalf("site admin = %q", out)
	}

	// The agent sees its own scope.
	if out := agent.must("whoami"); !strings.Contains(out, "agent:alice/backend (acting for user:alice)") || !strings.Contains(out, "Paths: only api/**") || !strings.Contains(out, "Lands: 3 of 3 left today") {
		t.Fatalf("scoped whoami = %q", out)
	}
	if out := agent.must("agent", "show"); !strings.Contains(out, "Set by user:root") {
		t.Fatalf("agent show own = %q", out)
	}

	agent.must("clone", "acme/shop", "limits", "a")
	agent.dir = filepath.Join(work, "a")
	agent.must("new", "Handler")
	agent.write("api/h.go", "package api\n")
	if out := agent.must("land", "--run", "true"); !strings.Contains(out, "Landed") {
		t.Fatalf("land in scope = %q", out)
	}

	// Outside the scope: status warns, land refuses before running checks.
	agent.must("new", "Store")
	agent.write("store/x.go", "package store\n")
	if out := agent.must("status"); !strings.Contains(out, "Your scope (agent:alice/backend)") || !strings.Contains(out, "Outside your scope, vx land will refuse: store/x.go") || !strings.Contains(out, "Lands: 2 of 3 left today (1 landed)") {
		t.Fatalf("status = %q", out)
	}
	out, code := agent.run("land", "--run", "echo SHOULD-NOT-RUN")
	if code == 0 || !strings.Contains(out, "permission denied: agent:alice/backend may only change api/** except api/gen/**; store/x.go is outside it. Nothing ran or landed") || strings.Contains(out, "SHOULD-NOT-RUN") {
		t.Fatalf("out-of-scope land = %d %q", code, out)
	}

	// Too many lines: only the server counts them, and says so.
	agent.must("abandon")
	if err := os.Remove(filepath.Join(agent.dir, "store/x.go")); err != nil {
		t.Fatal(err)
	}
	agent.must("new", "Big")
	agent.write("api/big.go", "1\n2\n3\n4\n5\n")
	out, code = agent.run("land")
	if code == 0 || !strings.Contains(out, "this land changes 5 lines (added plus deleted) and agent:alice/backend may change at most 4 per land") {
		t.Fatalf("too many lines = %d %q", code, out)
	}

	// One open workspace at a time: a second working copy cannot open one.
	agent2 := s.user("backend", agent.env["VX_TOKEN"], work)
	agent2.must("clone", "acme/shop", "limits", "b")
	agent2.dir = filepath.Join(work, "b")
	if out, code := agent2.run("new", "Second"); code == 0 || !strings.Contains(out, "already has 1 open workspace and may have at most 1") {
		t.Fatalf("second workspace = %d %q", code, out)
	}

	// --clear goes back to no limits.
	if out := alice.must("agent", "scope", "backend", "--clear"); !strings.Contains(out, "Scope: no limits") {
		t.Fatalf("clear = %q", out)
	}
	if out := agent.must("land"); !strings.Contains(out, "Landed") {
		t.Fatalf("land after clear = %q", out)
	}
}
