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
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/hash"
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
	rules         bool                // acme/shop has a vx.toml with a human policy on db/**
	linkRoot      func(service.Actor) // links the superadmin to GitHub, for the GitHub matrix
	frozenReview  string              // when set, pendingReview returns it (acme is deleted: no new reviews)
	checkID       string              // a check result in acme/shop (checkResult)
}

// checkResult stores a check result with an artifact in acme/shop once
// (as a land's --run result would be stored) and returns its id.
func (e *authzEnv) checkResult() string {
	e.t.Helper()
	if e.checkID != "" {
		return e.checkID
	}
	hs, err := e.svc.PutObjects(e.ctx, e.alice, "acme/shop", []service.Object{{Kind: hash.KindBlob, Data: []byte("png")}})
	if err != nil {
		e.t.Fatal(err)
	}
	eff, _ := e.svc.Store.EffortByID(e.ctx, e.eff.ID)
	head, _ := e.svc.Store.GetChangeset(e.ctx, e.repo.ID, eff.Head)
	r, err := e.svc.Store.InsertCheckResult(e.ctx, store.CheckResult{RepoID: e.repo.ID, EffortID: &eff.ID, ChangeID: head.ChangeID, Tree: head.Tree,
		CheckName: "e2e", Status: store.CheckPass, Reporter: e.agent.Ref(), Source: store.CheckSourceReport,
		Artifacts: []store.CheckArtifact{{Name: "shot.png", ContentType: "image/png", Hash: hs[0], Size: 3}}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.checkID = r.ID.String()
	return e.checkID
}

// pendingReview lands a file under a human landing policy as alice's agent
// and returns the pending review's id.
func (e *authzEnv) pendingReview() string {
	e.t.Helper()
	defer e.ungated()()
	if e.frozenReview != "" {
		return e.frozenReview
	}
	eff, err := e.svc.Store.EffortByID(e.ctx, e.eff.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if !e.rules {
		e.rules = true
		e.land(e.repo, eff, "Policy", map[string]*string{"vx.toml": s("[[policy]]\npaths = [\"db/**\"]\nmode = \"human\"\n")}, nil)
		eff, _ = e.svc.Store.EffortByID(e.ctx, e.eff.ID)
	}
	ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, "acme/shop", eff.Slug, "Migration")
	if err != nil {
		e.t.Fatal(err)
	}
	e.n++
	tree := e.snapshot(e.repo, ws.Workspace.Base, map[string]*string{fmt.Sprintf("db/%03d.sql", e.n): s("x\n")})
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
	if err != nil || res.Review == nil {
		e.t.Fatalf("pending review: %+v %v", res, err)
	}
	return res.Review.ID.String()
}

// ungated switches the data processing agreement step off until the
// returned function runs, for setting up what an operation acts on.
func (e *authzEnv) ungated() func() {
	keep := e.svc.Agreement
	e.svc.Agreement = service.AgreementSettings{}
	return func() { e.svc.Agreement = keep }
}

// pastAccess turns the refusal of an agent's decision into success: the
// call got past the access check (agents never decide reviews).
func pastAccess(a service.Actor, err error) error {
	if a.Kind == store.KindAgent && isCode(err, service.CodePermission) && strings.Contains(err.Error(), "only people approve") {
		return nil
	}
	return err
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
	{"ShipmentsCI", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ShipmentsCI(e.ctx, a, "acme/shop", e.eff.Slug)
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
	{"ListOverlaps", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ListOverlaps(e.ctx, a, "acme/shop", e.eff.Slug)
		return err
	}},
	{"ListOverlapsRepo", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ListOverlaps(e.ctx, a, "acme/shop", "")
		return err
	}},
	{"EffortOverlaps", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.EffortOverlaps(e.ctx, a, "acme/shop", e.eff.Slug)
		return err
	}},
	{"OverlapCounts", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.OverlapCounts(e.ctx, a, "acme/shop")
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
	{"CloseEffort", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		e.n++
		restore := e.ungated()
		eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: fmt.Sprintf("To close %d", e.n)})
		restore()
		if err != nil {
			return err
		}
		_, err = e.svc.CloseEffort(e.ctx, a, "acme/shop", eff.Slug, store.EffortAbandoned)
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
	{"ReportChanges", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		return e.svc.ReportChanges(e.ctx, a, e.workspace(a), []string{"cart.go"})
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
	{"GetRules", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.GetRules(e.ctx, a, "acme/shop", e.eff.Slug)
		return err
	}},
	{"ListReservations", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ListReservations(e.ctx, a, "acme/shop", "")
		return err
	}},
	{"Reserve", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, _, err := e.svc.Reserve(e.ctx, a, "acme/shop", e.eff.Slug, "migration")
		if isCode(err, service.CodeFailedPrecondition) {
			return nil // past the access check: acme/shop has no vx.toml
		}
		return err
	}},
	{"ReportCheck", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ReportCheck(e.ctx, a, service.ReportInput{Repo: "acme/shop", Effort: e.eff.Slug, Name: "unit", Status: "pass"})
		if isCode(err, service.CodeNotFound) && strings.Contains(err.Error(), `no check "unit" in vx.toml`) {
			return nil // past the access check: acme/shop defines no such check
		}
		return err
	}},
	{"CheckStatus", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.CheckStatus(e.ctx, a, service.CheckStatusInput{Repo: "acme/shop", Effort: e.eff.Slug})
		return err
	}},
	{"CheckResults", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.CheckResults(e.ctx, a, "acme/shop", e.eff.Slug, "", hash.Zero)
		return err
	}},
	{"GetCheckResult", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.GetCheckResult(e.ctx, a, e.checkResult())
		return err
	}},
	{"CheckArtifact", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, _, err := e.svc.CheckArtifact(e.ctx, a, e.checkResult(), "shot.png")
		return err
	}},
	{"PlanShipment", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.PlanShipment(e.ctx, a, "acme/shop", e.eff.Slug, "")
		if isCode(err, service.CodeFailedPrecondition) && !errors.Is(err, service.ErrAgreementRequired) {
			return nil // past the access check: acme/shop has no git upstream to ship to
		}
		return err
	}},
	{"ListReviews", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ListReviews(e.ctx, a, service.ReviewQuery{Repo: "acme/shop", Effort: e.eff.Slug})
		return err
	}},
	{"GetReview", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.GetReview(e.ctx, a, e.pendingReview())
		return err
	}},
	{"ApproveReview", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ApproveReview(e.ctx, a, e.pendingReview())
		return pastAccess(a, err)
	}},
	{"RejectReview", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.RejectReview(e.ctx, a, e.pendingReview(), "not now")
		return pastAccess(a, err)
	}},
	{"PlanSplit", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.PlanSplit(e.ctx, a, service.SplitInput{Repo: "acme/shop", Effort: e.eff.Slug, Mode: service.SplitStacked})
		if isCode(err, service.CodeFailedPrecondition) && !errors.Is(err, service.ErrAgreementRequired) {
			return nil // past the access check: acme/shop has no git upstream to ship to
		}
		return err
	}},
	{"OpenShipment split", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.OpenShipment(e.ctx, a, service.ShipInput{Repo: "acme/shop", Effort: e.eff.Slug, Split: service.SplitIndependent})
		if isCode(err, service.CodeFailedPrecondition) && !errors.Is(err, service.ErrAgreementRequired) {
			return nil // past the access check: this server has no git bridge
		}
		return err
	}},
	{"OpenShipment", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.OpenShipment(e.ctx, a, service.ShipInput{Repo: "acme/shop", Effort: e.eff.Slug})
		if isCode(err, service.CodeFailedPrecondition) && !errors.Is(err, service.ErrAgreementRequired) {
			return nil // past the access check: this server has no git bridge
		}
		return err
	}}, {"SetRepoHost", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.SetRepoHost(e.ctx, a, service.SetRepoHostInput{Repo: "acme/shop", RotateWebhookSecret: true})
		if isCode(err, service.CodeFailedPrecondition) {
			return nil // past the access check: acme/shop is not on GitLab, Gitea, Forgejo or Bitbucket
		}
		return err
	}},
	// The check queue (#47).
	{"ListCheckJobs", service.RoleRead, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ListCheckJobs(e.ctx, a, service.ListJobsInput{Repo: "acme/shop", Effort: e.eff.Slug})
		return err
	}},
	{"NextCheckJob", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.NextCheckJob(e.ctx, a, service.NextJobInput{Repo: "acme/shop"})
		if a.Kind == store.KindAgent && isCode(err, service.CodeFailedPrecondition) {
			return nil // past the access check: the agent serves no runner kinds
		}
		return err
	}},
	{"RequestCheck", service.RoleWrite, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.RequestCheck(e.ctx, a, service.RequestCheckInput{Repo: "acme/shop", Effort: e.eff.Slug, Check: "e2e"})
		if isCode(err, service.CodeFailedPrecondition) {
			return nil // past the access check: acme/shop's vx.toml has no check e2e
		}
		return err
	}},
	{"ListCITokens", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		_, err := e.svc.ListCITokens(e.ctx, a, "acme/shop", true)
		return err
	}},
	{"CreateCIToken", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		e.n++
		_, _, err := e.svc.CreateCIToken(e.ctx, a, "acme/shop", fmt.Sprintf("ci-%d", e.n), []string{"unit"})
		if a.Kind == store.KindAgent && isCode(err, service.CodePermission) && strings.Contains(err.Error(), "only people create CI tokens") {
			return nil // past the access check: agents never mint tokens
		}
		return err
	}},
	{"RevokeCIToken", service.RoleAdmin, func(e *authzEnv, a service.Actor) error {
		e.n++
		name := fmt.Sprintf("ci-%d", e.n)
		if _, _, err := e.svc.CreateCIToken(e.ctx, e.alice, "acme/shop", name, nil); err != nil {
			return err // not found for everyone once acme is deleted
		}
		_, err := e.svc.RevokeCIToken(e.ctx, a, "acme/shop", name)
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
	e.superadminRows(t, "")
	e.agreementRows(t)
}

// agreementGated are the operations the data processing agreement gates
// (#41, docs/organizations.md): work that brings code into vx or takes it
// out. Everything else, reads above all, works without an agreement.
var agreementGated = map[string]bool{
	"SyncBranch": true, "CreateEffort": true, "OpenWorkspace": true, "Land": true, "ApproveReview": true,
	"PlanShipment": true, "PlanSplit": true, "OpenShipment split": true, "OpenShipment": true,
}

// agreementRows is the matrix with the agreement step on and acme not
// accepted: without access still "not found", with too little still a
// permission error, with enough a refusal naming acme's owners for the
// gated operations and success for the others; after alice accepts, all
// pass.
func (e *authzEnv) agreementRows(t *testing.T) {
	t.Helper()
	vs, err := service.LoadAgreementVersions(writeAgreements(t, fakeVersion{ID: "v1", Published: "2026-01-01", Effective: "2026-01-01"}))
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Agreement = service.AgreementSettings{Required: true, Versions: vs}
	defer func() { e.svc.Agreement = service.AgreementSettings{} }()
	for _, role := range []service.RepoRole{service.RoleNone, service.RoleRead, service.RoleWrite, service.RoleAdmin} {
		e.grant(role)
		for _, actor := range []service.Actor{e.bob, e.bobAgent} {
			for _, op := range authzOps {
				err := op.run(e, actor)
				switch {
				case role == service.RoleNone:
					if !isCode(err, service.CodeNotFound) {
						t.Errorf("agreement: %s by %s without access: want not found, got %v", op.name, actor.Ref(), err)
					}
				case !role.Includes(op.need):
					if !isCode(err, service.CodePermission) {
						t.Errorf("agreement: %s by %s with %s: want a permission error, got %v", op.name, actor.Ref(), role, err)
					}
				case agreementGated[op.name] && !(op.name == "ApproveReview" && actor.Kind == store.KindAgent):
					if !isAgreementRefusal(err, "acme") || !strings.Contains(err.Error(), "Owners: alice") {
						t.Errorf("agreement: %s by %s with %s: want the agreement refusal, got %v", op.name, actor.Ref(), role, err)
					}
				default:
					if err != nil {
						t.Errorf("agreement: %s by %s with %s (not gated): %v", op.name, actor.Ref(), role, err)
					}
				}
			}
		}
	}
	if _, err := e.svc.AcceptAgreement(e.ctx, e.alice, "acme", vs[0].ID, vs[0].SHA256); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []service.Actor{e.alice, e.agent, e.bob, e.bobAgent} {
		for _, op := range authzOps {
			if err := op.run(e, actor); err != nil {
				t.Errorf("agreement accepted: %s by %s: %v", op.name, actor.Ref(), err)
			}
		}
	}
	e.deletedRows(t)
}

// superadminRows is the matrix for root, a superadmin outside both
// organizations, and root's agent: no access to any repository operation
// from being a superadmin, then exactly the role of a support grant (read,
// write) while it lasts, and nothing once it expires or is revoked.
func (e *authzEnv) superadminRows(t *testing.T, label string) {
	t.Helper()
	root, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "root", SiteAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	rootAgent, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindAgent, Handle: "root/ops", ParentID: &root.ID})
	if err != nil {
		t.Fatal(err)
	}
	sa, _ := e.svc.ResolveActor(e.ctx, root)
	saAgent, _ := e.svc.ResolveActor(e.ctx, rootAgent)
	if e.linkRoot != nil {
		e.linkRoot(sa)
	}
	check := func(role service.RepoRole, when string) {
		t.Helper()
		for _, actor := range []service.Actor{sa, saAgent} {
			for _, op := range authzOps {
				err := op.run(e, actor)
				switch {
				case role == service.RoleNone:
					if !isCode(err, service.CodeNotFound) {
						t.Errorf("%s%s by superadmin %s %s: want not found, got %v", label, op.name, actor.Ref(), when, err)
					}
				case !role.Includes(op.need):
					if !isCode(err, service.CodePermission) {
						t.Errorf("%s%s by superadmin %s %s: want a permission error (needs %s), got %v", label, op.name, actor.Ref(), when, op.need, err)
					}
				default:
					if err != nil {
						t.Errorf("%s%s by superadmin %s %s: %v", label, op.name, actor.Ref(), when, err)
					}
				}
			}
		}
	}
	check(service.RoleNone, "without a grant")
	repos, err := e.svc.ListRepos(e.ctx, sa)
	if err != nil || len(repos) != 0 {
		t.Errorf("%ssuperadmin lists %+v, %v", label, repos, err)
	}
	for _, role := range []service.RepoRole{service.RoleRead, service.RoleWrite} {
		if _, err := e.svc.GrantSupport(e.ctx, e.alice, service.SupportInput{Repo: "acme/shop", Person: "root", Role: role, For: time.Hour}); err != nil {
			t.Fatal(err)
		}
		check(role, "with "+string(role)+" support access")
	}
	// Expired: as if the grant had never been made.
	clock := e.svc.Now
	e.svc.Now = func() time.Time { return clock().Add(2 * time.Hour) }
	check(service.RoleNone, "after the support access expired")
	e.svc.Now = clock
	if _, err := e.svc.GrantSupport(e.ctx, e.alice, service.SupportInput{Repo: "acme/shop", Person: "root", For: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeSupport(e.ctx, e.alice, "acme/shop", "root"); err != nil {
		t.Fatal(err)
	}
	check(service.RoleNone, "after the support access was revoked")
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
	// The superadmin is a GitHub user outside acme: GitHub gives them
	// nothing, and support grants count on GitHub-backed repositories.
	rootGH := gh.AddUser("root")
	e.linkRoot = func(a service.Actor) { linkGitHub(e.env, a, rootGH) }
	e.superadminRows(t, "GitHub-backed: ")
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
