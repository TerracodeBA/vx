package cli_test

import (
	"encoding/json"
	"path/filepath"
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
