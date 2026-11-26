package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

const queueToml = `[[check]]
name     = "e2e"
when     = "land"
paths    = ["web/**"]
runner   = "agent:browser"
reporter = "independent"
inputs   = ["diff", "changed_files", "preview_url"]
blocks   = "nothing"
timeout  = "10m"

[[check]]
name     = "ship-e2e"
when     = "ship"
paths    = ["web/**"]
runner   = "agent:browser"
reporter = "independent"
blocks   = "nothing"

[[check]]
name     = "api-review"
when     = "land"
paths    = ["api/**"]
runner   = "agent:browser"
reporter = "independent"
allow_same_person = true
blocks   = "land"

[[check]]
name   = "unit"
when   = "land"
run    = "go test ./..."
blocks = "nothing"

[[check]]
name     = "lint"
when     = "manual"
runner   = "ci"
reporter = "ci"

[[check]]
name     = "signoff"
when     = "manual"
runner   = "human"
reporter = "human"
`

func (q *queueEnv) jobsOf(check string) []service.CheckJobView {
	q.t.Helper()
	vs, err := q.svc.ListCheckJobs(q.ctx, q.alice, service.ListJobsInput{Repo: "acme/shop", Effort: q.eff.Slug})
	if err != nil {
		q.t.Fatal(err)
	}
	var out []service.CheckJobView
	for _, v := range vs {
		if v.Check == check {
			out = append(out, v)
		}
	}
	return out
}

// TestCheckJobsOnLandAndShip: jobs are made on land and ship for checks
// whose paths the change touched, only for checks with a runner; a land
// gate queues its checks on the snapshot; asking to ship again makes no
// second job.
func TestCheckJobsOnLandAndShip(t *testing.T) {
	q := newQueueEnv(t)
	eff := q.effort(q.eff)
	q.land(q.repo, eff, "Checks", map[string]*string{"vx.toml": s(queueToml)}, nil)
	if n := len(q.jobsOf("e2e")) + len(q.jobsOf("unit")); n != 0 {
		t.Fatalf("the change adding the checks made %d jobs", n)
	}

	web := q.land(q.repo, q.effort(q.eff), "Cart page", map[string]*string{"web/cart.html": s("<h1>Cart</h1>\n")}, nil)
	e2e := q.jobsOf("e2e")
	if len(e2e) != 1 || e2e[0].Tree != web.Tree || e2e[0].ChangeID != web.ChangeID || e2e[0].Trigger != "land" ||
		e2e[0].AuthorPrincipal != q.agent.Ref() || e2e[0].State != store.JobQueued {
		t.Fatalf("e2e jobs after a web land: %+v", e2e)
	}
	if len(q.jobsOf("unit")) != 0 || len(q.jobsOf("ship-e2e")) != 0 || len(q.jobsOf("lint")) != 0 {
		t.Fatal("a check without a runner, a ship check or a manual check made a job on land")
	}
	q.land(q.repo, q.effort(q.eff), "Docs", map[string]*string{"README.md": s("shop\n")}, nil)
	if got := q.jobsOf("e2e"); len(got) != 1 || got[0].State != store.JobQueued {
		t.Fatalf("a land outside web/** touched the e2e job: %+v", got)
	}
	web2 := q.land(q.repo, q.effort(q.eff), "Cart total", map[string]*string{"web/cart.html": s("<h1>Cart</h1>\n<p>Total</p>\n")}, nil)
	got := q.jobsOf("e2e")
	if len(got) != 2 || got[0].Tree != web2.Tree || got[0].State != store.JobQueued || got[1].State != store.JobSuperseded {
		t.Fatalf("after a second web land: %+v", got)
	}

	// The runner sees the change's own diff and files.
	v := q.next(q.bobAgent, "browser")
	if v == nil || v.Check != "e2e" || v.Inputs == nil || !strings.Contains(v.Inputs.Diff, "+<p>Total</p>") || len(v.Inputs.ChangedFiles) != 1 ||
		strings.Join(v.Inputs.Missing, ",") != "preview_url" {
		t.Fatalf("job for the runner: %+v", v)
	}

	// Ship: the effort's whole diff, once.
	for range 2 {
		if _, err := q.svc.PlanShipment(q.ctx, q.alice, "acme/shop", q.eff.Slug, ""); err != nil && !isCode(err, service.CodeFailedPrecondition) {
			t.Fatal(err)
		}
	}
	head := q.effort(q.eff)
	ship := q.jobsOf("ship-e2e")
	if len(ship) != 1 || ship[0].Trigger != "ship" || ship[0].Tree != web2.Tree || head.Head != web2.Hash {
		t.Fatalf("ship jobs: %+v", ship)
	}

	// A land gate (api-review blocks land) queues its check on the snapshot.
	ws, err := q.svc.OpenWorkspace(q.ctx, q.agent, "acme/shop", q.eff.Slug, "Prices API")
	if err != nil {
		t.Fatal(err)
	}
	snap := q.snapshot(q.repo, ws.Workspace.Base, map[string]*string{"api/prices.go": s("package api\n")})
	_, err = q.svc.Land(q.ctx, q.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: snap})
	if !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "api-review") || !strings.Contains(err.Error(), "Queued api-review") {
		t.Fatalf("gated land: %v", err)
	}
	gate := q.jobsOf("api-review")
	if len(gate) != 1 || gate[0].Tree != snap || gate[0].ChangeID != ws.Workspace.ChangeID || !gate[0].Changeset.IsZero() {
		t.Fatalf("gate jobs: %+v", gate)
	}
	// allow_same_person: the author's sibling may take and report it.
	gv := q.next(q.sibling, "browser")
	if gv == nil || gv.ID != gate[0].ID {
		t.Fatalf("sibling takes the gate job: %+v", gv)
	}
	res, err := q.svc.ReportCheck(q.ctx, q.sibling, service.ReportInput{Repo: "acme/shop", Effort: q.eff.Slug, ChangeID: ws.Workspace.ChangeID,
		Tree: snap, Name: "api-review", Status: store.CheckPass, Summary: "looks right"})
	if err != nil || !res.Counts {
		t.Fatalf("sibling's report: %+v %v", res, err)
	}
	if j := q.jobsOf("api-review"); j[0].State != store.JobDone || j[0].ResultID != res.ID.String() {
		t.Fatalf("gate job after the pass: %+v", j[0])
	}
	if lr, err := q.svc.Land(q.ctx, q.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: snap}); err != nil || !lr.Landed {
		t.Fatalf("land after the pass: %+v %v", lr, err)
	}
	if j := q.jobsOf("api-review"); len(j) != 1 {
		t.Fatalf("the landed tree made a second api-review job: %+v", j)
	}
}

