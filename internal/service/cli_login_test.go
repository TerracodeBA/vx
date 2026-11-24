package service_test

import (
	"crypto/sha256"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/version"
)

// TestCLILoginApprove: start, pending, slow down, approve, the token comes
// out exactly once and works, then the login says used.
func TestCLILoginApprove(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.svc.Now = func() time.Time { return now }
	ctx := service.WithClientIP(e.ctx, "203.0.113.7")
	l, err := e.svc.StartCLILogin(ctx, "vx on alice-laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`).MatchString(l.UserCode) {
		t.Fatalf("user code %q", l.UserCode)
	}
	if len(l.DeviceCode) < 40 || l.Interval != 2*time.Second || !l.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("login = %+v", l)
	}
	if l.VerificationURL != "/ui/cli/approve?code="+l.UserCode {
		t.Fatalf("verification url %q", l.VerificationURL)
	}
	// The device code itself is never stored: only its sha256.
	row, err := e.svc.Store.CLILoginByUserCode(ctx, l.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(l.DeviceCode))
	if string(row.DeviceHash) != string(sum[:]) || row.ClientName != "vx on alice-laptop" || row.ClientIP != "203.0.113.7" || row.HasSecret {
		t.Fatalf("row = %+v", row)
	}

	status, tok, _, err := e.svc.PollCLILogin(ctx, l.DeviceCode)
	if err != nil || status != service.CLILoginPending || tok != "" {
		t.Fatalf("first poll = %q %q %v", status, tok, err)
	}
	now = now.Add(500 * time.Millisecond)
	if status, _, _, _ := e.svc.PollCLILogin(ctx, l.DeviceCode); status != service.CLILoginSlowDown {
		t.Fatalf("fast poll = %q, want slow_down", status)
	}
	now = now.Add(2 * time.Second)
	if status, _, _, _ := e.svc.PollCLILogin(ctx, l.DeviceCode); status != service.CLILoginPending {
		t.Fatalf("poll after interval = %q", status)
	}

	// The approval page: lower case and no dash find the login too.
	loose := strings.ToLower(strings.ReplaceAll(l.UserCode, "-", " "))
	client, exp, err := e.svc.PendingCLILogin(ctx, e.alice, loose)
	if err != nil || client != "vx on alice-laptop" || exp.Sub(l.ExpiresAt).Abs() > time.Millisecond {
		t.Fatalf("pending = %q %v %v", client, exp, err)
	}
	// Agents cannot authorize CLIs.
	if err := e.svc.ApproveCLILogin(ctx, e.agent, l.UserCode, true); codeOf(err) != service.CodePermission {
		t.Fatalf("agent approve = %v", err)
	}
	if err := e.svc.ApproveCLILogin(ctx, e.alice, l.UserCode, true); err != nil {
		t.Fatal(err)
	}
	// Decided: the page no longer offers it, approving twice fails.
	if _, _, err := e.svc.PendingCLILogin(ctx, e.alice, l.UserCode); codeOf(err) != service.CodeNotFound {
		t.Fatalf("pending after approve = %v", err)
	}
	if err := e.svc.ApproveCLILogin(ctx, e.alice, l.UserCode, false); codeOf(err) != service.CodeNotFound {
		t.Fatalf("deny after approve = %v", err)
	}

	status, tok, p, err := e.svc.PollCLILogin(ctx, l.DeviceCode)
	if err != nil || status != service.CLILoginApproved || tok == "" || p.ID != e.alice.ID {
		t.Fatalf("approved poll = %q %q %v %v", status, tok, p.Handle, err)
	}
	if got, err := e.svc.Store.Authenticate(ctx, tok); err != nil || got.ID != e.alice.ID {
		t.Fatalf("token does not authenticate: %v", err)
	}
	toks, _ := e.svc.Store.ListTokens(ctx, e.alice.ID)
	if len(toks) != 1 || toks[0].Name != "vx on alice-laptop (browser login)" {
		t.Fatalf("tokens = %+v", toks)
	}
	ops, _ := e.svc.Store.ListActorOps(ctx, e.alice.ID, 10)
	var seen []string
	for _, op := range ops {
		seen = append(seen, op.Op)
	}
	if !strings.Contains(strings.Join(seen, ","), "token.create") || !strings.Contains(strings.Join(seen, ","), "cli_login.approve") {
		t.Fatalf("ops = %v", seen)
	}
	// Exactly once: replaying the device code gets no token.
	for range 2 {
		now = now.Add(3 * time.Second)
		status, tok, _, err = e.svc.PollCLILogin(ctx, l.DeviceCode)
		if err != nil || status != service.CLILoginUsed || tok != "" {
			t.Fatalf("replayed poll = %q %q %v", status, tok, err)
		}
	}
	if toks, _ := e.svc.Store.ListTokens(ctx, e.alice.ID); len(toks) != 1 {
		t.Fatalf("replay created tokens: %d", len(toks))
	}
	if _, _, _, err := e.svc.PollCLILogin(ctx, "not-a-device-code"); codeOf(err) != service.CodeNotFound {
		t.Fatalf("unknown device code = %v", err)
	}
}

