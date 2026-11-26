package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

const gateRules = `[[check]]
name  = "unit"
run   = "go test ./..."
paths = ["**/*.go"]

[[check]]
name     = "security"
runner   = "agent:security"
reporter = "independent"
paths    = ["**/*.go"]
outputs  = { required = ["findings.json"], artifacts = ["*.md", "*.png"] }

[[check]]
name   = "e2e"
when   = "ship"
runner = "agent:browser"
paths  = ["web/**"]
`

// checkEnv is acme/shop (authzEnv: alice owns acme, bob may write) with
// the checks of gateRules in its effort.
func newCheckEnv(t *testing.T) *authzEnv {
	t.Helper()
	e := newAuthzEnv(t)
	e.grant(service.RoleWrite)
	e.land(e.repo, e.effort(e.eff), "Checks", map[string]*string{"vx.toml": s(gateRules)}, nil)
	return e
}

// checkWorkspace opens a workspace of alice's agent with files on its base and
// returns it with the snapshot tree.
func (e *authzEnv) checkWorkspace(intent string, files map[string]*string) (service.WorkspaceView, hash.Hash) {
	e.t.Helper()
	ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, "acme/shop", e.eff.Slug, intent)
	if err != nil {
		e.t.Fatal(err)
	}
	return ws, e.snapshot(e.repo, ws.Workspace.Base, files)
}

func (e *authzEnv) report(a service.Actor, in service.ReportInput) (service.CheckResult, error) {
	in.Repo, in.Effort = "acme/shop", e.eff.Slug
	return e.svc.ReportCheck(e.ctx, a, in)
}

func (e *authzEnv) checkState(changeID string, tree hash.Hash, name string) service.CheckState {
	e.t.Helper()
	v, err := e.svc.CheckStatus(e.ctx, e.alice, service.CheckStatusInput{Repo: "acme/shop", Effort: e.eff.Slug, ChangeID: changeID, Tree: tree})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range v.Checks {
		if c.Def.Name == name {
			return c
		}
	}
	e.t.Fatalf("no check %s in %+v", name, v.Checks)
	return service.CheckState{}
}

var findings = []byte(`{"findings": []}`)