// TestCheckReportTrust: every reporter rule through ReportCheck: a result
// from a reporter the check does not allow is stored, does not count, says
// why, and leaves the job waiting.
func TestCheckReportTrust(t *testing.T) {
	q := newQueueEnv(t)
	q.land(q.repo, q.effort(q.eff), "Checks", map[string]*string{"vx.toml": s(queueToml)}, nil)
	web := q.land(q.repo, q.effort(q.eff), "Cart page", map[string]*string{"web/cart.html": s("<h1>Cart</h1>\n")}, nil)
	_, secret, err := q.svc.CreateCIToken(q.ctx, q.alice, "acme/shop", "gha", []string{"lint"})
	if err != nil {
		t.Fatal(err)
	}
	ci, err := q.ciActor(secret)
	if err != nil {
		t.Fatal(err)
	}
	report := func(a service.Actor, check string) service.CheckResult {
		t.Helper()
		r, err := q.svc.ReportCheck(q.ctx, a, service.ReportInput{Repo: "acme/shop", Effort: q.eff.Slug, ChangeID: web.ChangeID, Tree: web.Tree,
			Name: check, Status: store.CheckPass, Summary: "ok"})
		if err != nil {
			t.Fatalf("%s reports %s: %v", a.Ref(), check, err)
		}
		return r
	}
	for _, c := range []struct {
		who    service.Actor
		check  string
		counts bool
		why    string
	}{
		{q.agent, "e2e", false, "made this change"},              // the author
		{q.sibling, "e2e", false, "another agent of user:alice"}, // the author's person's other agent
		{q.alice, "e2e", false, "the person whose agent made it"},
		{q.bobAgent, "e2e", true, ""}, // another person's agent
		{q.agent, "lint", false, "only CI"},
		{q.bob, "lint", false, "only CI"},
		{ci, "lint", true, ""},
		{q.bobAgent, "signoff", false, "only a person"}, // agents never report as a person
		{q.agent, "signoff", false, "only a person"},
		{ci, "signoff", false, "may report only lint"},
		{q.alice, "signoff", true, ""}, // her agent's change: she may sign it off
		{q.bob, "signoff", true, ""},
	} {
		if c.who.ID == ci.ID && c.check == "signoff" {
			_, err := q.svc.ReportCheck(q.ctx, ci, service.ReportInput{Repo: "acme/shop", Effort: q.eff.Slug, ChangeID: web.ChangeID, Tree: web.Tree, Name: "signoff", Status: store.CheckPass})
			if !isCode(err, service.CodePermission) || !strings.Contains(err.Error(), c.why) {
				t.Errorf("CI token reporting signoff: %v", err)
			}
			continue
		}
		r := report(c.who, c.check)
		if r.Counts != c.counts || (c.why != "" && !strings.Contains(r.Why, c.why)) {
			t.Errorf("%s reports %s: counts=%v why=%q, want %v %q", c.who.Ref(), c.check, r.Counts, r.Why, c.counts, c.why)
		}
	}
	// Results that do not count leave the job waiting; bob's agent's
	// finished it.
	if j := q.jobsOf("e2e"); len(j) != 1 || j[0].State != store.JobDone {
		t.Fatalf("e2e job: %+v", j)
	}
	// A CI token reports only on its repository.
	if _, err := q.svc.ReportCheck(q.ctx, ci, service.ReportInput{Repo: "globex/site", Effort: "x", Name: "lint", Status: store.CheckPass}); !isCode(err, service.CodeNotFound) {
		t.Fatalf("CI token on another repository: %v", err)
	}
}
