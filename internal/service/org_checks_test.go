package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/service"
)

const orgChecksV1 = `[[check]]
name   = "security-review"
when   = "ship"
runner = "human"
blocks = "ship"

[[check]]
name   = "unit"
run    = "go test ./..."
blocks = "land"
`

// Only owners and admins, and only people, change an organization's
// checks; members read them; every version is kept and logged.
func TestOrgChecksOwnersAndAdminsOnly(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	alice := e.alice
	bob, carol, dave := e.person("bob"), e.person("carol"), e.person("dave")
	if _, err := svc.CreateOrg(ctx, alice, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	for who, role := range map[string]service.OrgRole{"bob": service.OrgAdmin, "carol": service.OrgMember} {
		if _, err := svc.SetMember(ctx, alice, "acme", who, role); err != nil {
			t.Fatal(err)
		}
	}
	aliceAgent := e.agentOf(alice, "rules-bot")

	// Nothing defined yet.
	if cur, err := svc.GetOrgChecks(ctx, carol, "acme"); err != nil || cur.Version != 0 {
		t.Fatalf("GetOrgChecks before any = %+v, %v", cur, err)
	}
	// A member cannot change them; an outsider does not see the org; an
	// agent cannot either, even its owner's.
	if _, _, err := svc.SetOrgChecks(ctx, carol, "acme", orgChecksV1); code(err) != service.CodePermission || !strings.Contains(err.Error(), "owner or admin") {
		t.Errorf("member SetOrgChecks: %v", err)
	}
	if _, _, err := svc.SetOrgChecks(ctx, dave, "acme", orgChecksV1); code(err) != service.CodeNotFound {
		t.Errorf("outsider SetOrgChecks: %v", err)
	}
	if _, err := svc.GetOrgChecks(ctx, dave, "acme"); code(err) != service.CodeNotFound {
		t.Errorf("outsider GetOrgChecks: %v", err)
	}
	if _, _, err := svc.SetOrgChecks(ctx, aliceAgent, "acme", orgChecksV1); code(err) != service.CodePermission {
		t.Errorf("agent SetOrgChecks: %v", err)
	}
	// An admin saves version 1, the owner version 2.
	v1, changed, err := svc.SetOrgChecks(ctx, bob, "acme", orgChecksV1)
	if err != nil || !changed || v1.Version != 1 || strings.Join(v1.Names(), ",") != "security-review,unit" || v1.By != "user:bob" {
		t.Fatalf("admin SetOrgChecks = %+v, %v, %v", v1, changed, err)
	}
	if _, changed, err := svc.SetOrgChecks(ctx, bob, "acme", orgChecksV1); err != nil || changed {
		t.Errorf("saving the same again: changed %v, %v", changed, err)
	}
	v2src := orgChecksV1 + "\n[[check]]\nname = \"e2e\"\nafter = [\"unit\"]\nrunner = \"agent:browser\"\n"
	v2, _, err := svc.SetOrgChecks(ctx, alice, "acme", v2src)
	if err != nil || v2.Version != 2 {
		t.Fatalf("owner SetOrgChecks = %+v, %v", v2, err)
	}
	cur, err := svc.GetOrgChecks(ctx, carol, "acme")
	if err != nil || cur.Version != 2 || len(cur.Checks) != 3 || string(cur.Checks[0].Origin) != "org" {
		t.Fatalf("member GetOrgChecks = %+v, %v", cur, err)
	}
	// Invalid tables are refused with the reason; nothing is saved.
	if _, _, err := svc.SetOrgChecks(ctx, alice, "acme", "[[check]]\nname = \"a\"\nrun = \"true\"\nafter = [\"b\"]\n[[check]]\nname = \"b\"\nrun = \"true\"\nafter = [\"a\"]\n"); code(err) != service.CodeInvalid || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cyclic org checks: %v", err)
	}
	if _, _, err := svc.SetOrgChecks(ctx, alice, "acme", "[[check]]\nname = \"x\"\nrun = \"true\"\nafter = [\"repo-only\"]\n"); code(err) != service.CodeInvalid || !strings.Contains(err.Error(), `"repo-only", which is not a check here`) {
		t.Errorf("org check after an unknown check: %v", err)
	}
	hist, err := svc.OrgCheckHistory(ctx, carol, "acme")
	if err != nil || len(hist) != 2 || hist[0].Version != 2 || hist[1].Version != 1 || hist[1].Source != orgChecksV1 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	// Clearing is a version too.
	if v3, _, err := svc.SetOrgChecks(ctx, alice, "acme", ""); err != nil || v3.Version != 3 || len(v3.Checks) != 0 {
		t.Errorf("clear = %+v, %v", v3, err)
	}
	// Every change is in the operation log with what it was before.
	rows := exportRows(t, e, alice, service.AuditQuery{Org: "acme"})
	n := 0
	for _, r := range rows {
		if r.Op == "org.checks" && r.Target == "acme" {
			n++
			if r.Actor == "" || len(r.After) == 0 {
				t.Errorf("org.checks entry without actor or after: %+v", r)
			}
		}
	}
	if n != 3 {
		t.Errorf("org.checks entries = %d, want 3: %s", n, opNames(rows))
	}
}

// A repository adds checks of its own but cannot remove or weaken an
// organization check: one with the same name in vx.toml is ignored with a
// warning that says why.
func TestOrgChecksRepoCannotWeaken(t *testing.T) {
	org := service.OrgChecks{Org: "acme", Version: 4}
	defs, err := service.ParseOrgChecks(orgChecksV1)
	if err != nil {
		t.Fatal(err)
	}
	org.Checks = defs
	repo, err := rules.ParseChecks([]byte(`[[check]]
name   = "unit"
run    = "true"
blocks = "nothing"

[[check]]
name  = "lint"
run   = "golangci-lint run"
after = ["unit"]

[[check]]
name   = "deploy"
runner = "ci"
after  = ["staging"]
`))
	if err != nil {
		t.Fatal(err)
	}
	all, warnings := service.MergeOrgChecks(org, repo)
	var got []string
	for _, c := range all {
		got = append(got, c.Name+"@"+service.CheckOrigin(c, org))
	}
	want := "security-review@organization acme (version 4),unit@organization acme (version 4),lint@vx.toml line 6,deploy@vx.toml line 11"
	if strings.Join(got, ",") != want {
		t.Errorf("merged = %s\nwant     %s", strings.Join(got, ","), want)
	}
	for _, c := range all {
		if c.Name == "unit" && c.Run != "go test ./..." {
			t.Errorf("the organization's unit was replaced: %+v", c)
		}
	}
	w := strings.Join(warnings, "\n")
	for _, want := range []string{
		`vx.toml [[check]] "unit" is ignored: organization acme defines a check of that name, and a repository cannot change or remove an organization check`,
		`vx.toml [[check]] "deploy": after names "staging", which is not a check here`,
	} {
		if !strings.Contains(w, want) {
			t.Errorf("missing warning %q in:\n%s", want, w)
		}
	}
	if strings.Contains(w, "lint") {
		t.Errorf("lint runs after the organization's unit, which is fine:\n%s", w)
	}
}