// A check that blocks land needs a passing result on exactly the snapshot
// that lands, from a reporter it allows: the author's own agent is not
// independent, bob's agent is. A run check is satisfied by vx land --run.
// A new tree needs new evidence.
func TestCheckGateOnLand(t *testing.T) {
	e := newCheckEnv(t)
	ws, tree := e.checkWorkspace("Pay", map[string]*string{"pay.go": s("package shop\n")})
	_, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
	if !isCode(err, service.CodeFailedPrecondition) {
		t.Fatalf("land without results: %v", err)
	}
	for _, want := range []string{
		"not landed: 2 checks that block land have no passing result on this change's tree " + tree.Short() + " (change " + ws.Workspace.ChangeID + "):",
		"unit      missing  vx land --run 'go test ./...' (or vx check run unit)",
		"security  missing  runner agent:security: vx check report security --status pass --summary TEXT (by someone independent",
		"vx check status shows what the change still needs",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%v", want, err)
		}
	}
	if st := e.checkState(ws.Workspace.ChangeID, tree, "e2e"); st.Applies || st.State != service.CheckStateNotNeeded {
		t.Fatalf("e2e covers web/** only: %+v", st)
	}

	// A docs change touches none of the checks' paths.
	docs, docsTree := e.checkWorkspace("Docs", map[string]*string{"README.md": s("shop\n")})
	if res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: docs.Workspace.ID, Tree: docsTree}); err != nil || !res.Landed {
		t.Fatalf("a change no check applies to: %+v %v", res, err)
	}

	in := service.ReportInput{ChangeID: ws.Workspace.ChangeID, Tree: tree, Name: "security", Status: "pass", Summary: "no findings", Data: findings, DataName: "findings.json"}
	if _, err := e.report(e.agent, in); err != nil {
		t.Fatal(err)
	}
	st := e.checkState(ws.Workspace.ChangeID, tree, "security")
	if st.State != service.CheckStateMissing || len(st.NotCounted()) != 1 || !strings.Contains(st.NotCounted()[0].Why, "agent:alice/claude made this change; an independent reporter must") {
		t.Fatalf("the author's own result: %+v", st)
	}
	if _, err := e.report(e.alice, in); err != nil {
		t.Fatal(err)
	}
	st = e.checkState(ws.Workspace.ChangeID, tree, "security")
	if st.State != service.CheckStateMissing || !strings.Contains(st.NotCounted()[0].Why, "user:alice is the person whose agent made it") {
		t.Fatalf("the author's person: %+v", st)
	}

	// Outputs: a passing result needs findings.json; artifacts must match.
	bad := in
	bad.Data, bad.DataName = nil, ""
	if _, err := e.report(e.bobAgent, bad); !isCode(err, service.CodeInvalid) || !strings.Contains(err.Error(), "must include findings.json (outputs.required)") {
		t.Fatalf("pass without the required output: %v", err)
	}
	bad = in
	bad.Artifacts = []service.ArtifactInput{{Name: "notes.txt", Data: []byte("x")}}
	if _, err := e.report(e.bobAgent, bad); !isCode(err, service.CodeInvalid) || !strings.Contains(err.Error(), `artifact "notes.txt" does not match the outputs`) {
		t.Fatalf("an artifact outputs.artifacts does not allow: %v", err)
	}
	bad = in
	bad.Data = []byte("{not json")
	if _, err := e.report(e.bobAgent, bad); !isCode(err, service.CodeInvalid) || !strings.Contains(err.Error(), "the data (findings.json) is not JSON") {
		t.Fatalf("data that is not JSON: %v", err)
	}
	if _, err := e.report(e.bobAgent, service.ReportInput{ChangeID: ws.Workspace.ChangeID, Tree: tree, Name: "nope", Status: "pass"}); !isCode(err, service.CodeNotFound) || !strings.Contains(err.Error(), "known: unit, security, e2e") {
		t.Fatalf("an unknown check: %v", err)
	}
	r, err := e.report(e.bobAgent, in)
	if err != nil || !r.Counts || r.Why != "" {
		t.Fatalf("an independent report counts: %+v %v", r, err)
	}
	st = e.checkState(ws.Workspace.ChangeID, tree, "security")
	if st.State != service.CheckStatePassed || st.Latest == nil || st.Latest.ID != r.ID || st.Latest.Reporter != "agent:bob/claude" || string(st.Latest.Data) != string(findings) {
		t.Fatalf("an independent result: %+v", st)
	}
	// A run check does not count a mere report.
	if _, err := e.report(e.agent, service.ReportInput{ChangeID: ws.Workspace.ChangeID, Tree: tree, Name: "unit", Status: "pass"}); err != nil {
		t.Fatal(err)
	}
	if st := e.checkState(ws.Workspace.ChangeID, tree, "unit"); st.State != service.CheckStateMissing || !strings.Contains(st.NotCounted()[0].Why, "reported, not run by vx") {
		t.Fatalf("a reported run check: %+v", st)
	}

	_, err = e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
	if err == nil || !strings.Contains(err.Error(), "1 check that blocks land has") || !strings.Contains(err.Error(), "unit") || strings.Contains(err.Error(), "security ") {
		t.Fatalf("only unit is missing now: %v", err)
	}
	// The gate lands exactly the snapshot: the docs change moved the
	// effort, so it is refused as moved rather than merged.
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree, Evidence: ran("go test ./...")})
	if err != nil || res.Landed || !res.EffortMoved {
		t.Fatalf("a gated land onto a moved effort: %+v %v", res, err)
	}
	ws2, tree2 := e.checkWorkspace("Pay", map[string]*string{"pay.go": s("package shop\n")})
	if _, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws2.Workspace.ID, Tree: tree2, Evidence: ran("go test ./...")}); err == nil || !strings.Contains(err.Error(), "security") {
		t.Fatalf("a new tree needs new evidence: %v", err)
	}
	in.ChangeID, in.Tree = ws2.Workspace.ChangeID, tree2
	if _, err := e.report(e.bobAgent, in); err != nil {
		t.Fatal(err)
	}
	res, err = e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws2.Workspace.ID, Tree: tree2, Evidence: ran("go test ./...")})
	if err != nil || !res.Landed {
		t.Fatalf("land with every result: %+v %v", res, err)
	}
	// The --run result is evidence of the landed change: under the check's
	// name, run by vx, on the tree it ran on.
	st = e.checkState(res.Changeset.ChangeID, hash.Zero, "unit")
	if st.State != service.CheckStatePassed || st.Latest.Source != store.CheckSourceLand || st.Latest.How != "go test ./..." || st.Latest.Reporter != "agent:alice/claude" {
		t.Fatalf("unit after the land: %+v", st.Latest)
	}
	// The changeset's evidence is as it always was.
	if ev := res.Changeset.Evidence; ev == nil || len(ev.Checks) != 1 || !ev.Checks[0].RanByVX || ev.Checks[0].Name != "go test ./..." {
		t.Fatalf("evidence = %+v", ev)
	}
	// A tree that is not part of the repository is refused.
	ws3, _ := e.checkWorkspace("Other", nil)
	if _, err := e.report(e.bobAgent, service.ReportInput{ChangeID: ws3.Workspace.ChangeID, Tree: hash.Of(hash.KindTree, []byte("nope")), Name: "security", Status: "fail"}); err == nil {
		t.Fatal("a report on a tree the repository lacks")
	}
}

