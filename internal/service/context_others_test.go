package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// landIn lands files in a new workspace of actor's in an effort of repo.
func landIn(e *env, actor service.Actor, repo store.Repo, slug, intent string, files map[string]*string) {
	e.t.Helper()
	ws, err := e.svc.OpenWorkspace(e.ctx, actor, repo.Name, slug, intent)
	if err != nil {
		e.t.Fatal(err)
	}
	tree := e.snapshot(repo, ws.Workspace.Base, files)
	res, err := e.svc.Land(e.ctx, actor, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
	if err != nil || !res.Landed {
		e.t.Fatalf("land %s: %+v %v", intent, res, err)
	}
}

func otherSlugs(os []service.OtherEffortActivity) string {
	var out []string
	for _, o := range os {
		out = append(out, o.Effort.Slug)
	}
	return strings.Join(out, ",")
}

// Two efforts in one repository: alice's agent in "Rate limiting" changes
// and claims x.go and posts an interface on pkg.Func; bob's agent in
// "Auth" asking about x.go sees all of it, and without targets sees the
// effort that changed the same files. Unrelated targets, closed efforts and
// people without read on the repository see nothing.
func TestContextOtherEfforts(t *testing.T) {
	b := newBoardEnv(t)
	auth, err := b.svc.CreateEffort(b.ctx, b.alice, service.CreateEffortInput{Repo: "shop-api", Name: "Auth"})
	if err != nil {
		t.Fatal(err)
	}
	x := "package pkg\n\nfunc Func() {}\n"
	landIn(b.env, b.agent, b.repo, b.effort.Slug, "Add Func", map[string]*string{"x.go": &x, "only-a.go": &x})
	open, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", b.effort.Slug, "Rework Func")
	if err != nil {
		t.Fatal(err)
	}
	b.claim(b.agent, open.Workspace.ID.String(), "reworking Func", "x.go")
	b.post(b.agent, service.NoteInterface, "func Func() error", "pkg.Func")
	b.post(b.agent, service.NoteDecision, "README stays short", "README.md")
	b.post(b.agent, service.NotePlan, "a plan is not shown to other efforts", "x.go")

	ctxOf := func(a service.Actor, slug string, targets ...string) service.NoteContext {
		t.Helper()
		c, err := b.svc.Context(b.ctx, a, "shop-api", slug, targets)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// bob's agent, in Auth, about to change x.go.
	c := ctxOf(b.bobBE, auth.Slug, "x.go")
	if len(c.OtherEfforts) != 1 {
		t.Fatalf("other efforts = %q", otherSlugs(c.OtherEfforts))
	}
	o := c.OtherEfforts[0]
	if o.Effort.ID != b.effort.ID || o.Owner.Ref() != "user:alice" || strings.Join(o.ChangedPaths, ",") != "x.go" {
		t.Errorf("other effort = %s owner %s changed %q", o.Effort.Slug, o.Owner.Ref(), o.ChangedPaths)
	}
	if len(o.Claims) != 1 || o.Claims[0].Reason != "reworking Func" || o.Claims[0].Owner.Ref() != "agent:alice/claude" {
		t.Errorf("claims = %+v", o.Claims)
	}
	// The symbol note comes along (vx cannot tell which file pkg.Func is
	// in); the decision on README.md and the plan do not.
	if strings.Join(ids(o.Notes), "|") != "func Func() error" || o.Notes[0].EffortSlug != b.effort.Slug {
		t.Errorf("notes = %q", ids(o.Notes))
	}
	if len(o.Working) != 1 || o.Working[0].Workspace.ID != open.Workspace.ID {
		t.Errorf("working = %+v", o.Working)
	}
	if len(c.Notes) != 0 || len(c.Claims) != 0 {
		t.Errorf("the effort's own board has the other effort's notes or claims: %+v %+v", c.Notes, c.Claims)
	}

	// Asking about the symbol finds the interface alone.
	c = ctxOf(b.bobBE, auth.Slug, "pkg")
	if len(c.OtherEfforts) != 1 || len(c.OtherEfforts[0].ChangedPaths) != 0 || len(c.OtherEfforts[0].Claims) != 0 || len(c.OtherEfforts[0].Notes) != 1 {
		t.Errorf("context pkg = %+v", c.OtherEfforts)
	}
	// Nothing matching: left out.
	if c := ctxOf(b.bobBE, auth.Slug, "other.go"); len(c.OtherEfforts) != 0 {
		t.Errorf("context other.go = %q", otherSlugs(c.OtherEfforts))
	}
	// The other way round: Rate limiting sees nothing of Auth, which has
	// done nothing yet.
	if c := ctxOf(b.agent, b.effort.Slug, "x.go"); len(c.OtherEfforts) != 0 {
		t.Errorf("rate limiting sees %q", otherSlugs(c.OtherEfforts))
	}

	// Without targets: the efforts that changed files Auth changed.
	if c := ctxOf(b.bobBE, auth.Slug); len(c.OtherEfforts) != 0 {
		t.Errorf("no changes in Auth yet, but other efforts %q", otherSlugs(c.OtherEfforts))
	}
	y := "package pkg\n\nfunc Func() error { return nil }\n"
	landIn(b.env, b.bobBE, b.repo, auth.Slug, "Change Func", map[string]*string{"x.go": &y, "only-b.go": &y})
	c = ctxOf(b.bobBE, auth.Slug)
	if len(c.OtherEfforts) != 1 || strings.Join(c.OtherEfforts[0].ChangedPaths, ",") != "x.go" ||
		strings.Join(ids(c.OtherEfforts[0].Notes), "|") != "README stays short|func Func() error" || len(c.OtherEfforts[0].Claims) != 1 {
		t.Errorf("context without targets = %+v", c.OtherEfforts)
	}
	if c := ctxOf(b.agent, b.effort.Slug); len(c.OtherEfforts) != 1 || c.OtherEfforts[0].Effort.ID != auth.ID || c.OtherEfforts[0].Owner.Ref() != "user:alice" {
		t.Errorf("rate limiting without targets = %q", otherSlugs(c.OtherEfforts))
	}

	// Someone without read on the repository sees nothing at all.
	carol := b.person("carol")
	if _, err := b.svc.Context(b.ctx, carol, "shop-api", auth.Slug, []string{"x.go"}); code(err) != service.CodeNotFound {
		t.Errorf("carol's context: %v", err)
	}

	// Closed and shipped efforts are left out.
	for _, st := range []store.EffortState{store.EffortAbandoned, store.EffortMerged, store.EffortShipping} {
		if err := b.svc.Store.SetEffortState(b.ctx, b.effort.ID, st); err != nil {
			t.Fatal(err)
		}
		if c := ctxOf(b.bobBE, auth.Slug, "x.go"); len(c.OtherEfforts) != 0 {
			t.Errorf("%s effort still shown: %q", st, otherSlugs(c.OtherEfforts))
		}
	}
}

// On a GitHub-backed repository the same holds with roles from GitHub:
// bob (read on acme/api from GitHub) sees the other effort, carol (not in
// acme) sees nothing.
func TestContextOtherEffortsGitHub(t *testing.T) {
	e := newGHEnv(t)
	one, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/api", Name: "Rate limits"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/api", Name: "Auth"})
	if err != nil {
		t.Fatal(err)
	}
	x := "package api\n"
	landIn(e.env, e.agent, e.api, one.Slug, "Add x", map[string]*string{"x.go": &x})
	if _, err := e.svc.PostNote(e.ctx, e.agent, service.PostNoteInput{Repo: "acme/api", Effort: one.Slug, Kind: service.NoteInterface, Targets: []string{"api.Func"}, Body: "func Func()"}); err != nil {
		t.Fatal(err)
	}
	c, err := e.svc.Context(e.ctx, e.bob, "acme/api", two.Slug, []string{"x.go"})
	if err != nil || len(c.OtherEfforts) != 1 || c.OtherEfforts[0].Effort.ID != one.ID || len(c.OtherEfforts[0].Notes) != 1 {
		t.Fatalf("bob's context = %+v, %v", c.OtherEfforts, err)
	}
	for _, a := range []service.Actor{e.carol, e.agentOf(e.carol, "claude")} {
		if _, err := e.svc.Context(e.ctx, a, "acme/api", two.Slug, []string{"x.go"}); code(err) != service.CodeNotFound {
			t.Errorf("%s's context: %v", a.Ref(), err)
		}
	}
}
