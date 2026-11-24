package web_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/web"
)

// TestAuditPageAndExport: Admin > Audit for superadmins and org admins,
// downloads with the session or a bearer token, refusals for members.
func TestAuditPageAndExport(t *testing.T) {
	e := newAdminEnv(t)
	code, body := e.get("alice", "/ui/admin/audit?since=1d")
	if code != http.StatusOK {
		t.Fatalf("audit page = %d\n%s", code, body)
	}
	for _, want := range []string{"Audit log", "org.create", "repo.create", "acme/shop", "Download JSONL", "Download CSV", "vx admin audit"} {
		if !strings.Contains(body, want) {
			t.Errorf("audit page lacks %q", want)
		}
	}
	// Bob owns only his personal organization: he sees its log, and is
	// refused acme's, where he is a member.
	if code, body := e.get("bob", "/ui/admin/audit"); code != http.StatusOK || strings.Contains(body, "acme/shop") || !strings.Contains(body, `<option value="bob" selected>`) {
		t.Errorf("bob's audit page = %d\n%s", code, body)
	}
	if code, body := e.get("bob", "/ui/admin/audit?org=acme"); code != http.StatusForbidden || !strings.Contains(body, "only owners and admins of acme") {
		t.Errorf("member's acme audit page = %d, want 403", code)
	}
	if _, body := e.get("alice", "/ui/admin/people"); !strings.Contains(body, `href="/ui/admin/audit"`) {
		t.Error("Admin > People has no link to Audit")
	}
	if _, body := e.get("alice", "/ui/orgs/acme"); !strings.Contains(body, `/ui/admin/audit?org=acme`) {
		t.Error("the org page has no audit link for its owner")
	}

	// Download with the browser session.
	resp, err := e.browsers["alice"].Get(e.srv.URL + web.AuditExportPath + "?org=acme&format=csv")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(string(b), "id,at,op,org,repo,actor,target,before,after\n") || !strings.Contains(resp.Header.Get("Content-Disposition"), "vx-audit-acme-") {
		t.Fatalf("csv download = %d %s %q\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"), b)
	}

	// With a bearer token, as vx admin audit does.
	tok, err := e.st.CreateToken(e.ctx, e.bob.ID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+web.AuditExportPath+"?org=acme", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "only owners and admins of acme") {
		t.Fatalf("member's export = %d %s", resp.StatusCode, b)
	}
	resp, err = http.Get(e.srv.URL + web.AuditExportPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous export = %d", resp.StatusCode)
	}
}

// TestOrgQuotaPanel: the org page shows quotas to members; superadmins
// change them there.
func TestOrgQuotaPanel(t *testing.T) {
	e := newAdminEnv(t)
	_, body := e.get("bob", "/ui/orgs/acme")
	if !strings.Contains(body, `id="quotas"`) || !strings.Contains(body, "Repositories") || strings.Contains(body, "Save quotas") {
		t.Fatalf("member's quota panel wrong:\n%s", body)
	}
	resp, body := e.post("alice", "/ui/orgs/acme/quota", url.Values{"repos": {"5"}, "stored": {"20GB"}, "push": {"0"}}, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Quotas saved.") {
		t.Fatalf("save quotas = %d\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"20 GB", "set for this organization", `value="20GB"`, `value="5"`} {
		if !strings.Contains(body, want) {
			t.Errorf("after saving, the page lacks %q", want)
		}
	}
	resp, body = e.post("alice", "/ui/orgs/acme/quota", url.Values{"stored": {"lots"}}, "")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "is not valid") {
		t.Errorf("bad quota = %d", resp.StatusCode)
	}
	resp, body = e.post("bob", "/ui/orgs/acme/quota", url.Values{"repos": {"500"}}, "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "only superadmins change quotas") {
		t.Errorf("member saving quotas = %d", resp.StatusCode)
	}

	// The disk warning shows to superadmins on every page.
	e.svc.Quotas.DataDir, e.svc.Quotas.DiskWarnPercent = t.TempDir(), 100
	if _, body := e.get("alice", "/ui/repos"); !strings.Contains(body, "almost full") {
		t.Error("no disk warning for the superadmin")
	}
	if _, body := e.get("bob", "/ui/repos"); strings.Contains(body, "almost full") {
		t.Error("disk warning shown to a member")
	}
}
