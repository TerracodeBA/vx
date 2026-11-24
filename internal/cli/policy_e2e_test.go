package cli_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/cli"
)

const e2ePolicies = `[checks]
required = ["true"]

[[policy]]
paths   = ["api/**"]
mode    = "gated"
require = ["true", "test -f api/limit.go"]

[[policy]]
paths = ["db/migrations/**"]
mode  = "human"
`

// An agent lands under each landing policy: fast lands, gated is refused
// with the exact vx land --run to use, human waits for a person. Alice
// approves one in the CLI and rejects another; the agent's vx wait, vx
// status and vx sync follow the decisions.
func TestLandingPoliciesE2E(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Schema", "--repo", "svc")
	agent := h.user("agent", agentToken(t, alice, "claude"), work)
	agent.must("clone", "svc", "schema", "a")
	agent.dir = filepath.Join(work, "a")
	agent.must("new", "Rules")
	agent.write("vx.toml", e2ePolicies)
	agent.must("land")
	if out := agent.must("rules"); !strings.Contains(out, "Landing policies (the strictest matching one applies):") ||
		!strings.Contains(out, "gated  api/**") || !strings.Contains(out, "human  db/migrations/**") {
		t.Fatalf("vx rules:\n%s", out)
	}

	// gated: refused without --run, with the command to use.
	agent.must("new", "Limits")
	agent.write("api/limit.go", "package api\n")
	out, code := agent.run("land")
	if code != 1 || !strings.Contains(out, "api/limit.go is under a gated landing policy") ||
		!strings.Contains(out, "vx land --run 'true' --run 'test -f api/limit.go'") {
		t.Fatalf("gated land without checks (exit %d):\n%s", code, out)
	}
	out = agent.must("land", "--run", "true", "--run", "test -f api/limit.go")
	if !strings.Contains(out, "Landed ") {
		t.Fatalf("gated land with checks:\n%s", out)
	}

	// human: waits for a person, exit 0, distinct status in --json.
	agent.must("new", "Users table")
	agent.write("db/migrations/002_users.sql", "create table users ();\n")
	var res struct {
		Status string `json:"status"`
		Landed bool   `json:"landed"`
		Review struct {
			ID      string `json:"id"`
			Waiting string `json:"waiting"`
			State   string `json:"state"`
		} `json:"review"`
	}
	raw := agent.must("land", "--run", "true", "--json")
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("%v:\n%s", err, raw)
	}
	if res.Status != "pending_review" || res.Landed || res.Review.State != "pending" ||
		res.Review.Waiting != "Waiting for a person to review: db/migrations/002_users.sql (policy in vx.toml)" {
		t.Fatalf("human land json: %+v", res)
	}
	id := res.Review.ID[:8]
	if out := agent.must("status"); !strings.Contains(out, "Waiting for a person to review: db/migrations/002_users.sql (policy in vx.toml)") ||
		!strings.Contains(out, "Waiting for a person to review (not in the effort yet; vx review list):") {
		t.Fatalf("status while pending:\n%s", out)
	}
	if out, code := agent.run("new", "Next"); code != 1 || !strings.Contains(out, "waits for a person to review it") {
		t.Fatalf("vx new while pending (exit %d):\n%s", code, out)
	}
	if out, code := agent.run("review", "approve", id); code != 1 || !strings.Contains(out, "only people approve") {
		t.Fatalf("agent approves (exit %d):\n%s", code, out)
	}
	if out := alice.must("review", "list"); !strings.Contains(out, id) || !strings.Contains(out, "you can approve it: vx review approve "+id) {
		t.Fatalf("review list:\n%s", out)
	}
	if out := alice.must("review", "show", id); !strings.Contains(out, "db/migrations/002_users.sql") || !strings.Contains(out, "pass true (run by vx)") {
		t.Fatalf("review show:\n%s", out)
	}
	if log := alice.must("log", "schema", "--repo", "svc"); strings.Contains(log, "Users table") {
		t.Fatalf("pending change in the log:\n%s", log)
	}
	out = alice.must("review", "approve", id)
	if !strings.Contains(out, "Approved review "+id) {
		t.Fatalf("approve:\n%s", out)
	}
	out = agent.must("wait", "--timeout", "5s")
	if !strings.Contains(out, "user:alice approved your change") || !strings.Contains(out, "Run `vx sync` to bring it in.") {
		t.Fatalf("wait after approval:\n%s", out)
	}
	if out := agent.must("sync"); !strings.Contains(out, "Synced to") {
		t.Fatalf("sync after approval:\n%s", out)
	}
	if out := agent.must("status"); !strings.Contains(out, "No workspace open") || !strings.Contains(out, "No local changes.") {
		t.Fatalf("status after approval:\n%s", out)
	}
	if log := alice.must("log", "schema", "--repo", "svc"); !strings.Contains(log, "approved by user:alice") {
		t.Fatalf("log after approval does not say who approved:\n%s", log)
	}

	// Reject: the agent hears the reason and its workspace stays open.
	agent.must("new", "Drop table")
	agent.write("db/migrations/003_drop.sql", "drop table users;\n")
	if out := agent.must("land", "--run", "true"); !strings.Contains(out, "Waiting for a person to review: db/migrations/003_drop.sql (policy in vx.toml)") {
		t.Fatalf("second human land:\n%s", out)
	}
	var list struct {
		Reviews []struct {
			ID string `json:"id"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(alice.must("review", "list", "--json")), &list); err != nil || len(list.Reviews) != 1 {
		t.Fatalf("review list --json: %+v %v", list, err)
	}
	if out, code := alice.run("review", "reject", list.Reviews[0].ID[:8]); code != 1 || !strings.Contains(out, "say why") {
		t.Fatalf("reject without reason (exit %d):\n%s", code, out)
	}
	alice.must("review", "reject", list.Reviews[0].ID[:8], "--reason", "Never drop tables")
	out = agent.must("wait", "--timeout", "5s")
	if !strings.Contains(out, "user:alice rejected your change") || !strings.Contains(out, "Never drop tables") {
		t.Fatalf("wait after rejection:\n%s", out)
	}
	if out := agent.must("status"); !strings.Contains(out, "Workspace ") || !strings.Contains(out, "db/migrations/003_drop.sql") {
		t.Fatalf("status after rejection:\n%s", out)
	}
	// The rejection note reached the agent on the board, addressed to it.
	if out := agent.must("board"); !strings.Contains(out, "rejected your change") {
		t.Fatalf("board:\n%s", out)
	}
	agent.must("abandon")
}

// Approval that conflicts: the review stays pending, alice's vx review
// approve exits 2, the agent resolves and lands again, then it lands.
func TestLandingPolicyConflictE2E(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Schema", "--repo", "svc")
	agent := h.user("agent", agentToken(t, alice, "claude"), work)
	other := h.user("other", agentToken(t, alice, "other"), work)
	agent.must("clone", "svc", "schema", "a")
	agent.dir = filepath.Join(work, "a")
	agent.must("new", "Rules")
	agent.write("vx.toml", e2ePolicies)
	agent.write("config.txt", "limit = 1\n")
	agent.must("land")
	other.must("clone", "svc", "schema", "b")
	other.dir = filepath.Join(work, "b")

	agent.must("new", "Users and limit")
	agent.write("db/migrations/002_users.sql", "create table users ();\n")
	agent.write("config.txt", "limit = 2\n")
	agent.must("land", "--run", "true")
	other.must("new", "Other limit")
	other.write("config.txt", "limit = 3\n")
	other.must("land")

	var list struct {
		Reviews []struct {
			ID string `json:"id"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(alice.must("review", "list", "--repo", "svc", "--json")), &list); err != nil || len(list.Reviews) != 1 {
		t.Fatalf("review list: %+v %v", list, err)
	}
	id := list.Reviews[0].ID[:8]
	out, code := alice.run("review", "approve", id)
	if code != cli.ExitConflict || !strings.Contains(out, "config.txt") || !strings.Contains(out, "The review stays pending") {
		t.Fatalf("conflicting approval (exit %d):\n%s", code, out)
	}
	if out := agent.must("status"); !strings.Contains(out, "it conflicts with the effort in config.txt") {
		t.Fatalf("agent status after the conflicting approval:\n%s", out)
	}
	out, code = agent.run("sync")
	if code != cli.ExitConflict {
		t.Fatalf("sync (exit %d):\n%s", code, out)
	}
	agent.write("config.txt", "limit = 23\n")
	if out := agent.must("land"); !strings.Contains(out, "Waiting for a person to review: db/migrations/002_users.sql") {
		t.Fatalf("land after resolving:\n%s", out)
	}
	if out := alice.must("review", "approve", id); !strings.Contains(out, "Approved review "+id) {
		t.Fatalf("approve after resolving:\n%s", out)
	}
	other.must("sync")
	if got := other.read("config.txt"); got != "limit = 23\n" {
		t.Fatalf("config.txt = %q", got)
	}
}

