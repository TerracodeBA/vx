package service_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

const policyRules = `[checks]
required = ["go vet ./..."]

[[policy]]
paths   = ["api/**"]
mode    = "gated"
require = ["go test ./...", "go vet ./..."]

[[policy]]
paths = ["db/migrations/**"]
mode  = "human"
`

// ran is evidence of checks vx ran, all passing.
func ran(cmds ...string) *object.Evidence {
	ev := &object.Evidence{}
	for _, c := range cmds {
		ev.Checks = append(ev.Checks, object.Check{Name: c, Result: "pass", RanByVX: true})
	}
	return ev
}

// policyEnv is env with an effort whose vx.toml has policyRules.
type policyEnv struct {
	*env
	repo store.Repo
	eff  store.Effort
}

func newPolicyEnv(t *testing.T, rulesToml string) *policyEnv {
	t.Helper()
	e := &policyEnv{env: newEnv(t)}
	e.repo, e.eff = e.setup()
	e.land(e.repo, e.eff, "Rules", map[string]*string{"vx.toml": s(rulesToml), "config.txt": s("limit = 1\n")}, nil)
	return e
}

func (e *policyEnv) head() hash.Hash {
	e.t.Helper()
	return e.effort(e.eff).Head
}

// try opens a workspace for actor and lands files with ev.
func (e *policyEnv) try(actor service.Actor, intent string, files map[string]*string, ev *object.Evidence) (service.WorkspaceView, service.LandResult, error) {
	e.t.Helper()
	ws, err := e.svc.OpenWorkspace(e.ctx, actor, e.repo.Name, e.eff.Slug, intent)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.svc.Land(e.ctx, actor, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: e.snapshot(e.repo, ws.Workspace.Base, files), Evidence: ev, Model: "claude-opus-5-5"})
	return ws, res, err
}

// pending lands a migration under the human policy and returns its review.
func (e *policyEnv) pending(actor service.Actor, name string) (service.WorkspaceView, service.Review) {
	e.t.Helper()
	before := e.head()
	ws, res, err := e.try(actor, "Add "+name, map[string]*string{"db/migrations/" + name: s("create table x ();\n")}, ran("go vet ./..."))
	if err != nil || res.Landed || res.Review == nil {
		e.t.Fatalf("human land: %+v %v", res, err)
	}
	if e.head() != before || res.EffortHead != before {
		e.t.Fatalf("a pending review moved the effort head")
	}
	return ws, *res.Review
}

// approvalNotes are the system approval notes on the board, newest first.
func (e *policyEnv) approvalNotes() []service.Note {
	e.t.Helper()
	notes, err := e.svc.ListNotes(e.ctx, e.alice, service.ListNotesInput{Repo: e.repo.Name, Effort: e.eff.Slug})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []service.Note
	for _, n := range notes {
		if n.Kind == service.NoteApproval {
			out = append(out, n)
		}
	}
	return out
}

func TestPolicyFastIsDefault(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	_, res, err := e.try(e.agent, "Docs", map[string]*string{"README.md": s("hi\n")}, nil)
	if err != nil || !res.Landed || res.Review != nil {
		t.Fatalf("a land no policy covers: %+v %v", res, err)
	}
	if got := res.Changeset.Evidence; got == nil || strings.Join(got.NotVerified, ",") != "go vet ./..." {
		t.Fatalf("fast lands still record what was not verified: %+v", got)
	}
}

func TestPolicyGated(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	files := map[string]*string{"api/limit.go": s("package api\n")}
	reported := &object.Evidence{Checks: []object.Check{{Name: "go test ./...", Result: "pass"}, {Name: "go vet ./...", Result: "fail", RanByVX: true}}}
	before := e.head()
	_, _, err := e.try(e.agent, "Limits", files, reported)
	if !isCode(err, service.CodeFailedPrecondition) {
		t.Fatalf("gated land without checks run by vx: %v", err)
	}
	for _, want := range []string{
		`not landed: api/limit.go is under a gated landing policy ([[policy]] paths = ["api/**"] (vx.toml line 4))`,
		"It needs every required check run by vx (vx land --run) and passing in this land:",
		"  go test ./...  (not run by vx)",
		"  go vet ./...   (failed)",
		"Land with:\n  vx land --run 'go test ./...' --run 'go vet ./...'",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, err)
		}
	}
	if e.head() != before {
		t.Fatal("a refused land moved the head")
	}

	// Run by vx and passing: it lands.
	_, res, err := e.try(e.agent, "Limits", files, ran("go test ./...", "go vet ./..."))
	if err != nil || !res.Landed {
		t.Fatalf("gated land with its checks: %+v %v", res, err)
	}

	// Gated lands only what the checks ran on: when the effort moved, the
	// land is refused as moved (vx land --run syncs and runs them again).
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, e.repo.Name, e.eff.Slug, "More limits")
	e.land(e.repo, e.effort(e.eff), "Other", map[string]*string{"other.txt": s("x\n")}, nil)
	r2, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID,
		Tree: e.snapshot(e.repo, w2.Workspace.Base, map[string]*string{"api/more.go": s("package api\n")}), Evidence: ran("go test ./...", "go vet ./...")})
	if err != nil || r2.Landed || !r2.EffortMoved {
		t.Fatalf("gated land onto a moved effort: %+v %v", r2, err)
	}
}

