package service_test

// The access matrix: which role each repository operation needs. Two
// organizations, acme (alice) and globex (bob), each person with an agent.
// Bob starts with no access to acme/shop, then gets read, write and admin
// grants. Without access every operation reports "not found" (the
// repository's existence does not leak); with too little it is a permission
// error; with enough it passes the access check.
//
// When you add a service function that takes a repository, add it to ops
// below and to the table in docs/organizations.md. TestAuthzMatrixGitHub
// runs the same operations on a GitHub-backed repository.

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// authzEnv is env plus a second organization.
type authzEnv struct {
	*env
	bob, bobAgent service.Actor
	repo          store.Repo // acme/shop
	eff           store.Effort
	aliceWS       uuid.UUID // a workspace of alice's agent in the effort
	question      service.Note
	n             int
}

func newAuthzEnv(t *testing.T) *authzEnv {
	t.Helper()
	e := &authzEnv{env: newEnv(t)}
	ctx := e.ctx
	bob, err := e.svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob", DisplayName: "Bob", Email: "bob@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	bobAgent, err := e.svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "bob/claude", ParentID: &bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.bob, _ = e.svc.ResolveActor(ctx, bob)
	e.bobAgent, _ = e.svc.ResolveActor(ctx, bobAgent)

	if _, err := e.svc.CreateOrg(ctx, e.alice, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateOrg(ctx, e.bob, "globex", "Globex"); err != nil {
		t.Fatal(err)
	}
	if e.repo, err = e.svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/shop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateRepo(ctx, e.bob, service.CreateRepoInput{Name: "globex/site"}); err != nil {
		t.Fatal(err)
	}
	if e.eff, err = e.svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Checkout"}); err != nil {
		t.Fatal(err)
	}
	ws, err := e.svc.OpenWorkspace(ctx, e.agent, "acme/shop", e.eff.Slug, "Add cart")
	if err != nil {
		t.Fatal(err)
	}
	tree := e.snapshot(e.repo, ws.Workspace.Base, map[string]*string{"cart.go": s("package shop\n")})
	if res, err := e.svc.Land(ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree}); err != nil || !res.Landed {
		t.Fatalf("seed land: %v", err)
	}
	open, err := e.svc.OpenWorkspace(ctx, e.agent, "acme/shop", e.eff.Slug, "Add totals")
	if err != nil {
		t.Fatal(err)
	}
	e.aliceWS = open.Workspace.ID
	// A question to bob in a repository he may not read yet.
	if e.question, err = e.svc.Ask(ctx, e.agent, service.AskInput{Repo: "acme/shop", Effort: e.eff.Slug, Targets: []string{"cart.go"}, Body: "Who owns pricing?", To: "user:bob"}); err != nil {
		t.Fatal(err)
	}
	return e
}

// grant sets bob's direct role on acme/shop; RoleNone revokes it.
func (e *authzEnv) grant(role service.RepoRole) {
	e.t.Helper()
	if role == service.RoleNone {
		if err := e.svc.RevokeRepo(e.ctx, e.alice, "acme/shop", "user:bob"); err != nil && !isCode(err, service.CodeNotFound) {
			e.t.Fatal(err)
		}
		return
	}
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, "acme/shop", "user:bob", role); err != nil {
		e.t.Fatal(err)
	}
}

