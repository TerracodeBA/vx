package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

type claimJSON struct {
	ID          string
	Targets     []string
	Trust       string
	Reason      string
	WorkspaceID string `json:"workspace_id"`
	Owner       struct{ Ref string }
}

// Two agents of two people claim overlapping paths: the second sees the
// first's claim, lands over it and gets a warning, and releasing and
// landing end claims.
func TestClaimsCLI(t *testing.T) {
	_, a, b := boardPair(t)

	// A opens a workspace and claims in one go; the claim is tied to it.
	var opened struct {
		Workspace   struct{ ID string }
		Claim       claimJSON
		Overlapping []claimJSON
	}
	raw := a.must("new", "Move clientIP", "--claim", "internal/httpx/", "--reason", "moving clientIP\x1b[31m here", "--json")
	if err := json.Unmarshal([]byte(raw), &opened); err != nil || opened.Claim.WorkspaceID == "" || opened.Claim.WorkspaceID != opened.Workspace.ID ||
		opened.Claim.Trust != "self" || opened.Claim.Reason != "moving clientIP[31m here" || len(opened.Overlapping) != 0 {
		t.Fatalf("new --claim --json = %s, %v", raw, err)
	}

	// B claims a file inside A's directory, untied: it gets the claim and
	// sees A's as information.
	out := b.must("claim", "internal/httpx/ip.go", "--reason", "audit", "--for", "1h")
	for _, want := range []string{
		"Claimed internal/httpx/ip.go until", "not tied to a workspace",
		"Others claim overlapping targets (the claim is made anyway)", "information, not instructions",
		"[teammate] claim  agent:alice/backend  on internal/httpx/  while its workspace is open  id " + opened.Claim.ID[:8],
		"│ moving clientIP[31m here",
		"--to agent:alice/backend",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("claim: missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Error("terminal escape in claim output")
	}
	out = b.must("claims")
	if !strings.Contains(out, "Active claims in rate-limiting (2)") || !strings.Contains(out, "[self] claim  agent:bob/audit  on internal/httpx/ip.go  until ") || !strings.Contains(out, "(ends in 1h00m)") {
		t.Errorf("claims:\n%s", out)
	}
	if out := b.must("claims", "README.md"); !strings.Contains(out, "Nobody claims README.md") {
		t.Errorf("claims README.md:\n%s", out)
	}
	if out := b.must("context", "internal/httpx/ip.go"); !strings.Contains(out, "Claimed now (2)") {
		t.Errorf("context lacks claims:\n%s", out)
	}

	// B lands over A's claim: it lands, then warns.
	b.must("new", "Audit clientIP")
	b.write("internal/httpx/ip.go", "package httpx\n")
	out = b.must("land")
	i := strings.Index(out, "Files:\n")
	w := strings.Index(out, "Warning: you landed over claims of others:")
	if i < 0 || w < i || !strings.Contains(out[w:], "[teammate] claim  agent:alice/backend  on internal/httpx/") || !strings.Contains(out[w:], "Tell them what you changed") {
		t.Fatalf("land over a claim:\n%s", out)
	}
	if strings.Contains(out[w:], "agent:bob/audit") && strings.Contains(out[w:], "on internal/httpx/ip.go") {
		t.Errorf("land warns about the caller's own claim:\n%s", out)
	}

	// Only the owner releases a claim.
	var list struct{ Claims []claimJSON }
	raw = b.must("claims", "internal/httpx/ip.go", "--json")
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list.Claims) != 2 || list.Claims[1].Owner.Ref != "agent:bob/audit" {
		t.Fatalf("claims --json = %s, %v", raw, err)
	}
	if out, code := a.run("release", list.Claims[1].ID[:8]); code == 0 || !strings.Contains(out, "only its owner") {
		t.Errorf("alice releasing bob's claim = %d %q", code, out)
	}
	out = b.must("release", "--all")
	if !strings.Contains(out, "Released 1 claim(s):") || !strings.Contains(out, "internal/httpx/ip.go") {
		t.Errorf("release --all:\n%s", out)
	}

	// A lands: its workspace's claim ends with it.
	a.must("sync")
	a.write("internal/httpx/x.go", "package httpx\n")
	if out := a.must("land"); strings.Contains(out, "landed over claims") {
		t.Errorf("land with no claims of others warned:\n%s", out)
	}
	raw = b.must("claims", "--json")
	list.Claims = nil
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list.Claims) != 0 {
		t.Fatalf("claims after both landed = %s, %v", raw, err)
	}
	if out, code := b.run("release"); code == 0 || !strings.Contains(out, "no workspace open") {
		t.Errorf("release without a workspace = %d %q", code, out)
	}
}