// Logs and artifacts are objects of the repository: readable with read on
// it (and not found without), counted against the push quota, purged with
// the organization.
func TestCheckArtifactsAreRepoObjects(t *testing.T) {
	e := newCheckEnv(t)
	ws, tree := e.checkWorkspace("Pay", map[string]*string{"pay.go": s("package shop\n")})
	png := []byte("\x89PNG\r\n\x1a\nscreenshot of checkout")
	r, err := e.report(e.bobAgent, service.ReportInput{
		ChangeID: ws.Workspace.ChangeID, Tree: tree, Name: "security", Status: "fail", Summary: "SQL built from input",
		Log: []byte("scanned 3 files\n"), Artifacts: []service.ArtifactInput{{Name: "shot.png", Data: png}, {Name: "report.md", Data: []byte("# Findings\n")}},
		Started: time.Now().Add(-time.Minute), Finished: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Artifacts) != 2 || r.Artifacts[0].ContentType != "image/png" || r.Artifacts[1].Name != "report.md" || r.LogSize != 16 {
		t.Fatalf("result = %+v", r.CheckResult)
	}
	got, err := e.svc.GetObjects(e.ctx, e.alice, "acme/shop", []hash.Hash{r.Artifacts[0].Hash, r.LogHash}, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("artifacts are objects of acme/shop: %v %v", got, err)
	}
	if _, err := e.svc.GetObjects(e.ctx, e.bob, "globex/site", []hash.Hash{r.Artifacts[0].Hash}, 0); err == nil {
		t.Fatal("an artifact of acme/shop read through globex/site")
	}
	a, data, err := e.svc.CheckArtifact(e.ctx, e.bob, r.ID.String()[:8], "shot.png")
	if err != nil || string(data) != string(png) || a.Size != int64(len(png)) {
		t.Fatalf("artifact: %+v %q %v", a, data, err)
	}
	if _, log, err := e.svc.CheckArtifact(e.ctx, e.bob, r.ID.String(), ""); err != nil || string(log) != "scanned 3 files\n" {
		t.Fatalf("log: %q %v", log, err)
	}
	if _, _, err := e.svc.CheckArtifact(e.ctx, e.bob, r.ID.String(), "nope.gif"); !isCode(err, service.CodeNotFound) || !strings.Contains(err.Error(), "it has: report.md, shot.png") {
		t.Fatalf("unknown artifact: %v", err)
	}
	e.grant(service.RoleNone)
	if _, _, err := e.svc.CheckArtifact(e.ctx, e.bob, r.ID.String(), "shot.png"); !isCode(err, service.CodeNotFound) {
		t.Fatalf("an artifact without read: %v", err)
	}
	if _, err := e.svc.GetCheckResult(e.ctx, e.bob, r.ID.String()); !isCode(err, service.CodeNotFound) {
		t.Fatalf("a result without read: %v", err)
	}

	// Limits: per artifact (server setting) and the push quota.
	e.svc.Checks.MaxArtifactBytes = 8
	if _, err := e.report(e.alice, service.ReportInput{ChangeID: ws.Workspace.ChangeID, Tree: tree, Name: "security", Status: "fail", Artifacts: []service.ArtifactInput{{Name: "big.png", Data: png}}}); !isCode(err, service.CodeInvalid) || !strings.Contains(err.Error(), "one may be at most 8 B") {
		t.Fatalf("an artifact over the limit: %v", err)
	}
	e.svc.Checks.MaxArtifactBytes = 0
	push := int64(10)
	if err := e.svc.Store.SetOrgQuota(e.ctx, store.OrgQuota{OrgID: e.repo.OrgID, PushBytes: &push}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.report(e.alice, service.ReportInput{ChangeID: ws.Workspace.ChangeID, Tree: tree, Name: "security", Status: "fail", Artifacts: []service.ArtifactInput{{Name: "big.png", Data: png}}}); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("a report over the push quota: %v", err)
	}
	if err := e.svc.Store.SetOrgQuota(e.ctx, store.OrgQuota{OrgID: e.repo.OrgID}); err != nil {
		t.Fatal(err)
	}

	// The purge of acme deletes the results and their blobs.
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	e.later(service.DefaultOrgDeleteGrace + time.Minute)
	if done, err := e.svc.PurgeDueOrgs(e.ctx, nil); err != nil || len(done) != 1 {
		t.Fatalf("purge: %+v %v", done, err)
	}
	for _, h := range []hash.Hash{r.Artifacts[0].Hash, r.Artifacts[1].Hash, r.LogHash} {
		if ok, _ := e.blobs.Has(e.ctx, h); ok {
			t.Errorf("blob %s of a check result survived the purge", h.Short())
		}
	}
	if _, err := e.svc.Store.GetCheckResult(e.ctx, r.ID); err == nil {
		t.Error("the check result survived the purge")
	}
}

// A person's approval under a human [[policy]] is the policy's check
// "review", passed by that person; gated and required checks show as run
// checks.
func TestPolicyChecksInStatus(t *testing.T) {
	e := newPolicyEnv(t, policyRules)
	_, review := e.pending(e.agent, "001_x.sql")
	if _, err := e.svc.ApproveReview(e.ctx, e.alice, review.ID.String()); err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.CheckStatus(e.ctx, e.alice, service.CheckStatusInput{Repo: e.repo.Name, Effort: e.eff.Slug})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range v.Checks {
		who := ""
		if c.Latest != nil {
			who = c.Latest.Reporter + "/" + c.Latest.Source
		}
		got = append(got, string(c.Def.Origin)+":"+c.Def.Name+":"+c.State+":"+who)
	}
	want := []string{
		"required:go vet ./...:passed:agent:alice/claude/land",
		"policy:go test ./...:not needed:",
		"policy:go vet ./...:not needed:agent:alice/claude/land", // api/** only: not needed, its result shown
		"policy:go vet ./...:passed:agent:alice/claude/land",
		"policy:review:passed:user:alice/review",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("checks of the approved change:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !v.Head {
		t.Fatal("without a change, the status is of the effort head")
	}
	if _, err := e.svc.ReportCheck(e.ctx, e.alice, service.ReportInput{Repo: e.repo.Name, Effort: e.eff.Slug, Name: "review", Status: "pass"}); !isCode(err, service.CodeInvalid) || !strings.Contains(err.Error(), "vx review approve") {
		t.Fatalf("reporting the review check by hand: %v", err)
	}
}