func TestCLILoginDenyAndExpire(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.svc.Now = func() time.Time { return now }
	denied, err := e.svc.StartCLILogin(e.ctx, "vx on bob-box")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ApproveCLILogin(e.ctx, e.alice, denied.UserCode, false); err != nil {
		t.Fatal(err)
	}
	if status, tok, _, err := e.svc.PollCLILogin(e.ctx, denied.DeviceCode); err != nil || status != service.CLILoginDenied || tok != "" {
		t.Fatalf("denied poll = %q %q %v", status, tok, err)
	}

	late, _ := e.svc.StartCLILogin(e.ctx, "vx on slow")
	approvedLate, _ := e.svc.StartCLILogin(e.ctx, "vx on slower")
	if err := e.svc.ApproveCLILogin(e.ctx, e.alice, approvedLate.UserCode, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(service.CLILoginTTL)
	if _, _, err := e.svc.PendingCLILogin(e.ctx, e.alice, late.UserCode); codeOf(err) != service.CodeNotFound {
		t.Fatalf("expired pending = %v", err)
	}
	if err := e.svc.ApproveCLILogin(e.ctx, e.alice, late.UserCode, true); codeOf(err) != service.CodeNotFound {
		t.Fatalf("expired approve = %v", err)
	}
	for _, l := range []service.CLILogin{late, approvedLate} {
		if status, tok, _, err := e.svc.PollCLILogin(e.ctx, l.DeviceCode); err != nil || status != service.CLILoginExpired || tok != "" {
			t.Fatalf("expired poll = %q %q %v", status, tok, err)
		}
	}
	if toks, _ := e.svc.Store.ListTokens(e.ctx, e.alice.ID); len(toks) != 0 {
		t.Fatalf("tokens after deny/expire: %d", len(toks))
	}
	if _, _, err := e.svc.PendingCLILogin(e.ctx, e.alice, "AAAA-AAAA"); codeOf(err) != service.CodeNotFound {
		t.Fatalf("impossible code = %v", err)
	}
}

func TestCLILoginRateLimit(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.svc.Now = func() time.Time { return now }
	ctx := service.WithClientIP(e.ctx, "198.51.100.1")
	for i := range service.CLILoginStartsPerIP {
		if _, err := e.svc.StartCLILogin(ctx, "vx"); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	_, err := e.svc.StartCLILogin(ctx, "vx")
	if codeOf(err) != service.CodeTooMany || !strings.Contains(err.Error(), "wait a few minutes") {
		t.Fatalf("over the limit = %v", err)
	}
	// Another address is not affected; the first one may start again later.
	if _, err := e.svc.StartCLILogin(service.WithClientIP(e.ctx, "198.51.100.2"), "vx"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(service.CLILoginRateWindow + time.Second)
	if _, err := e.svc.StartCLILogin(ctx, "vx"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestCheckCLIVersion(t *testing.T) {
	e := newEnv(t)
	old := version.Version
	t.Cleanup(func() { version.Version = old })
	version.Version = "0.5.0"
	e.svc.GitHub.PublicURL = "https://vx.example.com"
	for _, tc := range []struct{ cli, status string }{
		{"dev", service.CLIOK},
		{"", service.CLIOK},
		{"0.5.0", service.CLIOK},
		{"0.6.0", service.CLIOK},
		{"0.4.2", service.CLIUpdateRecommended},
		{"0.0.9", service.CLIUpdateRequired},
	} {
		status, msg := e.svc.CheckCLIVersion(tc.cli, "http://ignored")
		if status != tc.status {
			t.Errorf("CheckCLIVersion(%q) = %q, want %q", tc.cli, status, tc.status)
		}
		if status != service.CLIOK && !strings.Contains(msg, "curl -fsSL https://vx.example.com/install.sh | sh") {
			t.Errorf("message for %q = %q", tc.cli, msg)
		}
	}
	// A dev server never recommends updates.
	version.Version = "dev"
	if status, _ := e.svc.CheckCLIVersion("0.4.2", "http://x"); status != service.CLIOK {
		t.Errorf("dev server: %q", status)
	}
}

// TestCLILoginTokenOnceConcurrently: polls racing after approval get the
// token once between them.
func TestCLILoginTokenOnceConcurrently(t *testing.T) {
	e := newEnv(t)
	l, err := e.svc.StartCLILogin(e.ctx, "vx on racer")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ApproveCLILogin(e.ctx, e.alice, l.UserCode, true); err != nil {
		t.Fatal(err)
	}
	const n = 8
	tokens := make(chan string, n)
	for range n {
		go func() {
			_, tok, _, err := e.svc.PollCLILogin(e.ctx, l.DeviceCode)
			if err != nil {
				t.Error(err)
			}
			tokens <- tok
		}()
	}
	got := 0
	for range n {
		if <-tokens != "" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d polls got a token, want 1", got)
	}
	if toks, _ := e.svc.Store.ListTokens(e.ctx, e.alice.ID); len(toks) != 1 {
		t.Fatalf("tokens = %d", len(toks))
	}
}
