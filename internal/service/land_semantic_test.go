package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

const semAuth = "package auth\n\n// Login signs in.\nfunc Login(user string) error {\n\treturn nil\n}\n\n// Logout signs out.\nfunc Logout() {\n}\n"

// One workspace deletes Logout, the other edits Login's doc right above
// it: the lines conflict, the declarations do not, and the land says the
// file merged by declaration.
func TestLandMergedByDecl(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	seed, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Seed")
	if r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: seed.Workspace.ID, Tree: e.snapshot(repo, seed.Workspace.Base, map[string]*string{"auth/auth.go": s(semAuth)})}); err != nil || !r.Landed {
		t.Fatalf("seed: %+v %v", r, err)
	}
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Drop Login")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Document Logout")
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{"auth/auth.go": s("package auth\n\n// Logout signs out.\nfunc Logout() {\n}\n")})
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{"auth/auth.go": s(strings.Replace(semAuth, "// Logout signs out.", "// Logout signs the user out\n// everywhere.", 1))})
	if r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1}); err != nil || !r.Landed {
		t.Fatalf("first land: %+v %v", r, err)
	}
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil || !r2.Landed || !r2.Rebased {
		t.Fatalf("second land should merge by declaration: %+v %v", r2, err)
	}
	if strings.Join(r2.MergedByDecl, ",") != "auth/auth.go" || strings.Join(r2.AutoMerged, ",") != "auth/auth.go" || len(r2.KeptBoth) != 0 {
		t.Fatalf("merged by decl %v, auto %v, kept both %v", r2.MergedByDecl, r2.AutoMerged, r2.KeptBoth)
	}
	want := "package auth\n\n// Logout signs the user out\n// everywhere.\nfunc Logout() {\n}\n"
	if got := e.read(repo, r2.EffortHead)["auth/auth.go"]; got != want {
		t.Fatalf("auth/auth.go = %q, want %q", got, want)
	}
}

// One workspace changes Login's signature; the other adds a caller with the
// old shape in another file of the package. Both land; the second gets a
// signature warning, stored with the change.
func TestLandSignatureWarning(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	seed, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Seed")
	if r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: seed.Workspace.ID, Tree: e.snapshot(repo, seed.Workspace.Base, map[string]*string{"auth/auth.go": s(semAuth)})}); err != nil || !r.Landed {
		t.Fatalf("seed: %+v %v", r, err)
	}
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Login takes ctx")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Session refresh")
	t1 := e.snapshot(repo, w1.Workspace.Base, map[string]*string{"auth/auth.go": s(strings.Replace(semAuth, "func Login(user string) error", "func Login(ctx context.Context, user string) error", 1))})
	t2 := e.snapshot(repo, w2.Workspace.Base, map[string]*string{"auth/session.go": s("package auth\n\nfunc Refresh(user string) error {\n\treturn Login(user)\n}\n")})
	if r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: t1}); err != nil || !r.Landed {
		t.Fatalf("first land: %+v %v", r, err)
	}
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: t2})
	if err != nil || !r2.Landed {
		t.Fatalf("second land: %+v %v", r2, err)
	}
	want := "auth.Login now takes (ctx context.Context, user string) (changed in the effort); auth/session.go:4 calls Login(user) (added by this land)"
	if len(r2.Warnings) != 1 || r2.Warnings[0].Kind != "signature" || r2.Warnings[0].Detail != want {
		t.Fatalf("warnings = %+v", r2.Warnings)
	}
	if ws := r2.Changeset.Warnings; len(ws) != 1 || ws[0].Kind != "signature" || strings.Join(ws[0].Files, ",") != "auth/auth.go,auth/session.go" {
		t.Fatalf("stored warnings = %+v", ws)
	}
}

// Two efforts: one renames a function, the other's new code in the same
// package calls the old name. The forecast says so on both boards.
func TestOverlapForecastRemovedName(t *testing.T) {
	e := newOverlapEnv(t, nil)
	e.landIn(e.a, "Rename Login", map[string]*string{"auth.go": s(strings.Replace(authBase, "func Login()", "func SignIn()", 1))})
	e.landIn(e.b, "Audit logins", map[string]*string{"audit.go": s("package auth\n\nfunc Audit() {\n\tLogin()\n}\n")})
	o := e.one(e.a, e.b)
	if o.Status != service.OverlapShared || len(o.Warnings) != 1 || o.Warnings[0].Kind != "removed" {
		t.Fatalf("forecast = %+v", o)
	}
	e.wantNote(e.b, "Shipping both merges cleanly, but: func auth.Login was removed (renamed to auth.SignIn?) (in effort ", "audit.go:4 still uses it: Login()")
}