// The strictest matching policy wins, and its checks add up with the
// gated ones; a change cannot drop its own policy by editing vx.toml.
func TestPolicyStrictestAndCannotWeaken(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	_, _, err := e.try(e.agent, "Both", map[string]*string{
		"api/x.go": s("package api\n"), "db/migrations/002_x.sql": s("x\n"),
	}, ran("go vet ./..."))
	if !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "db/migrations/002_x.sql is under a human landing policy") ||
		!strings.Contains(err.Error(), "Before a person reviews it") || !strings.Contains(err.Error(), "go test ./...  (not run by vx)") {
		t.Fatalf("human + gated without the gated checks: %v", err)
	}
	_, res, err := e.try(e.agent, "Both", map[string]*string{
		"api/x.go": s("package api\n"), "db/migrations/002_x.sql": s("x\n"),
	}, ran("go vet ./...", "go test ./..."))
	if err != nil || res.Landed || res.Review == nil || strings.Join(res.Review.Paths, ",") != "db/migrations/002_x.sql" {
		t.Fatalf("human wins over gated: %+v %v", res, err)
	}

	// Deleting the human policy in the same change does not help: the
	// effort head's vx.toml still applies.
	weakened := strings.Replace(policyRules, `mode  = "human"`, `mode  = "fast"`, 1)
	_, res, err = e.try(e.agent, "Sneaky", map[string]*string{"vx.toml": s(weakened), "db/migrations/003_y.sql": s("y\n")}, ran("go vet ./..."))
	if err != nil || res.Landed || res.Review == nil {
		t.Fatalf("a change that weakens its own policy: %+v %v", res, err)
	}
	// Making it stricter in the same change applies at once.
	stricter := policyRules + "\n[[policy]]\npaths = [\"docs/**\"]\nmode = \"human\"\n"
	_, res, err = e.try(e.agent, "Stricter", map[string]*string{"vx.toml": s(stricter), "docs/a.md": s("a\n")}, ran("go vet ./..."))
	if err != nil || res.Landed || res.Review == nil {
		t.Fatalf("a change that adds a policy for itself: %+v %v", res, err)
	}
}

