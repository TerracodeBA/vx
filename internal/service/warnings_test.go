package service_test

import (
	"testing"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/service"
)

// Two agents each add clientIP to the same package in different files. The
// text merges cleanly, so both land, but the second gets a warning.
func TestLandWarnsAboutDuplicateGoDeclarations(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Rate limit by client IP")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Audit log with client IP")
	const fn = "package httpapi\n\nfunc clientIP() string { return \"\" }\n"
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{"internal/httpapi/limit.go": s(fn)})
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{"internal/httpapi/audit.go": s(fn)})

	r1, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1})
	if err != nil || !r1.Landed || len(r1.Warnings) != 0 {
		t.Fatalf("first land: %+v %v", r1, err)
	}
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil || !r2.Landed || !r2.Rebased {
		t.Fatalf("second land should merge and land: %+v %v", r2, err)
	}
	want := "duplicate func clientIP in internal/httpapi (audit.go, limit.go)"
	if len(r2.Warnings) != 1 || r2.Warnings[0].Kind != engine.WarningDuplicate || r2.Warnings[0].Detail != want {
		t.Fatalf("warnings = %+v, want %q", r2.Warnings, want)
	}

	// Fixing it lands without warnings.
	w3, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Drop the second clientIP")
	t3 := e.snapshot(repo, w3.Workspace.Base, map[string]*string{"internal/httpapi/audit.go": s("package httpapi\n")})
	r3, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w3.Workspace.ID, Tree: t3})
	if err != nil || !r3.Landed || len(r3.Warnings) != 0 {
		t.Fatalf("fix land: %+v %v", r3, err)
	}

	// Anyone reading the effort later sees the warning on the change that
	// caused it, and only there.
	_, got, err := e.svc.GetChangeset(e.ctx, e.alice, repo.Name, w2.Workspace.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Detail != want || got.Warnings[0].Kind != string(engine.WarningDuplicate) ||
		got.Warnings[0].Path != "internal/httpapi" || got.Warnings[0].Name != "clientIP" {
		t.Fatalf("stored warnings = %+v, want %q", got.Warnings, want)
	}
	log, err := e.svc.Log(e.ctx, e.alice, repo.Name, eff.Slug, 10, false)
	if err != nil || len(log) < 3 {
		t.Fatalf("log = %d entries, %v", len(log), err)
	}
	for _, entry := range log {
		n := 0
		for _, w := range entry.Warnings {
			if w.Kind == string(engine.WarningDuplicate) {
				n++
			}
		}
		if (entry.Hash == r2.Changeset.Hash) != (n == 1) {
			t.Errorf("log entry %s (%s) has %d duplicate warnings", entry.ChangeID, entry.Intent, n)
		}
	}
}
