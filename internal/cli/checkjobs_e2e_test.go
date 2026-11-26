package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCheckQueueCommands: vx agent runner, vx repo ci-token and the
// vx check queue commands against a real server.
func TestCheckQueueCommands(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop-api")
	alice.must("effort", "new", "Checkout", "--repo", "shop-api")

	var created struct{ Token string }
	if err := json.Unmarshal([]byte(alice.must("agent", "add", "qa", "--json")), &created); err != nil || created.Token == "" {
		t.Fatalf("agent add: %v", err)
	}
	qa := h.user("qa", created.Token, work)
	if out := alice.must("agent", "runner", "qa"); !strings.Contains(out, "runs no check jobs") {
		t.Fatalf("runner before = %q", out)
	}
	if out := alice.must("agent", "runner", "qa", "--kinds", "browser,security"); !strings.Contains(out, "browser, security") {
		t.Fatalf("runner set = %q", out)
	}
	if out, code := qa.run("agent", "runner", "qa", "--kinds", "ci"); code == 0 || !strings.Contains(out, "only the agent's person") {
		t.Fatalf("agent changing its kinds = %d %q", code, out)
	}
	if out := qa.must("check", "next", "--repo", "shop-api"); !strings.Contains(out, "No check job for you now") {
		t.Fatalf("next = %q", out)
	}
	if out, code := qa.run("check", "next", "--repo", "shop-api", "--wait", "--timeout", "300ms", "--interval", "50ms"); code != 4 {
		t.Fatalf("next --wait timeout = %d %q", code, out)
	}
	if out := qa.must("check", "jobs", "--repo", "shop-api"); !strings.Contains(out, "No check jobs") {
		t.Fatalf("jobs = %q", out)
	}

	var tok struct {
		Token struct {
			Name      string
			Principal string
			Checks    []string
		}
		Secret string
	}
	if err := json.Unmarshal([]byte(alice.must("repo", "ci-token", "create", "gha", "--repo", "shop-api", "--checks", "unit,lint", "--json")), &tok); err != nil || tok.Secret == "" {
		t.Fatalf("ci-token create: %v", err)
	}
	if tok.Token.Name != "gha" || !strings.HasPrefix(tok.Token.Principal, "service:ci-") || strings.Join(tok.Token.Checks, ",") != "lint,unit" {
		t.Fatalf("token = %+v", tok.Token)
	}
	if out, code := qa.run("repo", "ci-token", "create", "agent-made", "--repo", "shop-api"); code == 0 || !strings.Contains(out, "only people create CI tokens") {
		t.Fatalf("agent creating a CI token = %d %q", code, out)
	}
	ci := h.user("ci", tok.Secret, work)
	if out := ci.must("check", "jobs", "--repo", "shop-api"); !strings.Contains(out, "No check jobs") {
		t.Fatalf("ci jobs = %q", out)
	}
	if out, code := ci.run("effort", "list", "--repo", "shop-api"); code == 0 || !strings.Contains(out, "not found") {
		t.Fatalf("CI token listing efforts = %d %q", code, out)
	}
	if out := alice.must("repo", "ci-token", "list", "--repo", "shop-api"); !strings.Contains(out, "gha") || !strings.Contains(out, "lint, unit") {
		t.Fatalf("ci-token list = %q", out)
	}
	alice.must("repo", "ci-token", "revoke", "gha", "--repo", "shop-api")
	if out, code := ci.run("check", "jobs", "--repo", "shop-api"); code == 0 {
		t.Fatalf("revoked CI token still works: %q", out)
	}
	if out, code := alice.run("check", "request", "e2e", "--repo", "shop-api", "--effort", "checkout"); code == 0 || !strings.Contains(out, "no check") {
		t.Fatalf("request without checks = %d %q", code, out)
	}
}
