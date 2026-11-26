package service_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

const pipelineRules = `[[check]]
name   = "unit"
runner = "agent:test"
blocks = "nothing"

[[check]]
name   = "e2e"
runner = "agent:browser"
after  = ["unit"]
inputs = ["diff", "unit.coverage", "unit.artifact:report.html"]
blocks = "nothing"
`

// pipelineShipped lands vx.toml with a two-check pipeline and code, and
// ships the effort to the fake GitHub.
func (e *feedbackEnv) pipelineShipped() (store.Effort, service.ShipResult) {
	e.t.Helper()
	eff, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: e.repo.Name, Name: "Checkout"})
	if err != nil {
		e.t.Fatal(err)
	}
	e.landAs(e.claude, eff, "Checks", map[string]*string{"vx.toml": s(pipelineRules)})
	e.landAs(e.claude, eff, "Checkout page", map[string]*string{"web/checkout.js": s("go()\n")})
	res, err := e.svc.OpenShipment(e.ctx, e.alice, service.ShipInput{Repo: e.repo.Name, Effort: eff.Slug})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.effort(eff), res
}

func (e *feedbackEnv) vxRuns() map[string]githubtest.CheckRun {
	out := map[string]githubtest.CheckRun{}
	for _, r := range e.gh.CheckRuns("acme/shop") {
		if strings.HasPrefix(r.Name, service.CheckRunPrefix) {
			out[strings.TrimPrefix(r.Name, service.CheckRunPrefix)] = r
		}
	}
	return out
}

