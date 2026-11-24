package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
)

// A question to an agent that is not working tells the asker at once and
// goes to the agent's person, under "For your agents"; once the agent
// works again, questions reach it as before.
func TestAskAgentNotWorking(t *testing.T) {
	b := newBoardEnv(t)
	b.now = time.Now() // token use and workspace times come from the database clock

	q := b.ask(b.agent, "bob/backend", "Which header carries the client IP?", 4*time.Hour, "limit.Allow")
	if q.ToAway == nil || q.ToAway.LastSeen != nil || q.CopiedTo == nil || q.CopiedTo.ID != b.bob.ID {
		t.Fatalf("question to an agent never seen = %+v", q)
	}
	if got, want := service.AwayNotice(q, b.now), "agent:bob/backend is not working right now (never seen); the question also goes to user:bob."; got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	in, err := b.svc.GetInbox(b.ctx, b.bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.ForAgents) != 1 || in.ForAgents[0].ID != q.ID || in.ForAgents[0].CopiedTo == nil || len(in.Questions) != 0 {
		t.Fatalf("bob's inbox = %+v", in)
	}
	// The agent itself still has it.
	if in, _ := b.svc.GetInbox(b.ctx, b.bobBE); len(in.Questions) != 1 || in.Questions[0].ID != q.ID {
		t.Fatalf("bob/backend's inbox = %+v", in)
	}

	// It used its token 40 minutes ago: "last seen 40 min ago".
	tok, err := b.svc.Store.CreateToken(b.ctx, b.bobBE.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.svc.Store.Authenticate(b.ctx, tok); err != nil {
		t.Fatal(err)
	}
	b.now = time.Now().Add(40 * time.Minute)
	q2 := b.ask(b.agent, "bob/backend", "And the user id?", 4*time.Hour)
	if got := service.AwayNotice(q2, b.now); !strings.Contains(got, "(last seen 40 min ago); the question also goes to user:bob.") {
		t.Fatalf("notice = %q", got)
	}

	// A token use within 15 minutes, or an open workspace, means working:
	// no notice, no copy, the question is bob's like any other to his agents.
	b.now = time.Now().Add(5 * time.Minute)
	if q3 := b.ask(b.agent, "bob/backend", "Working now?", 0); q3.ToAway != nil || q3.CopiedTo != nil || service.AwayNotice(q3, b.now) != "" {
		t.Fatalf("question to a working agent = %+v", q3)
	}
	b.now = time.Now().Add(90 * time.Minute)
	if _, err := b.svc.OpenWorkspace(b.ctx, b.bobBE, "shop-api", b.effort.Slug, "Limits"); err != nil {
		t.Fatal(err)
	}
	q4 := b.ask(b.agent, "bob/backend", "Still there?", 0)
	if q4.ToAway != nil || q4.CopiedTo != nil {
		t.Fatalf("question to an agent with an open workspace = %+v", q4)
	}
	in, err = b.svc.GetInbox(b.ctx, b.bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.ForAgents) != 2 || len(in.Questions) != 2 {
		t.Fatalf("bob's inbox: %d for agents, %d questions", len(in.ForAgents), len(in.Questions))
	}
	// A workspace idle for two hours does not count.
	b.now = time.Now().Add(3 * time.Hour)
	if q5 := b.ask(b.agent, "bob/backend", "Hello?", 0); q5.ToAway == nil || q5.CopiedTo == nil {
		t.Fatalf("question to an agent whose workspace is idle = %+v", q5)
	}
	// People and the asker's own person are never told "not working".
	if q6 := b.ask(b.agent, "user:bob", "Bob?", 0); q6.ToAway != nil || q6.CopiedTo != nil {
		t.Fatalf("question to a person = %+v", q6)
	}
	if q7 := b.ask(b.agent, "tests", "Tests?", 0); q7.ToAway == nil || q7.CopiedTo == nil || q7.CopiedTo.ID != b.alice.ID {
		t.Fatalf("question to another agent of the asker's person = %+v", q7)
	}
	// A person asking their own agent is not copied on it.
	if q8 := b.ask(b.alice, "tests", "Tests, again?", 0); q8.ToAway == nil || q8.CopiedTo != nil {
		t.Fatalf("question by a person to their own agent = %+v", q8)
	}
}

// A workspace claim lapses when its workspace has done nothing for two
// hours, and its owner gets a note; activity keeps it.
func TestClaimsLapseWhenWorkspaceIdle(t *testing.T) {
	b := newBoardEnv(t)
	start := time.Now()
	b.now = start
	ws, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", b.effort.Slug, "Limit Login")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := b.claim(b.agent, ws.Workspace.ID.String(), "limiting", "internal/http/limit.go", "auth.Login")

	b.now = start.Add(time.Hour + 30*time.Minute)
	if cs := b.listClaims(b.bob); len(cs) != 1 || cs[0].ID != c.ID {
		t.Fatalf("claims after 90 idle minutes = %+v", cs)
	}
	// vx status reports its files: activity.
	if err := b.svc.ReportChanges(b.ctx, b.agent, ws.Workspace.ID, []string{"internal/http/limit.go"}); err != nil {
		t.Fatal(err)
	}
	b.now = start.Add(3 * time.Hour)
	if cs := b.listClaims(b.bob); len(cs) != 1 {
		t.Fatalf("claims 90 minutes after the last activity = %+v", cs)
	}
	b.now = start.Add(3*time.Hour + 31*time.Minute)
	if cs := b.listClaims(b.bob); len(cs) != 0 {
		t.Fatalf("claims of a workspace idle for 2h = %+v", cs)
	}
	in, err := b.svc.GetInbox(b.ctx, b.agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Notes) != 1 || in.Notes[0].Kind != service.NoteClaim || !in.Notes[0].System ||
		!strings.Contains(in.Notes[0].Body, "Your claim on auth.Login, internal/http/limit.go lapsed: workspace "+ws.Workspace.ChangeID) ||
		!strings.Contains(in.Notes[0].Body, "for 2 hours") || !strings.Contains(in.Notes[0].Body, "vx claim auth.Login internal/http/limit.go") {
		t.Fatalf("owner's inbox = %+v", in.Notes)
	}
	// Once: reading again posts nothing more.
	b.listClaims(b.bob)
	if in, _ := b.svc.GetInbox(b.ctx, b.agent); len(in.Notes) != 1 {
		t.Fatalf("lapse noted twice: %+v", in.Notes)
	}
	// A claim not tied to a workspace keeps its own expiry.
	b.now = start.Add(5 * time.Hour)
	free, _ := b.claim(b.agent, "", "", "auth.Logout")
	b.now = start.Add(5*time.Hour + 20*time.Minute)
	if cs := b.listClaims(b.bob); len(cs) != 1 || cs[0].ID != free.ID {
		t.Fatalf("free claims = %+v", cs)
	}
}

// vx claim --moving: a land by someone else that changes the code being
// moved names the new location, the mover is told, and the claim outlives
// the land of the move.
func TestMovingClaimWarnsLanders(t *testing.T) {
	b := newBoardEnv(t)
	login := "package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {}\n"
	b.land(b.agent, "Add auth", map[string]*string{"auth/auth.go": s(login)})

	ws, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", b.effort.Slug, "Move Login")
	if err != nil {
		t.Fatal(err)
	}
	mv, _, err := b.svc.MakeClaim(b.ctx, b.agent, service.ClaimInput{Repo: "shop-api", Effort: b.effort.Slug,
		Targets: []string{"auth.Login"}, MovingTo: "auth/login.go", WorkspaceID: ws.Workspace.ID.String()})
	if err != nil || mv.MovingTo != "auth/login.go" {
		t.Fatalf("moving claim = %+v %v", mv, err)
	}
	_, _, err = b.svc.MakeClaim(b.ctx, b.agent, service.ClaimInput{Repo: "shop-api", Effort: b.effort.Slug, Targets: []string{"auth.Login"}, MovingTo: "auth.Login"})
	wantCode(t, err, service.CodeInvalid, "moving to itself")

	landBob := func(intent, body string) service.LandResult {
		t.Helper()
		w, err := b.svc.OpenWorkspace(b.ctx, b.bobBE, "shop-api", b.effort.Slug, intent)
		if err != nil {
			t.Fatal(err)
		}
		res, err := b.svc.Land(b.ctx, b.bobBE, service.LandInput{WorkspaceID: w.Workspace.ID,
			Tree: b.snapshot(b.repo, w.Workspace.Base, map[string]*string{"auth/auth.go": s(body)})})
		if err != nil || !res.Landed {
			t.Fatalf("land %s: %+v %v", intent, res, err)
		}
		return res
	}
	// A change to Logout is not a change to the code being moved.
	if res := landBob("Logout", strings.Replace(login, "func Logout() {}", "func Logout() { clear() }", 1)); len(res.OverlappingClaims) != 0 {
		t.Fatalf("a change elsewhere in the file: %+v", res.OverlappingClaims)
	}
	res := landBob("Limit Login", strings.Replace(strings.Replace(login, "check()", "limit()", 1), "func Logout() {}", "func Logout() { clear() }", 1))
	if len(res.OverlappingClaims) != 1 || res.OverlappingClaims[0].ID != mv.ID || res.OverlappingClaims[0].MovingTo != "auth/login.go" {
		t.Fatalf("land over the moving claim: %+v", res.OverlappingClaims)
	}
	in, err := b.svc.GetInbox(b.ctx, b.agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Notes) != 1 || in.Notes[0].Kind != service.NoteClaim ||
		!strings.Contains(in.Notes[0].Body, "agent:bob/backend landed a change to auth.Login, which you are moving to auth/login.go") {
		t.Fatalf("mover's inbox = %+v", in.Notes)
	}

	// The move lands (after a sync); the claim stays, without the workspace.
	if _, err := b.svc.SetBase(b.ctx, b.agent, ws.Workspace.ID, b.effortHead()); err != nil {
		t.Fatal(err)
	}
	moved, err := b.svc.Land(b.ctx, b.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: b.snapshot(b.repo, b.effortHead(), map[string]*string{
		"auth/auth.go":  s("package auth\n\nfunc Logout() { clear() }\n"),
		"auth/login.go": s("package auth\n\nfunc Login() {\n\tlimit()\n}\n"),
	})})
	if err != nil || !moved.Landed {
		t.Fatalf("the move: %+v %v", moved, err)
	}
	cs := b.listClaims(b.bob, "auth.Login")
	if len(cs) != 1 || cs[0].ID != mv.ID || cs[0].WorkspaceID != nil || cs[0].ExpiresAt.Before(b.now.Add(service.MovedClaimTTL)) {
		t.Fatalf("moving claim after the move = %+v", cs)
	}
	b.now = b.now.Add(service.MovedClaimTTL + time.Minute)
	if cs := b.listClaims(b.bob, "auth.Login"); len(cs) != 0 {
		t.Fatalf("moving claim long after the move = %+v", cs)
	}
}

func (b *boardEnv) effortHead() hash.Hash {
	b.t.Helper()
	e, err := b.svc.Store.EffortByID(b.ctx, b.effort.ID)
	if err != nil {
		b.t.Fatal(err)
	}
	return e.Head
}