func TestPolicyHumanApprove(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	ws, rv := e.pending(e.agent, "002_users.sql")
	if rv.WaitingText() != "Waiting for a person to review: db/migrations/002_users.sql (policy in vx.toml)" {
		t.Fatalf("waiting text: %s", rv.WaitingText())
	}
	if v, _ := e.svc.GetReview(e.ctx, e.alice, rv.ID.String()); !v.CanDecide {
		t.Fatalf("alice may approve her agent's change: %s", v.CannotDecide)
	}
	if rv.State != store.ReviewPending || rv.Author.ID != e.agent.ID || rv.ChangeID != ws.Workspace.ChangeID || rv.CanDecide || !strings.Contains(rv.CannotDecide, "only people") {
		t.Fatalf("review as its author's person sees it: %+v", rv)
	}
	// The board says it waits.
	if ns := e.approvalNotes(); len(ns) != 1 || ns[0].To != nil || !strings.Contains(ns[0].Body, "Waiting for a person to review: "+ws.Workspace.ChangeID) {
		t.Fatalf("waiting note: %+v", ns)
	}
	// Listed as pending; GetReview has its files.
	list, err := e.svc.ListReviews(e.ctx, e.alice, service.ReviewQuery{Repo: e.repo.Name, Effort: e.eff.Slug})
	if err != nil || len(list) != 1 || list[0].ID != rv.ID {
		t.Fatalf("ListReviews: %+v %v", list, err)
	}
	got, err := e.svc.GetReview(e.ctx, e.alice, rv.ID.String()[:8])
	if err != nil || len(got.Files) != 1 || got.Files[0].Path != "db/migrations/002_users.sql" || got.Files[0].Added != 1 {
		t.Fatalf("GetReview: %+v %v", got, err)
	}
	// The workspace stays open while it waits.
	if w, _ := e.svc.GetWorkspace(e.ctx, e.agent, ws.Workspace.ID); w.Workspace.State != store.WorkspaceOpen {
		t.Fatalf("workspace while pending: %s", w.Workspace.State)
	}

	// Agents never approve, not even for their own person.
	if _, err := e.svc.ApproveReview(e.ctx, e.agent, rv.ID.String()); !isCode(err, service.CodePermission) || !strings.Contains(err.Error(), "only people approve") {
		t.Fatalf("agent approves: %v", err)
	}
	// Someone else lands meanwhile: the approval merges onto it.
	other := e.land(e.repo, e.effort(e.eff), "Readme", map[string]*string{"README.md": s("x\n")}, nil)
	dec, err := e.svc.ApproveReview(e.ctx, e.alice, rv.ID.String()[:8])
	if err != nil || dec.Landed == nil || !dec.Landed.Landed || dec.Review.State != store.ReviewApproved {
		t.Fatalf("approve: %+v %v", dec, err)
	}
	cs := dec.Landed.Changeset
	if cs.Parents[0] != other.Hash || cs.Author.Principal != e.agent.Ref() || cs.ChangeID != ws.Workspace.ChangeID || e.head() != cs.Hash {
		t.Fatalf("approved change: %+v head %s", cs.Changeset, e.head())
	}
	if ap := cs.Evidence.Approvals; len(ap) != 1 || ap[0].By != "user:alice" || ap[0].Review != rv.ID.String() || strings.Join(ap[0].Paths, ",") != "db/migrations/002_users.sql" {
		t.Fatalf("evidence approvals: %+v", cs.Evidence)
	}
	stored, err := e.svc.Store.GetChangeset(e.ctx, e.repo.ID, cs.Hash)
	if err != nil || len(stored.Evidence.Approvals) != 1 {
		t.Fatalf("stored approvals: %+v %v", stored.Evidence, err)
	}
	if h, _ := stored.Changeset.Hash(); h != cs.Hash {
		t.Fatalf("stored changeset hashes to %s, not %s", h, cs.Hash)
	}
	if w, _ := e.svc.GetWorkspace(e.ctx, e.agent, ws.Workspace.ID); w.Workspace.State != store.WorkspaceLanded || w.Workspace.LandedAs != cs.Hash {
		t.Fatalf("workspace after approval: %+v", w.Workspace)
	}
	if files := e.read(e.repo, e.head()); files["db/migrations/002_users.sql"] == "" || files["README.md"] != "x\n" {
		t.Fatalf("head files: %v", files)
	}
	// The author hears of it, addressed to them.
	ns := e.approvalNotes()
	if len(ns) != 2 || ns[0].To == nil || ns[0].To.ID != e.agent.ID || !ns[0].System ||
		!strings.Contains(ns[0].Body, "user:alice approved your change "+ws.Workspace.ChangeID) || !strings.Contains(ns[0].Body, "landed as "+cs.Hash.Short()) {
		t.Fatalf("approval note: %+v", ns)
	}
	// Decided once.
	if _, err := e.svc.ApproveReview(e.ctx, e.alice, rv.ID.String()); !isCode(err, service.CodeFailedPrecondition) {
		t.Fatalf("second approval: %v", err)
	}
	if list, _ := e.svc.ListReviews(e.ctx, e.alice, service.ReviewQuery{}); len(list) != 0 {
		t.Fatalf("pending after approval: %+v", list)
	}
}