// workspace opens a workspace for actor directly in the store, so write
// operations on a workspace can be tried at every role.
func (e *authzEnv) workspace(actor service.Actor) uuid.UUID {
	e.t.Helper()
	eff, err := e.svc.Store.EffortByID(e.ctx, e.eff.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	ws, err := e.svc.Store.CreateWorkspace(e.ctx, store.Workspace{EffortID: eff.ID, OwnerID: actor.ID, Intent: "try", Base: eff.Head})
	if err != nil {
		e.t.Fatal(err)
	}
	return ws.ID
}

func isCode(err error, code service.Code) bool {
	var se *service.Error
	return errors.As(err, &se) && se.Code == code
}

type authzOp struct {
	name string
	need service.RepoRole
	run  func(e *authzEnv, a service.Actor) error
}

var authzOps = []authzOp{
	{"GetRepo", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, _, err := e.svc.GetRepo(e.ctx, a, "acme/shop")
		return err
	}},
	{"ListEfforts", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, _, err := e.svc.ListEfforts(e.ctx, a, "acme/shop", "")
		return err
	}},
	{"GetEffort", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.GetEffort(e.ctx, a, "acme/shop", e.eff.Slug)
		return err
	}},
	{"Log", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.Log(e.ctx, a, "acme/shop", e.eff.Slug, 10, true)
		return err
	}},
	{"GetChangeset", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, _, err := e.svc.GetChangeset(e.ctx, a, "acme/shop", e.eff.Base.String())
		return err
	}},
	{"GetWorkspace", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.GetWorkspace(e.ctx, a, e.aliceWS)
		return err
	}},
	{"ListShipments", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ListShipments(e.ctx, a, "acme/shop", e.eff.Slug)
		return err
	}},
	{"ListNotes", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ListNotes(e.ctx, a, service.ListNotesInput{Repo: "acme/shop", Effort: e.eff.Slug})
		return err
	}},
	{"Context", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.Context(e.ctx, a, "acme/shop", e.eff.Slug, []string{"cart.go"})
		return err
	}},
	{"GetNote", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.GetNote(e.ctx, a, e.question.ID.String())
		return err
	}},
	{"SyncBranch", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, _, err := e.svc.SyncBranch(e.ctx, a, "acme/shop", "")
		return err
	}},
	{"CreateEffort", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		e.n++
		_, err := e.svc.CreateEffort(e.ctx, a, service.CreateEffortInput{Repo: "acme/shop", Name: fmt.Sprintf("Effort %d", e.n)})
		return err
	}},
	{"OpenWorkspace", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.OpenWorkspace(e.ctx, a, "acme/shop", e.eff.Slug, "Try something")
		return err
	}},
	{"Land", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		id := e.workspace(a)
		eff, _ := e.svc.Store.EffortByID(e.ctx, e.eff.ID)
		e.n++
		tree := e.snapshot(e.repo, eff.Head, map[string]*string{fmt.Sprintf("f%d.go", e.n): s("package shop\n")})
		res, err := e.svc.Land(e.ctx, a, service.LandInput{WorkspaceID: id, Tree: tree})
		if err == nil && !res.Landed {
			return fmt.Errorf("land did not land: %+v", res.Conflicts)
		}
		return err
	}},
	{"SetBase", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		id := e.workspace(a)
		eff, _ := e.svc.Store.EffortByID(e.ctx, e.eff.ID)
		_, err := e.svc.SetBase(e.ctx, a, id, eff.Head)
		return err
	}},
	{"Abandon", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.Abandon(e.ctx, a, e.workspace(a))
		return err
	}},
	{"PostNote", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.PostNote(e.ctx, a, service.PostNoteInput{Repo: "acme/shop", Effort: e.eff.Slug, Kind: service.NotePlan, Targets: []string{"cart.go"}, Body: "I will change the cart"})
		return err
	}},
	{"Ask", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.Ask(e.ctx, a, service.AskInput{Repo: "acme/shop", Effort: e.eff.Slug, Targets: []string{"cart.go"}, Body: "Is the cart final?"})
		return err
	}},
	{"AnswerNote", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, _, err := e.svc.AnswerNote(e.ctx, a, e.question.ID.String(), "Pricing is mine")
		return err
	}},
	{"PlanShipment", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.PlanShipment(e.ctx, a, "acme/shop", e.eff.Slug, "")
		if isCode(err, service.CodeFailedPrecondition) {
			return nil // past the access check: acme/shop has no git upstream to ship to
		}
		return err
	}},
	{"OpenShipment", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.OpenShipment(e.ctx, a, service.ShipInput{Repo: "acme/shop", Effort: e.eff.Slug})
		if isCode(err, service.CodeFailedPrecondition) {
			return nil // past the access check: this server has no git bridge
		}
		return err
	}},
}

func TestAuthzMatrix(t *testing.T) {
	e := newAuthzEnv(t)
	for _, role := range []service.RepoRole{service.RoleNone, service.RoleRead, service.RoleWrite, service.RoleAdmin} {
		e.grant(role)
		for _, actor := range []service.Actor{e.bob, e.bobAgent} {
			for _, op := range authzOps {
				err := op.run(e, actor)
				switch {
				case role == service.RoleNone:
					if !isCode(err, service.CodeNotFound) {
						t.Errorf("%s by %s without access: want not found, got %v", op.name, actor.Ref(), err)
					}
				case !role.Includes(op.need):
					if !isCode(err, service.CodePermission) {
						t.Errorf("%s by %s with %s: want a permission error (needs %s), got %v", op.name, actor.Ref(), role, op.need, err)
					}
				default:
					if err != nil {
						t.Errorf("%s by %s with %s: %v", op.name, actor.Ref(), role, err)
					}
				}
			}
		}
	}
	// Alice and her agent, in their own organization, may do everything.
	for _, actor := range []service.Actor{e.alice, e.agent} {
		for _, op := range authzOps {
			if err := op.run(e, actor); err != nil {
				t.Errorf("%s by %s in acme: %v", op.name, actor.Ref(), err)
			}
		}
	}
}

