package service_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func ip(n int) *int { return &n }

// scope sets the scope of alice's agent claude, as alice.
func (e *env) scope(in service.ScopeInput) service.AgentView {
	e.t.Helper()
	v, err := e.svc.SetAgentScope(e.ctx, e.alice, "claude", in)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// tryLand opens a workspace for a on the effort's head, writes files and
// lands them.
func (e *env) tryLand(a service.Actor, repo store.Repo, eff store.Effort, files map[string]*string) (service.LandResult, error) {
	e.t.Helper()
	ws, err := e.svc.OpenWorkspace(e.ctx, a, repo.Name, eff.Slug, "try")
	if err != nil {
		return service.LandResult{}, err
	}
	res, err := e.svc.Land(e.ctx, a, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: e.snapshot(repo, ws.Workspace.Base, files)})
	if err != nil {
		// Keep the workspace budget out of the way of later lands.
		if _, aerr := e.svc.Abandon(e.ctx, a, ws.Workspace.ID); aerr != nil {
			e.t.Fatal(aerr)
		}
	}
	return res, err
}

func wantErr(t *testing.T, err error, c service.Code, parts ...string) {
	t.Helper()
	if code(err) != c {
		t.Fatalf("error = %v (code %d), want code %d", err, code(err), c)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not say %q", err, p)
		}
	}
}

func TestAgentScopePaths(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	v := e.scope(service.ScopeInput{SetAllow: true, Allow: []string{"internal/api/**", " docs/api.md ", "internal/api/**"}, SetDeny: true, Deny: []string{"internal/api/secret/**"}})
	if got := strings.Join(v.Scope.Allow, " "); got != "internal/api/** docs/api.md" {
		t.Errorf("allow = %q, want trimmed and deduplicated", got)
	}
	if v.UpdatedBy != "user:alice" || v.Scope.Unlimited() {
		t.Errorf("view = %+v", v)
	}

	if _, err := e.tryLand(e.agent, repo, eff, map[string]*string{"internal/api/h.go": s("package api\n"), "docs/api.md": s("# API\n")}); err != nil {
		t.Fatalf("land in scope: %v", err)
	}
	eff = e.effort(eff)
	_, err := e.tryLand(e.agent, repo, eff, map[string]*string{"internal/api/h2.go": s("package api\n"), "internal/store/x.go": s("package store\n")})
	wantErr(t, err, service.CodePermission,
		"permission denied: agent:alice/claude may only change internal/api/**, docs/api.md",
		"internal/store/x.go is outside",
		"Ask user:alice to widen it: vx agent scope claude --allow 'internal/api/**' --allow 'docs/api.md' --allow 'internal/store/**'")
	if now := e.effort(eff); now.Head != eff.Head {
		t.Fatal("a refused land moved the effort")
	}
	// Deny wins over allow.
	_, err = e.tryLand(e.agent, repo, eff, map[string]*string{"internal/api/secret/key.go": s("package secret\n")})
	wantErr(t, err, service.CodePermission, "internal/api/secret/key.go is denied by internal/api/secret/**", "--deny ''")
	// Deleting a file is changing it.
	_, err = e.tryLand(e.agent, repo, eff, map[string]*string{"auth.go": nil})
	wantErr(t, err, service.CodePermission, "auth.go is outside")

	// People are never scoped.
	if _, err := e.tryLand(e.alice, repo, eff, map[string]*string{"internal/store/x.go": s("package store\n")}); err != nil {
		t.Fatalf("alice lands: %v", err)
	}
	// Only deny: everything else is allowed.
	e.scope(service.ScopeInput{Clear: true, SetDeny: true, Deny: []string{"gen/**"}})
	eff = e.effort(eff)
	if _, err := e.tryLand(e.agent, repo, eff, map[string]*string{"internal/store/y.go": s("package store\n")}); err != nil {
		t.Fatalf("land outside deny: %v", err)
	}
	_, err = e.tryLand(e.agent, repo, e.effort(eff), map[string]*string{"gen/a.pb.go": s("package gen\n")})
	wantErr(t, err, service.CodePermission, "may change any path except gen/**", "gen/a.pb.go is denied by gen/**")
}

