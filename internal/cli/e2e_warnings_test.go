package cli_test

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The issue #14 case end to end: two agents add the same func in different
// files of one package. Both land; the second sees a warning, in text and
// in --json.
func TestLandPrintsDuplicateDeclarationWarning(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	a := h.user("a", aliceToken, work)
	b := h.user("b", aliceToken, work)
	a.must("clone", "svc", "feature", "a")
	b.must("clone", "svc", "feature", "b")
	a.dir, b.dir = filepath.Join(work, "a"), filepath.Join(work, "b")

	const fn = "package httpapi\n\nfunc clientIP() string { return \"\" }\n"
	a.must("new", "Rate limit")
	a.write("internal/httpapi/limit.go", fn)
	if out := a.must("land"); strings.Contains(out, "Warning") {
		t.Fatalf("first land should not warn: %s", out)
	}

	b.must("new", "Audit log")
	b.write("internal/httpapi/audit.go", fn)
	out := b.must("land")
	if !strings.Contains(out, "Warning: duplicate func clientIP in internal/httpapi (audit.go, limit.go)") {
		t.Fatalf("land should warn about the duplicate: %s", out)
	}
	m := regexp.MustCompile(`Landed [0-9a-f]+ \(([a-z0-9-]+)\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no change id in land output: %s", out)
	}
	changeID := m[1]

	// The warning is stored with the change: another agent sees it later in
	// vx log, vx show and vx status.
	const warnLine = "warning: duplicate func clientIP in internal/httpapi (audit.go, limit.go)"
	if a.must("sync"); !strings.Contains(a.must("log"), "    "+warnLine) {
		t.Fatalf("vx log should show the stored warning under the change")
	}
	if out := a.must("show", changeID); !strings.Contains(out, warnLine) {
		t.Fatalf("vx show should show the stored warning: %s", out)
	}
	if out := a.must("status"); !strings.Contains(out, "Warnings in recent lands: duplicate func clientIP in internal/httpapi (audit.go, limit.go) (change "+changeID+")") {
		t.Fatalf("vx status should mention the warning: %s", out)
	}
	var sj struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(a.must("status", "--json")), &sj); err != nil || len(sj.Warnings) != 1 {
		t.Fatalf("status --json warnings = %+v, %v", sj, err)
	}

	b.must("new", "Another duplicate")
	b.write("internal/httpapi/more.go", fn)
	var lj struct {
		Landed   bool `json:"landed"`
		Warnings []struct {
			Path   string `json:"path"`
			Kind   string `json:"kind"`
			Detail string `json:"detail"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(b.must("land", "--json")), &lj); err != nil {
		t.Fatal(err)
	}
	if !lj.Landed || len(lj.Warnings) != 1 || lj.Warnings[0].Kind != "duplicate" || lj.Warnings[0].Path != "internal/httpapi" ||
		lj.Warnings[0].Detail != "duplicate func clientIP in internal/httpapi (audit.go, limit.go, more.go)" {
		t.Fatalf("land --json warnings = %+v", lj)
	}
}
