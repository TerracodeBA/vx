package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

// Two workspaces add a different function at the end of the same file. The
// second land merges onto the first by keeping both, effort side first, and
// says so in KeptBoth.
func TestLandKeptBoth(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Add Refresh")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Add Revoke")
	const seed = "func Login() {\n}\n\nfunc Logout() {\n}\n"
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{"auth.go": s(seed + "\nfunc Refresh() {\n}\n")})
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{"auth.go": s(seed + "\nfunc Revoke() {\n}\n")})

	r1, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1})
	if err != nil || !r1.Landed || len(r1.KeptBoth) != 0 {
		t.Fatalf("first land: %+v %v", r1, err)
	}
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil || !r2.Landed || !r2.Rebased {
		t.Fatalf("second land should merge onto the first: %+v %v", r2, err)
	}
	if strings.Join(r2.KeptBoth, ",") != "auth.go" || strings.Join(r2.AutoMerged, ",") != "auth.go" {
		t.Fatalf("kept both %v, auto-merged %v", r2.KeptBoth, r2.AutoMerged)
	}
	want := seed + "\nfunc Refresh() {\n}\n\nfunc Revoke() {\n}\n"
	if got := e.read(repo, r2.EffortHead)["auth.go"]; got != want {
		t.Fatalf("auth.go = %q, want %q", got, want)
	}
}

// Changing the same line differently is not an insertion: it still conflicts.
func TestLandSameLineStillConflicts(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Login takes ctx")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Login takes opts")
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{"auth.go": s("func Login(ctx) {\n}\n\nfunc Logout() {\n}\n")})
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{"auth.go": s("func Login(opts) {\n}\n\nfunc Logout() {\n}\n")})
	if r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1}); err != nil || !r.Landed {
		t.Fatalf("first land: %+v %v", r, err)
	}
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil || r2.Landed || len(r2.Conflicts) != 1 || len(r2.KeptBoth) != 0 {
		t.Fatalf("expected a conflict: %+v %v", r2, err)
	}
}