// TestAgentScopeMergedLand checks lands that merge onto newer work: the
// agent's own change is checked, never what others landed meanwhile.
func TestAgentScopeMergedLand(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	e.scope(service.ScopeInput{SetAllow: true, Allow: []string{"api/**"}, MaxLines: ip(3), MaxFiles: ip(1)})

	open := func() (service.WorkspaceView, store.Effort) {
		ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "api")
		if err != nil {
			t.Fatal(err)
		}
		return ws, e.effort(eff)
	}
	ws, before := open()
	// Meanwhile alice lands a big change outside the agent's scope.
	big := strings.Repeat("x\n", 50)
	if _, err := e.tryLand(e.alice, repo, before, map[string]*string{"store/a.go": s(big), "store/b.go": s(big)}); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: e.snapshot(repo, ws.Workspace.Base, map[string]*string{"api/h.go": s("package api\n")})})
	if err != nil || !res.Landed || !res.Rebased {
		t.Fatalf("merged land in scope: %+v %v", res, err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "api/h.go" {
		t.Errorf("landed files = %+v", res.Files)
	}

	// An out-of-scope file in a snapshot on an old base is refused even
	// though the merge would succeed.
	eff = e.effort(eff)
	ws, _ = open()
	if _, err := e.tryLand(e.alice, repo, e.effort(eff), map[string]*string{"store/c.go": s("package store\n")}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: e.snapshot(repo, ws.Workspace.Base, map[string]*string{"store/d.go": s("package store\n")})})
	wantErr(t, err, service.CodePermission, "store/d.go is outside")
}

func TestAgentBudgetLandsPerDay(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	e.svc.Now = func() time.Time { return now }
	repo, eff := e.setup() // the agent's first land today
	e.scope(service.ScopeInput{MaxLandsPerDay: ip(3)})
	for i := range 2 {
		if _, err := e.tryLand(e.agent, repo, e.effort(eff), map[string]*string{fmt.Sprintf("f%d.go", i): s("package f\n")}); err != nil {
			t.Fatalf("land %d: %v", i+2, err)
		}
	}
	v, err := e.svc.GetAgent(e.ctx, e.agent, "")
	if err != nil || v.LandsToday != 3 || v.LandsLeft() != 0 || !v.ResetsAt.Equal(time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("usage = %+v (left %d) %v", v, v.LandsLeft(), err)
	}
	_, err = e.tryLand(e.agent, repo, e.effort(eff), map[string]*string{"f9.go": s("package f\n")})
	wantErr(t, err, service.CodeTooMany, "agent:alice/claude has used all 3 of its lands for today (0 left)",
		"resets at 2026-11-25 00:00 UTC (in 9h00m)", "vx agent scope claude --max-lands-per-day N")
	// Others' lands do not count.
	if _, err := e.tryLand(e.alice, repo, e.effort(eff), map[string]*string{"g.go": s("package g\n")}); err != nil {
		t.Fatal(err)
	}
	// A new UTC day starts from zero.
	now = now.Add(9 * time.Hour)
	if _, err := e.tryLand(e.agent, repo, e.effort(eff), map[string]*string{"f9.go": s("package f\n")}); err != nil {
		t.Fatalf("land on the next day: %v", err)
	}
}

func TestAgentBudgetLinesAndFiles(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	e.scope(service.ScopeInput{MaxLines: ip(4), MaxFiles: ip(2)})
	if _, err := e.tryLand(e.agent, repo, eff, map[string]*string{"a.go": s("a\nb\n"), "b.go": s("c\nd\n")}); err != nil {
		t.Fatalf("land at the limits: %v", err)
	}
	_, err := e.tryLand(e.agent, repo, e.effort(eff), map[string]*string{"c.go": s("1\n"), "d.go": s("2\n"), "e.go": s("3\n")})
	wantErr(t, err, service.CodeTooMany, "this land changes 3 files and agent:alice/claude may change at most 2 per land", "--max-files N")
	// Rewriting two lines is two deletions and two additions.
	_, err = e.tryLand(e.agent, repo, e.effort(eff), map[string]*string{"a.go": s("A\nB\n"), "z.go": s("z\n")})
	wantErr(t, err, service.CodeTooMany, "this land changes 5 lines (added plus deleted) and agent:alice/claude may change at most 4", "--max-lines N")
}

func TestAgentBudgetWorkspaces(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	e.scope(service.ScopeInput{MaxWorkspaces: ip(1)})
	first, err := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "first task")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "second task")
	wantErr(t, err, service.CodeTooMany, "agent:alice/claude already has 1 open workspace and may have at most 1 at once (0 left)",
		`the oldest is "first task", change `+first.Workspace.ChangeID, "--max-workspaces N")
	if v, _ := e.svc.GetAgent(e.ctx, e.alice, "claude"); v.OpenWorkspaces != 1 || v.WorkspacesLeft() != 0 {
		t.Errorf("usage = %+v", v)
	}
	// People are never limited.
	if _, err := e.svc.OpenWorkspace(e.ctx, e.alice, repo.Name, eff.Slug, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Abandon(e.ctx, e.agent, first.Workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "second task"); err != nil {
		t.Fatalf("open after abandoning: %v", err)
	}
}