// TestAuthzMatrixGitHub is the matrix on a GitHub-backed repository: bob's
// role comes from his GitHub permission (none, read, write, maintain) and
// vx grants are refused. Alice is an admin of the GitHub organization.
func TestAuthzMatrixGitHub(t *testing.T) {
	e := newAuthzEnv(t)
	gh := githubtest.New(t)
	aliceGH, bobGH := gh.AddUser("alice"), gh.AddUser("bob")
	acmeGH := gh.AddOrg("acme")
	gh.SetOrgMember("acme", "alice", "admin")
	gh.SetOrgMember("acme", "bob", "member")
	shopGH := gh.AddRepo("acme/shop", true)
	org, err := e.svc.Store.OrgByHandle(e.ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	inst := installOn(e.env, gh, acmeGH, service.Org{ID: org.ID, Handle: org.Handle})
	e.svc.GitHub.API = gh.Client(t)
	e.repo = backWithGitHub(e.env, e.repo, shopGH, inst)
	linkGitHub(e.env, e.alice, aliceGH)
	linkGitHub(e.env, e.bob, bobGH)
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, "acme/shop", "user:bob", service.RoleRead); !isCode(err, service.CodeFailedPrecondition) {
		t.Errorf("vx grant on a GitHub-backed repository: %v", err)
	}
	for perm, role := range map[string]service.RepoRole{"none": service.RoleNone, "read": service.RoleRead, "write": service.RoleWrite, "maintain": service.RoleAdmin} {
		gh.SetPermission("acme/shop", "bob", perm)
		if err := e.svc.DropGitHubRoles(e.ctx, service.GitHubRoleScope{GitHubRepoID: shopGH.ID}); err != nil {
			t.Fatal(err)
		}
		for _, actor := range []service.Actor{e.bob, e.bobAgent} {
			for _, op := range authzOps {
				err := op.run(e, actor)
				switch {
				case role == service.RoleNone:
					if !isCode(err, service.CodeNotFound) {
						t.Errorf("%s by %s with GitHub %s: want not found, got %v", op.name, actor.Ref(), perm, err)
					}
				case !role.Includes(op.need):
					if !isCode(err, service.CodePermission) {
						t.Errorf("%s by %s with GitHub %s: want a permission error (needs %s), got %v", op.name, actor.Ref(), perm, op.need, err)
					}
				default:
					if err != nil {
						t.Errorf("%s by %s with GitHub %s: %v", op.name, actor.Ref(), perm, err)
					}
				}
			}
		}
	}
	for _, actor := range []service.Actor{e.alice, e.agent} {
		for _, op := range authzOps {
			if err := op.run(e, actor); err != nil {
				t.Errorf("%s by %s, GitHub admin of acme: %v", op.name, actor.Ref(), err)
			}
		}
	}
}

// TestAuthzLists checks that lists only show readable repositories, with
// the caller's role, and that the inbox leaves out questions in
// repositories the caller cannot read.
func TestAuthzLists(t *testing.T) {
	e := newAuthzEnv(t)
	names := func(a service.Actor) map[string]service.RepoRole {
		t.Helper()
		repos, err := e.svc.ListRepos(e.ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]service.RepoRole{}
		for _, r := range repos {
			out[r.Name] = r.Role
		}
		return out
	}
	inboxHas := func(a service.Actor) bool {
		t.Helper()
		in, err := e.svc.GetInbox(e.ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(in.Questions, func(n service.Note) bool { return n.ID == e.question.ID })
	}

	for _, a := range []service.Actor{e.bob, e.bobAgent} {
		got := names(a)
		if _, ok := got["acme/shop"]; ok {
			t.Errorf("%s lists acme/shop without access: %v", a.Ref(), got)
		}
		if got["globex/site"] != service.RoleAdmin {
			t.Errorf("%s: globex/site role = %q, want admin", a.Ref(), got["globex/site"])
		}
	}
	if inboxHas(e.bob) {
		t.Error("bob's inbox shows a question in acme/shop, which he cannot read")
	}
	if got := names(e.alice); got["acme/shop"] != service.RoleAdmin || got["globex/site"] != "" {
		t.Errorf("alice lists %v, want acme/shop as admin and not globex/site", got)
	}

	e.grant(service.RoleRead)
	for _, a := range []service.Actor{e.bob, e.bobAgent} {
		if got := names(a); got["acme/shop"] != service.RoleRead {
			t.Errorf("%s with a read grant: acme/shop role = %q", a.Ref(), got["acme/shop"])
		}
	}
	if !inboxHas(e.bob) {
		t.Error("bob's inbox lacks the question once he can read acme/shop")
	}
	e.grant(service.RoleWrite)
	if got := names(e.bob); got["acme/shop"] != service.RoleWrite {
		t.Errorf("bob with a write grant: acme/shop role = %q", got["acme/shop"])
	}
}