func (e *feedbackEnv) reportUnit(eff store.Effort, status string) service.CheckResult {
	e.t.Helper()
	r, err := e.svc.ReportCheck(e.ctx, e.codex, service.ReportInput{Repo: e.repo.Name, Effort: eff.Slug, Name: "unit", Status: status,
		Summary: "142 tests, " + status, Data: []byte(`{"coverage": 81.5}`), DataName: "coverage.json",
		Artifacts: []service.ArtifactInput{{Name: "report.html", Data: []byte("<p>ok</p>")}}})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// A shipped effort's checks become GitHub check runs on the pull
// request's head, updated as results arrive: a failure skips the check
// after it, a pass lets it run with the earlier check's outputs.
func TestGitHubCheckRunsPerCheck(t *testing.T) {
	e := newFeedbackEnv(t)
	e.svc.GitHub.PublicURL = "https://vx.example"
	eff, res := e.pipelineShipped()
	head := res.Shipment.HeadGitSHA
	runs := e.vxRuns()
	if len(runs) != 2 {
		t.Fatalf("check runs after ship: %+v", e.gh.CheckRuns("acme/shop"))
	}
	if u := runs["unit"]; u.HeadSHA != head || u.Status != "queued" || u.Title != "Waiting for a result" ||
		!strings.HasPrefix(u.DetailsURL, "https://vx.example/ui/effort?") || !strings.Contains(u.DetailsURL, "view=checks") {
		t.Errorf("unit after ship: %+v", u)
	}
	if e2e := runs["e2e"]; e2e.Status != "queued" || e2e.Title != "Waits for unit" {
		t.Errorf("e2e after ship: %+v", e2e)
	}

	// unit fails: its run completes as a failure, e2e is skipped, not
	// left queued, and no e2e job is made.
	fail := e.reportUnit(eff, "fail")
	runs = e.vxRuns()
	if u := runs["unit"]; u.Status != "completed" || u.Conclusion != "failure" || !strings.Contains(u.Title, "142 tests, fail") ||
		!strings.Contains(u.Summary, "Reported by agent:alice/codex") || !strings.Contains(u.Summary, "/ui/checks/artifact?") || !strings.Contains(u.Summary, "report.html") || u.Updates != 1 {
		t.Errorf("unit after the failure: %+v", u)
	}
	if e2e := runs["e2e"]; e2e.Status != "completed" || e2e.Conclusion != "skipped" || e2e.Title != "Skipped: unit failed" {
		t.Errorf("e2e after the failure: %+v", e2e)
	}
	if jobs := e.jobs(eff, "agent:browser"); len(jobs) != 0 {
		t.Errorf("e2e job after a failure: %+v", jobs)
	}

	// unit passes: e2e is queued again and its job carries unit's outputs.
	pass := e.reportUnit(eff, "pass")
	runs = e.vxRuns()
	if u := runs["unit"]; u.Conclusion != "success" || u.Updates != 2 {
		t.Errorf("unit after the pass: %+v", u)
	}
	if e2e := runs["e2e"]; e2e.Status != "queued" || e2e.Conclusion != "skipped" && e2e.Title != "Waiting for a result" {
		t.Errorf("e2e after the pass: %+v", e2e)
	}
	jobs := e.jobs(eff, "agent:browser")
	if len(jobs) != 1 || jobs[0].Check != "e2e" {
		t.Fatalf("e2e jobs: %+v", jobs)
	}
	if v := jobs[0].Values; v["unit.coverage"] != "81.5" || !strings.Contains(v["unit.artifact:report.html"], "/ui/checks/artifact?") || !strings.Contains(v["unit.artifact:report.html"], pass.ID.String()) {
		t.Errorf("e2e job values: %+v", v)
	}
	// Shipping again pushes a new head: its runs start with the results
	// the effort head has.
	res2, err := e.svc.OpenShipment(e.ctx, e.alice, service.ShipInput{Repo: e.repo.Name, Effort: eff.Slug})
	if err != nil {
		t.Fatal(err)
	}
	runs = e.vxRuns()
	if u := runs["unit"]; u.HeadSHA != res2.Shipment.HeadGitSHA || u.Status != "completed" || u.Conclusion != "success" {
		t.Errorf("unit on the new head: %+v", u)
	}
	// Publishing with nothing new changes nothing on GitHub.
	n := len(e.gh.CheckRuns("acme/shop"))
	e.svc.PublishCheckRunsForTest(e.ctx, e.repo, eff)
	if len(e.gh.CheckRuns("acme/shop")) != n || e.vxRuns()["unit"].Updates != 0 {
		t.Errorf("an unchanged check was published again")
	}
	_ = fail
}

func (e *feedbackEnv) jobs(eff store.Effort, runner string) []store.CheckJob {
	e.t.Helper()
	id := eff.ID
	js, err := e.svc.Store.ListCheckJobs(e.ctx, store.CheckJobFilter{RepoIDs: []uuid.UUID{e.repo.ID}, EffortID: &id, Runners: []string{runner}, States: []string{"queued", "leased"}})
	if err != nil {
		e.t.Fatal(err)
	}
	return js
}

// Without the App's Checks permission, shipping and reporting work; vx
// records the gap (Admin > GitHub says GitHub checks are off) and posts
// the runs once the permission is granted.
func TestGitHubCheckRunsWithoutPermission(t *testing.T) {
	e := newFeedbackEnv(t)
	e.gh.SetAppPermission("checks", "read")
	eff, res := e.pipelineShipped()
	if res.PR.Number == 0 || len(e.vxRuns()) != 0 {
		t.Fatalf("ship without checks: write: %+v, runs %+v", res.PR, e.vxRuns())
	}
	e.reportUnit(eff, "pass") // never fails for want of the permission
	root := e.siteAdmin("root")
	gaps, err := e.svc.GitHubAppGaps(e.ctx, root)
	if err != nil || !service.GitHubChecksOff(gaps) {
		t.Fatalf("gaps: %+v %v", gaps, err)
	}
	e.gh.SetAppPermission("checks", "write")
	e.reportUnit(eff, "pass")
	runs := e.vxRuns()
	if runs["unit"].Conclusion != "success" || runs["e2e"].Status != "queued" {
		t.Fatalf("runs after the grant: %+v", runs)
	}
	if gaps, _ := e.svc.GitHubAppGaps(e.ctx, root); service.GitHubChecksOff(gaps) {
		t.Errorf("checks still off after the grant: %+v", gaps)
	}
}

// The pipeline view puts organization checks first, says where each
// check comes from, and notes a change that edits vx.toml.
func TestPipelineStatusOriginsAndRuleEdits(t *testing.T) {
	e := newFeedbackEnv(t)
	if _, _, err := e.svc.SetOrgChecks(e.ctx, e.alice, "acme", "[[check]]\nname = \"security\"\nrunner = \"human\"\nblocks = \"nothing\"\n"); err != nil {
		t.Fatal(err)
	}
	eff, _ := e.pipelineShipped()
	ws, err := e.svc.OpenWorkspace(e.ctx, e.claude, e.repo.Name, eff.Slug, "Weaken")
	if err != nil {
		t.Fatal(err)
	}
	snap := e.snapshot(e.repo, ws.Workspace.Base, map[string]*string{"vx.toml": s(pipelineRules + "\n[[check]]\nname = \"security\"\nrun = \"true\"\nblocks = \"nothing\"\n")})
	v, err := e.svc.PipelineStatus(e.ctx, e.alice, service.CheckStatusInput{Repo: e.repo.Name, Effort: eff.Slug, ChangeID: ws.Workspace.ChangeID, Tree: snap})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range v.Checks {
		got = append(got, strings.Repeat(" ", c.Depth)+c.Def.Name+"@"+c.Origin+"="+c.State)
	}
	want := []string{"security@organization acme (version 1)=ready", "unit@vx.toml line 1=ready", " e2e@vx.toml line 6=waiting"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("checks:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	notes := strings.Join(v.Notes, "\n")
	if !strings.Contains(notes, "this change edits vx.toml: the checks of the effort's base apply until it lands") ||
		!strings.Contains(notes, `vx.toml [[check]] "security" is ignored: organization acme defines a check of that name`) {
		t.Errorf("notes:\n%s", notes)
	}
	// Once landed, its vx.toml is the effort's: no note.
	if v, err := e.svc.PipelineStatus(e.ctx, e.alice, service.CheckStatusInput{Repo: e.repo.Name, Effort: eff.Slug}); err != nil || v.EditsRules {
		t.Errorf("effort head: edits rules %v, %v", v.EditsRules, err)
	}
	// Someone without read sees nothing.
	carol := e.person("carol")
	if _, err := e.svc.PipelineStatus(e.ctx, carol, service.CheckStatusInput{Repo: e.repo.Name, Effort: eff.Slug}); code(err) != service.CodeNotFound {
		t.Errorf("outsider: %v", err)
	}
	_ = rules.StateReady
}