// TestAgentScopeWhoMaySet: only the agent's person sets scopes; other
// people, superadmins and every agent, the scoped one included, cannot.
func TestAgentScopeWhoMaySet(t *testing.T) {
	e := newEnv(t)
	bob := e.person("bob")
	admin := e.siteAdmin("root")
	adminAgent := e.agentOf(admin, "ops")
	sibling := e.agentOf(e.alice, "docs")
	in := service.ScopeInput{SetAllow: true, Allow: []string{"api/**"}}

	if _, err := e.svc.SetAgentScope(e.ctx, e.alice, "claude", in); err != nil {
		t.Fatalf("alice: %v", err)
	}
	_, err := e.svc.SetAgentScope(e.ctx, bob, "alice/claude", in)
	wantErr(t, err, service.CodePermission, "user:bob cannot change the scope of agent:alice/claude: only user:alice can")
	_, err = e.svc.GetAgent(e.ctx, bob, "alice/claude")
	wantErr(t, err, service.CodePermission, "user:bob cannot see the scope of agent:alice/claude")
	// "claude" from bob means bob/claude, which does not exist.
	_, err = e.svc.SetAgentScope(e.ctx, bob, "claude", in)
	wantErr(t, err, service.CodeNotFound, "no agent bob/claude")

	_, err = e.svc.SetAgentScope(e.ctx, admin, "alice/claude", service.ScopeInput{MaxFiles: ip(5)})
	wantErr(t, err, service.CodePermission, "user:root cannot change the scope of agent:alice/claude: only user:alice can")
	_, err = e.svc.GetAgent(e.ctx, admin, "alice/claude")
	wantErr(t, err, service.CodePermission, "user:root cannot see the scope of agent:alice/claude")
	v, err := e.svc.SetAgentScope(e.ctx, e.alice, "claude", service.ScopeInput{MaxFiles: ip(5)})
	if err != nil || v.UpdatedBy != "user:alice" || v.Scope.MaxFiles != 5 || len(v.Scope.Allow) != 1 {
		t.Fatalf("alice sets budgets: %+v %v", v, err)
	}

	_, err = e.svc.SetAgentScope(e.ctx, e.agent, "", service.ScopeInput{Clear: true})
	wantErr(t, err, service.CodePermission, "agent:alice/claude cannot change scopes, not even its own", "Ask user:alice to run: vx agent scope claude")
	_, err = e.svc.SetAgentScope(e.ctx, e.agent, "claude", service.ScopeInput{Clear: true})
	wantErr(t, err, service.CodePermission, "cannot change scopes")
	_, err = e.svc.SetAgentScope(e.ctx, sibling, "claude", service.ScopeInput{Clear: true})
	wantErr(t, err, service.CodePermission, "cannot change scopes")
	// A site admin's agent is still an agent.
	_, err = e.svc.SetAgentScope(e.ctx, adminAgent, "alice/claude", service.ScopeInput{Clear: true})
	wantErr(t, err, service.CodePermission, "cannot change scopes")
	_, err = e.svc.GetAgent(e.ctx, adminAgent, "alice/claude")
	wantErr(t, err, service.CodePermission, "cannot see the scope")

	// Agents read their own scope, not their siblings'.
	own, err := e.svc.GetAgent(e.ctx, e.agent, "")
	if err != nil || own.Agent.ID != e.agent.ID || own.Owner.Handle != "alice" || own.Scope.MaxFiles != 5 {
		t.Fatalf("own scope: %+v %v", own, err)
	}
	_, err = e.svc.GetAgent(e.ctx, sibling, "claude")
	wantErr(t, err, service.CodePermission, "cannot see the scope")
	_, err = e.svc.GetAgent(e.ctx, e.alice, "")
	wantErr(t, err, service.CodeInvalid, "scopes and budgets are for agents")

	// Bad input says what to write instead.
	_, err = e.svc.SetAgentScope(e.ctx, e.alice, "claude", service.ScopeInput{SetAllow: true, Allow: []string{"/abs/**"}})
	wantErr(t, err, service.CodeInvalid, "--allow", "starts with /")
	_, err = e.svc.SetAgentScope(e.ctx, e.alice, "claude", service.ScopeInput{SetDeny: true, Deny: []string{"src/{a,b}/**"}})
	wantErr(t, err, service.CodeInvalid, "--deny", "only *, ? and ** are supported")
	_, err = e.svc.SetAgentScope(e.ctx, e.alice, "claude", service.ScopeInput{MaxLines: ip(-1)})
	wantErr(t, err, service.CodeInvalid, "--max-lines -1")
	_, err = e.svc.SetAgentScope(e.ctx, e.alice, "", in)
	wantErr(t, err, service.CodeInvalid, "name the agent")

	// Clear goes back to no limits, and the history is in the op log.
	v, err = e.svc.SetAgentScope(e.ctx, e.alice, "claude", service.ScopeInput{Clear: true})
	if err != nil || !v.Scope.Unlimited() {
		t.Fatalf("clear: %+v %v", v, err)
	}
}