// Who approves: people with write, in every approvers list, never the
// author themselves.
func TestPolicyApprovers(t *testing.T) {
	rules := policyRules + `
[[policy]]
paths     = ["db/migrations/**"]
mode      = "human"
approvers = ["user:bob", "team:acme/db"]
`
	e := newPolicyEnv(t, rules)
	bob, carol, dave := e.person("bob"), e.person("carol"), e.person("dave")
	if _, err := e.svc.CreateOrg(e.ctx, e.alice, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateTeam(e.ctx, e.alice, "acme", "db"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetMember(e.ctx, e.alice, "acme", "carol", service.OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetTeamMember(e.ctx, e.alice, "acme", "db", "carol"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"user:bob", "user:carol", "user:dave"} {
		if _, err := e.svc.GrantRepo(e.ctx, e.alice, e.repo.Name, p, service.RoleWrite); err != nil {
			t.Fatal(err)
		}
	}
	_, rv := e.pending(e.agent, "002_a.sql")
	if len(rv.Approvers) != 1 || strings.Join(rv.Approvers[0], ",") != "user:bob,team:acme/db" {
		t.Fatalf("approvers: %+v", rv.Approvers)
	}
	// Alice has write but is not listed; dave neither.
	for _, a := range []service.Actor{e.alice, dave} {
		if _, err := e.svc.ApproveReview(e.ctx, a, rv.ID.String()); !isCode(err, service.CodePermission) || !strings.Contains(err.Error(), "you are not one of them: user:bob, team:acme/db") {
			t.Fatalf("%s approves: %v", a.Ref(), err)
		}
	}
	if v, _ := e.svc.GetReview(e.ctx, dave, rv.ID.String()); v.CanDecide || !strings.Contains(v.CannotDecide, "not one of them") {
		t.Fatalf("dave's view: %+v", v)
	}
	// Carol, in team acme/db, approves.
	if dec, err := e.svc.ApproveReview(e.ctx, carol, rv.ID.String()); err != nil || dec.Landed == nil {
		t.Fatalf("carol approves: %+v %v", dec, err)
	}
	// Bob with only read cannot approve, even though he is listed.
	_, rv2 := e.pending(e.agent, "003_b.sql")
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, e.repo.Name, "user:bob", service.RoleRead); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ApproveReview(e.ctx, bob, rv2.ID.String()); !isCode(err, service.CodePermission) {
		t.Fatalf("bob with read approves: %v", err)
	}
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, e.repo.Name, "user:bob", service.RoleWrite); err != nil {
		t.Fatal(err)
	}
	// A change bob landed himself needs another person.
	_, rv3 := e.pending(bob, "004_c.sql")
	if _, err := e.svc.ApproveReview(e.ctx, bob, rv3.ID.String()); !isCode(err, service.CodePermission) || !strings.Contains(err.Error(), "you wrote this change yourself") {
		t.Fatalf("bob approves his own change: %v", err)
	}
	if _, err := e.svc.ApproveReview(e.ctx, bob, rv2.ID.String()); err != nil {
		t.Fatalf("bob approves the agent's change: %v", err)
	}
	if _, err := e.svc.ApproveReview(e.ctx, carol, rv3.ID.String()); err != nil {
		t.Fatalf("carol approves bob's change: %v", err)
	}
	// Someone who cannot read the repository does not see the review.
	eve := e.person("eve")
	if _, err := e.svc.GetReview(e.ctx, eve, rv.ID.String()); !isCode(err, service.CodeNotFound) {
		t.Fatalf("eve reads a review: %v", err)
	}
	if _, err := e.svc.ApproveReview(e.ctx, eve, rv.ID.String()); !isCode(err, service.CodeNotFound) {
		t.Fatalf("eve approves: %v", err)
	}
	if list, err := e.svc.ListReviews(e.ctx, eve, service.ReviewQuery{States: []store.ReviewState{store.ReviewApproved}}); err != nil || len(list) != 0 {
		t.Fatalf("eve lists reviews: %+v %v", list, err)
	}
	if list, err := e.svc.ListReviews(e.ctx, e.alice, service.ReviewQuery{States: []store.ReviewState{store.ReviewApproved}}); err != nil || len(list) != 3 {
		t.Fatalf("approved reviews: %d %v", len(list), err)
	}
}

// Approval that no longer merges: the review stays pending, goes back to
// the author with a note, and a new land from the workspace replaces it.
func TestPolicyApproveConflictsGoBackToAuthor(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, e.repo.Name, e.eff.Slug, "Edit auth with a migration")
	if err != nil {
		t.Fatal(err)
	}
	tree := e.snapshot(e.repo, ws.Workspace.Base, map[string]*string{"config.txt": s("limit = 2\n"), "db/migrations/002_x.sql": s("x\n")})
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree, Evidence: ran("go vet ./...")})
	if err != nil || res.Review == nil {
		t.Fatalf("submit: %+v %v", res, err)
	}
	rv := *res.Review
	// Someone changes the same lines meanwhile.
	e.land(e.repo, e.effort(e.eff), "Other limit", map[string]*string{"config.txt": s("limit = 3\n")}, nil)
	head := e.head()
	dec, err := e.svc.ApproveReview(e.ctx, e.alice, rv.ID.String())
	if err != nil || dec.Landed != nil || len(dec.Conflicts) != 1 || dec.Conflicts[0].Path != "config.txt" || dec.Review.State != store.ReviewPending {
		t.Fatalf("approve with conflicts: %+v %v", dec, err)
	}
	if e.head() != head {
		t.Fatal("a conflicting approval moved the head")
	}
	got, _ := e.svc.GetReview(e.ctx, e.alice, rv.ID.String())
	if strings.Join(got.Conflicts, ",") != "config.txt" || got.State != store.ReviewPending {
		t.Fatalf("stored conflicts: %+v", got.Review)
	}
	ns := e.approvalNotes()
	if len(ns) == 0 || ns[0].To == nil || ns[0].To.ID != e.agent.ID || !strings.Contains(ns[0].Body, "conflicts in config.txt") || !strings.Contains(ns[0].Body, "Run vx sync, resolve them, then vx land again") {
		t.Fatalf("conflict note to the author: %+v", ns)
	}
	// The author syncs, resolves and lands again: the same review, new
	// snapshot, conflicts cleared.
	if _, err := e.svc.SetBase(e.ctx, e.agent, ws.Workspace.ID, head); err != nil {
		t.Fatal(err)
	}
	resolved := e.snapshot(e.repo, head, map[string]*string{"config.txt": s("limit = 23\n"), "db/migrations/002_x.sql": s("x\n")})
	res, err = e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: resolved, Evidence: ran("go vet ./...")})
	if err != nil || res.Review == nil || res.Review.ID != rv.ID || len(res.Review.Conflicts) != 0 || res.Review.Tree != resolved {
		t.Fatalf("resubmit: %+v %v", res.Review, err)
	}
	if ns2 := e.approvalNotes(); len(ns2) != len(ns) {
		t.Fatalf("a resubmission posts no new waiting note: %d -> %d", len(ns), len(ns2))
	}
	dec, err = e.svc.ApproveReview(e.ctx, e.alice, rv.ID.String())
	if err != nil || dec.Landed == nil {
		t.Fatalf("approve after resolving: %+v %v", dec, err)
	}
	if e.read(e.repo, e.head())["config.txt"] != "limit = 23\n" {
		t.Fatal("resolved file did not land")
	}
}

