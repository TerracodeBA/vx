package service_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
)

func exportRows(t *testing.T, e *env, a service.Actor, q service.AuditQuery) []service.AuditRow {
	t.Helper()
	var out []service.AuditRow
	if err := e.svc.ExportAudit(e.ctx, a, q, func(r service.AuditRow) error { out = append(out, r); return nil }); err != nil {
		t.Fatalf("ExportAudit(%s, %+v): %v", a.Ref(), q, err)
	}
	return out
}

func opNames(rows []service.AuditRow) string {
	var ops []string
	for _, r := range rows {
		ops = append(ops, r.Op+"@"+r.Target)
	}
	return strings.Join(ops, " ")
}

// TestAuditExport: org admins export their organization with payloads;
// superadmins export the whole server with repository payloads left out
// (#33: operation metadata yes, repository content no); members and
// outsiders cannot.
func TestAuditExport(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	root := e.siteAdmin("root")
	bob := e.person("bob")
	if _, err := svc.CreateOrg(ctx, e.alice, "acme", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateOrg(ctx, bob, "other", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRepo(ctx, bob, service.CreateRepoInput{Name: "other/web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/api", Name: "Secret plan", Goal: "content"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetMember(ctx, e.alice, "acme", "bob", service.OrgMember); err != nil {
		t.Fatal(err)
	}

	// Alice owns acme: its org-level entries and its repository's, with
	// payloads; nothing of "other".
	rows := exportRows(t, e, e.alice, service.AuditQuery{Org: "acme"})
	got := opNames(rows)
	for _, want := range []string{"org.create@acme", "repo.create@acme/api", "effort.create@secret-plan", "org.member@acme"} {
		if !strings.Contains(got, want) {
			t.Errorf("acme export lacks %s: %s", want, got)
		}
	}
	if strings.Contains(got, "other") {
		t.Errorf("acme export leaks the other organization: %s", got)
	}
	for _, r := range rows {
		if r.Org != "acme" {
			t.Errorf("row %d org = %q", r.ID, r.Org)
		}
		if r.Op == "effort.create" && (r.Repo != "acme/api" || r.Actor != "user:alice" || len(r.After) == 0) {
			t.Errorf("effort.create row for the org owner = %+v", r)
		}
	}

	// Bob is only a member of acme: refused, and pointed at the owners.
	err := svc.ExportAudit(ctx, bob, service.AuditQuery{Org: "acme"}, func(service.AuditRow) error { return nil })
	wantErr(t, err, service.CodePermission, "only owners and admins of acme export its audit log", "user:alice")
	// Without --org, only superadmins.
	err = svc.ExportAudit(ctx, e.alice, service.AuditQuery{}, func(service.AuditRow) error { return nil })
	wantErr(t, err, service.CodePermission, "only superadmins export the whole server")
	// A repository of another organization is not found through acme.
	err = svc.ExportAudit(ctx, e.alice, service.AuditQuery{Org: "acme", Repo: "other/web"}, func(service.AuditRow) error { return nil })
	wantErr(t, err, service.CodeNotFound, "other/web not found")
	// A stranger does not learn acme exists.
	err = svc.ExportAudit(ctx, e.person("mallory"), service.AuditQuery{Org: "acme"}, func(service.AuditRow) error { return nil })
	if code(err) != service.CodeNotFound {
		t.Errorf("stranger: %v", err)
	}

	// The superadmin sees both organizations, metadata only for
	// repository entries.
	rows = exportRows(t, e, root, service.AuditQuery{})
	got = opNames(rows)
	for _, want := range []string{"repo.create@acme/api", "repo.create@other/web", "effort.create@secret-plan", "org.create@other"} {
		if !strings.Contains(got, want) {
			t.Errorf("server export lacks %s: %s", want, got)
		}
	}
	for _, r := range rows {
		if r.Repo != "" && (r.Before != nil || r.After != nil) {
			t.Errorf("superadmin got the payload of repository entry %s %s: %s", r.Op, r.Target, r.After)
		}
	}

	// Filters: actor, repository, time.
	rows = exportRows(t, e, root, service.AuditQuery{Actor: "bob"})
	for _, r := range rows {
		if r.Actor != "user:bob" {
			t.Errorf("actor filter let %s through", r.Actor)
		}
	}
	if len(rows) == 0 {
		t.Error("no entries by bob")
	}
	rows = exportRows(t, e, e.alice, service.AuditQuery{Org: "acme", Repo: "acme/api"})
	for _, r := range rows {
		if r.Repo != "acme/api" {
			t.Errorf("repo filter let %+v through", r)
		}
	}
	if rows := exportRows(t, e, root, service.AuditQuery{Since: time.Now().Add(time.Hour)}); len(rows) != 0 {
		t.Errorf("entries from the future: %s", opNames(rows))
	}
	if rows := exportRows(t, e, root, service.AuditQuery{Until: time.Now().Add(-time.Hour)}); len(rows) != 0 {
		t.Errorf("entries before the test: %s", opNames(rows))
	}
	if rows := exportRows(t, e, root, service.AuditQuery{Newest: 2}); len(rows) != 2 || rows[0].ID < rows[1].ID {
		t.Errorf("newest 2 = %+v", rows)
	}

	// Formats.
	var buf bytes.Buffer
	if err := svc.WriteAudit(ctx, e.alice, service.AuditQuery{Org: "acme"}, "jsonl", &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first["op"] == nil || first["at"] == nil {
		t.Fatalf("jsonl line = %s (%v)", lines[0], err)
	}
	buf.Reset()
	if err := svc.WriteAudit(ctx, e.alice, service.AuditQuery{Org: "acme"}, "csv", &buf); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(recs) != len(lines)+1 || strings.Join(recs[0], ",") != "id,at,op,org,repo,actor,target,before,after" {
		t.Fatalf("csv = %v rows (%v), header %v", len(recs), err, recs[0])
	}
	err = svc.WriteAudit(ctx, e.alice, service.AuditQuery{Org: "acme"}, "xml", &buf)
	wantErr(t, err, service.CodeInvalid, "use jsonl or csv")
}

func TestAuditRetention(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	if _, err := svc.CreateOrg(ctx, e.alice, "acme", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.PruneAudit(ctx); n != 0 || err != nil {
		t.Fatalf("no retention pruned %d, %v", n, err)
	}
	svc.Audit.Retention = 30 * 24 * time.Hour
	svc.Now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	n, err := svc.PruneAudit(ctx)
	if err != nil || n == 0 {
		t.Fatalf("PruneAudit = %d, %v", n, err)
	}
	if got := opsOf(t, svc, e.alice.ID, ""); len(got) != 0 {
		t.Errorf("entries left after pruning: %v", got)
	}

	for in, want := range map[string]time.Duration{"": 0, "90d": 90 * 24 * time.Hour, "12w": 84 * 24 * time.Hour, "1y": 365 * 24 * time.Hour, "2160h": 90 * 24 * time.Hour} {
		if got, err := service.ParseRetention(in); err != nil || got != want {
			t.Errorf("ParseRetention(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := service.ParseRetention("1d"); err == nil {
		t.Error("a retention under 7 days was accepted")
	}
}