// A pending review is not shipped: the ship plan and the pull request
// body have the landed changes only, until a person approves it.
func TestShipPlanLeavesOutPendingReviews(t *testing.T) {
	up := newGitUpstream(t)
	up.commit("Initial", map[string]string{"vx.toml": e2ePolicies, "README.md": "hi\n"})
	h, aliceToken := newHarnessWith(t, true)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop", "--git", up.dir)
	alice.must("effort", "new", "Schema", "--repo", "shop")
	agent := h.user("agent", agentToken(t, alice, "claude"), work)
	agent.must("clone", "shop", "schema", "a")
	agent.dir = filepath.Join(work, "a")
	agent.must("new", "Docs")
	agent.write("README.md", "hello\n")
	agent.must("land")
	agent.must("new", "Users table")
	agent.write("db/migrations/002_users.sql", "create table users ();\n")
	agent.must("land", "--run", "true")

	out := alice.must("ship", "plan", "--repo", "shop", "schema")
	if !strings.Contains(out, "Docs") || strings.Contains(out, "Users table") {
		t.Fatalf("ship plan with a pending review:\n%s", out)
	}
	var list struct {
		Reviews []struct {
			ID string `json:"id"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(alice.must("review", "list", "--json")), &list); err != nil || len(list.Reviews) != 1 {
		t.Fatalf("review list: %+v %v", list, err)
	}
	alice.must("review", "approve", list.Reviews[0].ID)
	if out := alice.must("ship", "plan", "--repo", "shop", "schema"); !strings.Contains(out, "Users table") {
		t.Fatalf("ship plan after approval:\n%s", out)
	}
}
