package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// A question to an agent that is not working says so at once and lands in
// its person's inbox under "For your agents (not working now)".
func TestAskAgentNotWorkingCLI(t *testing.T) {
	alice, a, _ := boardPair(t)
	alice.must("agent", "add", "idle") // its token is never used

	out := a.must("ask", "Who owns the limiter?", "--to", "idle")
	for _, want := range []string{
		"Asked agent:alice/idle: question ",
		"Note: agent:alice/idle is not working right now (never seen); the question also goes to user:alice.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ask: missing %q in\n%s", want, out)
		}
	}
	var resp struct {
		Notice   string
		CopiedTo string `json:"copied_to"`
	}
	if err := json.Unmarshal([]byte(a.must("ask", "And the tests?", "--to", "idle", "--json")), &resp); err != nil ||
		!strings.Contains(resp.Notice, "is not working right now") || resp.CopiedTo != "user:alice" {
		t.Fatalf("ask --json = %+v %v", resp, err)
	}
	// A working agent: no notice.
	if out := alice.must("ask", "Busy?", "--to", "backend", "--repo", "shop-api", "--effort", "rate-limiting"); strings.Contains(out, "not working") {
		t.Errorf("ask a working agent:\n%s", out)
	}
	out = alice.must("inbox")
	for _, want := range []string{
		"For your agents (not working now) (2):",
		"Who owns the limiter?",
		"Answer yourself (vx answer ID \"TEXT\"), or start the agent.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inbox: missing %q in\n%s", want, out)
		}
	}
	if strings.Count(out, "Who owns the limiter?") != 1 {
		t.Errorf("the copied question is repeated under Questions for you:\n%s", out)
	}
}

// vx ask --wait --timeout stops waiting like vx wait (exit 4); --timeout
// alone is refused; questions need no --on.
func TestAskTimeoutCLI(t *testing.T) {
	_, a, _ := boardPair(t)
	out, code := a.run("ask", "Anyone?", "--wait", "--timeout", "300ms", "--interval", "50ms")
	if code != 4 || !strings.Contains(out, "No answer within 300ms; the question stays open") {
		t.Fatalf("ask --wait --timeout = %d\n%s", code, out)
	}
	if out, code := a.run("ask", "Anyone?", "--timeout", "1s"); code == 0 || !strings.Contains(out, "add --wait") {
		t.Fatalf("ask --timeout without --wait = %d\n%s", code, out)
	}
	if out := a.must("ask", "--help"); !strings.Contains(out, "ask QUESTION [--on TARGET...]") || !strings.Contains(out, "--timeout duration") {
		t.Errorf("ask --help:\n%s", out)
	}
}

// vx claim --moving pkg.Func --to PATH: someone landing a change to the old
// location is told where the code is going.
func TestMovingClaimCLI(t *testing.T) {
	_, a, b := boardPair(t)
	login := "package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {}\n"
	a.must("new", "Add auth")
	a.write("auth/auth.go", login)
	a.must("land")

	a.must("new", "Move Login")
	if out, code := a.run("claim", "--moving", "auth.Login"); code == 0 || !strings.Contains(out, "--moving needs --to") {
		t.Fatalf("claim --moving without --to = %d\n%s", code, out)
	}
	if out, code := a.run("claim"); code == 0 || !strings.Contains(out, "say what you are claiming") {
		t.Fatalf("claim without targets = %d\n%s", code, out)
	}
	out := a.must("claim", "--moving", "auth.Login", "--to", "auth/login.go")
	if !strings.Contains(out, "Claimed auth.Login as moving to auth/login.go") {
		t.Fatalf("claim --moving:\n%s", out)
	}
	if out := b.must("claims"); !strings.Contains(out, "on auth.Login  moving to auth/login.go") {
		t.Errorf("claims:\n%s", out)
	}
	// --on works like the arguments, as for vx post and vx ask.
	if out := b.must("claim", "--on", "auth.Logout", "--no-workspace"); !strings.Contains(out, "Claimed auth.Logout until") {
		t.Errorf("claim --on:\n%s", out)
	}

	b.must("sync")
	b.must("new", "Limit Login")
	b.write("auth/auth.go", strings.Replace(login, "check()", "limit()", 1))
	out = b.must("land")
	if want := "Warning: auth.Login is being moved to auth/login.go by agent:alice/backend (claim "; !strings.Contains(out, want) {
		t.Errorf("land over a moving claim: missing %q in\n%s", want, out)
	}
	if !strings.Contains(out, "Your change landed at the old location: make it at auth/login.go too") {
		t.Errorf("land over a moving claim:\n%s", out)
	}
	if out := a.must("inbox"); !strings.Contains(out, "About your claims (1):") || !strings.Contains(out, "agent:bob/audit landed a change to auth.Login, which you are moving to auth/login.go") {
		t.Errorf("mover's inbox:\n%s", out)
	}
}