// TestAgentScopeNeverWidensRole: on a GitHub-backed repository a scope
// that allows everything gives an agent nothing its person lacks.
func TestAgentScopeNeverWidensRole(t *testing.T) {
	e := newGHEnv(t)
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/api", Name: "Scoped"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetAgentScope(e.ctx, e.bob, "claude", service.ScopeInput{SetAllow: true, Allow: []string{"**"}}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.OpenWorkspace(e.ctx, e.bobAgent, "acme/api", eff.Slug, "try")
	wantErr(t, err, service.CodePermission) // bob has read on GitHub
	_, err = e.svc.OpenWorkspace(e.ctx, e.bobAgent, "acme/secret", "x", "try")
	wantErr(t, err, service.CodeNotFound) // and nothing on acme/secret

	e.gh.SetPermission("acme/api", "bob", "write")
	e.drop(service.GitHubRoleScope{GitHubRepoID: e.apiGH.ID, Reason: "member edited"})
	if _, err := e.svc.SetAgentScope(e.ctx, e.bob, "claude", service.ScopeInput{SetAllow: true, Allow: []string{"api/**"}}); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.svc.Store.EffortByID(e.ctx, eff.ID)
	if _, err := e.tryLand(e.bobAgent, e.api, cur, map[string]*string{"api/h.go": s("package api\n")}); err != nil {
		t.Fatalf("bob's agent lands in scope with write: %v", err)
	}
	cur, _ = e.svc.Store.EffortByID(e.ctx, eff.ID)
	_, err = e.tryLand(e.bobAgent, e.api, cur, map[string]*string{"web/x.go": s("package web\n")})
	wantErr(t, err, service.CodePermission, "agent:bob/claude may only change api/**", "Ask user:bob")
}

// TestAgentScopeRebaseResolution: what an agent writes to resolve a rebase
// conflict must be in its scope; taking a side unchanged is not its edit.
func TestAgentScopeRebaseResolution(t *testing.T) {
	e, repo, eff, c, v := conflicting(t)
	if v.Rebase.State != store.RebaseStopped {
		t.Fatalf("rebase = %+v", v.Rebase)
	}
	e.scope(service.ScopeInput{SetAllow: true, Allow: []string{"limit.go"}})
	cur, _ := e.svc.Store.GetChangeset(e.ctx, repo.ID, v.Rebase.NewHead)
	merged := e.treeWith(repo, cur.Hash, map[string]*string{"auth.go": s("func SignIn() {\n}\n\nfunc LogIn() {\n}\n\nfunc Logout() {\n}\n")})
	_, err := e.svc.ContinueRebase(e.ctx, e.agent, service.ContinueInput{Repo: repo.Name, Effort: eff.Slug, StoppedAt: c.Hash, Tree: merged})
	wantErr(t, err, service.CodePermission, "agent:alice/claude may only change limit.go", "auth.go is outside")
	if got, _, _ := e.svc.GetRebase(e.ctx, e.alice, repo.Name, eff.Slug); got.Rebase.State != store.RebaseStopped {
		t.Fatalf("a refused resolution moved the rebase: %+v", got.Rebase)
	}
	// Taking the change's version as it is passes.
	theirs := e.treeWith(repo, cur.Hash, map[string]*string{"auth.go": s("func SignIn() {\n}\n\nfunc Logout() {\n}\n")})
	done, err := e.svc.ContinueRebase(e.ctx, e.agent, service.ContinueInput{Repo: repo.Name, Effort: eff.Slug, StoppedAt: c.Hash, Tree: theirs})
	if err != nil || done.Rebase.State != store.RebaseDone {
		t.Fatalf("continue with the change's side: %+v %v", done.Rebase, err)
	}
}