// A submit that already conflicts comes back like a land's conflicts.
func TestPolicyHumanSubmitConflicts(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	ws, _ := e.svc.OpenWorkspace(e.ctx, e.agent, e.repo.Name, e.eff.Slug, "Login and migration")
	e.land(e.repo, e.effort(e.eff), "Other limit", map[string]*string{"config.txt": s("limit = 3\n")}, nil)
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Evidence: ran("go vet ./..."),
		Tree: e.snapshot(e.repo, ws.Workspace.Base, map[string]*string{"config.txt": s("limit = 2\n"), "db/migrations/002_x.sql": s("x\n")})})
	if err != nil || res.Landed || res.Review != nil || len(res.Conflicts) != 1 {
		t.Fatalf("submit onto conflicting work: %+v %v", res, err)
	}
	if list, _ := e.svc.ListReviews(e.ctx, e.alice, service.ReviewQuery{}); len(list) != 0 {
		t.Fatalf("a conflicting submit stored a review: %+v", list)
	}
}

func TestPolicyReject(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	ws, rv := e.pending(e.agent, "002_drop.sql")
	if _, err := e.svc.RejectReview(e.ctx, e.alice, rv.ID.String(), "  "); !isCode(err, service.CodeInvalid) {
		t.Fatalf("reject without a reason: %v", err)
	}
	if _, err := e.svc.RejectReview(e.ctx, e.agent, rv.ID.String(), "no"); !isCode(err, service.CodePermission) {
		t.Fatalf("agent rejects: %v", err)
	}
	dec, err := e.svc.RejectReview(e.ctx, e.alice, rv.ID.String(), "Do not drop tables in a migration")
	if err != nil || dec.Review.State != store.ReviewRejected || dec.Review.Reason != "Do not drop tables in a migration" || dec.Review.DecidedBy == nil || dec.Review.DecidedBy.ID != e.alice.ID {
		t.Fatalf("reject: %+v %v", dec, err)
	}
	ns := e.approvalNotes()
	if len(ns) != 2 || ns[0].To == nil || ns[0].To.ID != e.agent.ID || !strings.Contains(ns[0].Body, "user:alice rejected your change "+ws.Workspace.ChangeID) ||
		!strings.Contains(ns[0].Body, "Do not drop tables in a migration") {
		t.Fatalf("rejection note: %+v", ns)
	}
	if _, err := e.svc.ApproveReview(e.ctx, e.alice, rv.ID.String()); !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "already rejected") {
		t.Fatalf("approve a rejected review: %v", err)
	}
	// The workspace is still open; landing again opens a new review.
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Evidence: ran("go vet ./..."),
		Tree: e.snapshot(e.repo, ws.Workspace.Base, map[string]*string{"db/migrations/002_drop.sql": s("alter table x rename to y;\n")})})
	if err != nil || res.Review == nil || res.Review.ID == rv.ID {
		t.Fatalf("land after rejection: %+v %v", res, err)
	}
	// Abandoning the workspace withdraws it.
	if _, err := e.svc.Abandon(e.ctx, e.agent, ws.Workspace.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.GetReview(e.ctx, e.alice, res.Review.ID.String()); got.State != store.ReviewWithdrawn {
		t.Fatalf("after abandon: %s", got.State)
	}
}

// A pending review is not part of the effort: not in its log, so not in a
// ship plan or the overlap forecast, which read the effort's chain.
func TestPolicyPendingNotInEffort(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	_, rv := e.pending(e.agent, "002_users.sql")
	log, err := e.svc.Log(e.ctx, e.alice, e.repo.Name, e.eff.Slug, 50, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range log {
		for _, f := range l.Files {
			if strings.HasPrefix(f.Path, "db/") {
				t.Fatalf("pending change in the log: %s", l.ChangeID)
			}
		}
	}
	// A normal land from the same workspace that no longer touches the
	// policy's paths withdraws the review.
	ws, err := e.svc.Store.GetWorkspace(e.ctx, rv.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.ID, Tree: e.snapshot(e.repo, ws.Base, map[string]*string{"notes.txt": s("n\n")})})
	if err != nil || !res.Landed {
		t.Fatalf("plain land from the workspace: %+v %v", res, err)
	}
	if got, _ := e.svc.GetReview(e.ctx, e.alice, rv.ID.String()); got.State != store.ReviewWithdrawn {
		t.Fatalf("review after a plain land: %s", got.State)
	}
	if _, err := e.svc.GetReview(e.ctx, e.alice, uuid.NewString()); !isCode(err, service.CodeNotFound) {
		t.Fatalf("unknown review: %v", err)
	}
}

// On a GitHub-backed repository approvers need write on GitHub: vx grants
// do not count there.
func TestPolicyApproversOnGitHub(t *testing.T) {
	e := newGHEnv(t)
	aliceAgent := e.agentOf(e.alice, "claude2")
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/api", Name: "Schema"})
	if err != nil {
		t.Fatal(err)
	}
	pe := &policyEnv{env: e.env, repo: e.api, eff: eff}
	pe.land(e.api, eff, "Rules", map[string]*string{"vx.toml": s(policyRules)}, nil)
	_, rv := pe.pending(aliceAgent, "002_a.sql")
	// bob has read on GitHub: he sees the review but cannot approve.
	if v, err := e.svc.GetReview(e.ctx, e.bob, rv.ID.String()); err != nil || v.CanDecide || !strings.Contains(v.CannotDecide, "approving needs write") {
		t.Fatalf("bob's view with read: %+v %v", v, err)
	}
	if _, err := e.svc.ApproveReview(e.ctx, e.bob, rv.ID.String()); !isCode(err, service.CodePermission) {
		t.Fatalf("bob approves with read on GitHub: %v", err)
	}
	// carol is not in acme: the review does not exist for her.
	if _, err := e.svc.ApproveReview(e.ctx, e.carol, rv.ID.String()); !isCode(err, service.CodeNotFound) {
		t.Fatalf("carol approves: %v", err)
	}
	// Write on GitHub: bob approves.
	e.gh.SetPermission("acme/api", "bob", "write")
	e.drop(service.GitHubRoleScope{GitHubRepoID: e.apiGH.ID, Reason: "member edited"})
	dec, err := e.svc.ApproveReview(e.ctx, e.bob, rv.ID.String())
	if err != nil || dec.Landed == nil || dec.Landed.Changeset.Evidence.Approvals[0].By != "user:bob" {
		t.Fatalf("bob approves with write on GitHub: %+v %v", dec, err)
	}
}
