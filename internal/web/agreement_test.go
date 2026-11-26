package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
)

// fakeAgreement is obviously fake fixture text with what a hostile or
// careless Markdown file could hold: raw HTML, a javascript: link, a table
// with alignment.
const fakeAgreement = "# Test agreement, version 1\n\nThis is **fake** fixture text for tests.\n\n<script>alert(1)</script>\n\n[a link](javascript:alert(1))\n\n| Left | Right |\n|:-----|------:|\n| 1 | 2 |\n"

// withAgreement turns the agreement step on with one version in effect.
func withAgreement(t *testing.T, e *adminEnv) service.AgreementVersion {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "v1.md"), []byte(fakeAgreement), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(map[string]any{"versions": []map[string]string{{"id": "v1", "file": "v1.md", "published": "2026-01-01", "effective": "2026-01-01"}}})
	if err := os.WriteFile(filepath.Join(dir, service.AgreementManifest), m, 0o644); err != nil {
		t.Fatal(err)
	}
	vs, err := service.LoadAgreementVersions(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Agreement = service.AgreementSettings{Required: true, Dir: dir, Versions: vs}
	return vs[0]
}

// agreementSection is the Agreement panel of an organization page.
func agreementSection(page string) string {
	i := strings.Index(page, `<section class="panel" id="agreement">`)
	if i < 0 {
		return ""
	}
	j := strings.Index(page[i:], "</section>")
	return page[i : i+j]
}

func TestAgreementPage(t *testing.T) {
	e := newAdminEnv(t)
	// Off: no panel, no banner.
	if _, page := e.get("alice", "/ui/orgs/acme"); strings.Contains(page, `id="agreement"`) || strings.Contains(page, "data processing agreement") {
		t.Fatal("the agreement shows with the step off")
	}
	v := withAgreement(t, e)

	resp, err := e.browsers["alice"].Get(e.srv.URL + "/ui/orgs/acme")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	_, page := e.get("alice", "/ui/orgs/acme")
	sec := agreementSection(page)
	for _, want := range []string{"<strong>fake</strong>", `<th align="left">Left</th>`, `name="agree"`, `value="` + v.SHA256 + `"`, ">Accept</button>", "cannot be used for work", "No version accepted yet"} {
		if !strings.Contains(sec, want) {
			t.Errorf("owner's panel lacks %q:\n%s", want, sec)
		}
	}
	for _, bad := range []string{"<script>alert(1)", "javascript:", "style="} {
		if strings.Contains(sec, bad) {
			t.Errorf("panel contains %q", bad)
		}
	}
	if !strings.Contains(page, `href="/ui/orgs/acme#agreement">Read and accept</a>`) {
		t.Error("the owner sees no banner")
	}
	_, page = e.get("bob", "/ui/orgs/acme")
	if sec := agreementSection(page); !strings.Contains(sec, "Only an owner of acme") || strings.Contains(sec, `name="agree"`) {
		t.Errorf("member's panel:\n%s", sec)
	}
	if strings.Contains(page, "Read and accept</a>") {
		t.Error("a member sees the banner; only owners can act on it")
	}

	accept := url.Values{"version": {v.ID}, "sha256": {v.SHA256}, "agree": {"yes"}}
	if resp, _ := e.post("alice", "/ui/orgs/acme/agreement", accept, "https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin accept = %d", resp.StatusCode)
	}
	if resp, body := e.post("agent", "/ui/orgs/acme/agreement", accept, ""); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "agents cannot accept") {
		t.Errorf("agent accept = %d", resp.StatusCode)
	}
	if resp, _ := e.post("bob", "/ui/orgs/acme/agreement", accept, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("member accept = %d", resp.StatusCode)
	}
	if resp, body := e.post("alice", "/ui/orgs/acme/agreement", url.Values{"version": {v.ID}, "sha256": {v.SHA256}}, ""); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "tick the box") {
		t.Errorf("accept without the box = %d", resp.StatusCode)
	}
	if st, _ := e.svc.OrgAgreement(e.ctx, e.alice, "acme"); len(st.History) != 0 {
		t.Fatalf("refused posts recorded acceptances: %+v", st.History)
	}
	resp, body := e.post("alice", "/ui/orgs/acme/agreement", accept, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Data processing agreement accepted") {
		t.Fatalf("accept = %d\n%s", resp.StatusCode, agreementSection(body))
	}
	st, err := e.svc.OrgAgreement(e.ctx, e.alice, "acme")
	if err != nil || !st.Usable || len(st.History) != 1 {
		t.Fatalf("after accepting: %+v, %v", st, err)
	}
	rec := st.History[0]
	if rec.VersionID != "v1" || rec.SHA256 != v.SHA256 || rec.AcceptedByRef != "user:alice" || rec.IP != "127.0.0.1" || time.Since(rec.AcceptedAt) > time.Minute {
		t.Errorf("acceptance = %+v", rec)
	}
	sec = agreementSection(body)
	if !strings.Contains(sec, "has accepted version v1") || !strings.Contains(sec, "/ui/orgs/acme/agreement/accepted/"+rec.ID.String()) || strings.Contains(sec, `name="agree"`) {
		t.Errorf("panel after accepting:\n%s", sec)
	}
	if strings.Contains(body, "Read and accept</a>") {
		t.Error("the banner stays after accepting")
	}

	// The accepted text, viewed and downloaded exactly as accepted, by any
	// member; not by outsiders.
	code, page := e.get("bob", "/ui/orgs/acme/agreement/accepted/"+rec.ID.String())
	if code != 200 || !strings.Contains(page, "<strong>fake</strong>") || !strings.Contains(page, "exactly as it was accepted") || strings.Contains(page, "<script>alert(1)") {
		t.Errorf("accepted page = %d", code)
	}
	dl, err := e.browsers["bob"].Get(e.srv.URL + "/ui/orgs/acme/agreement/accepted/" + rec.ID.String() + "?download=1")
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, len(fakeAgreement)+10)
	n, _ := dl.Body.Read(raw)
	dl.Body.Close()
	if string(raw[:n]) != fakeAgreement || !strings.Contains(dl.Header.Get("Content-Disposition"), `filename="dpa-acme-v1-accepted-`) || dl.Header.Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Errorf("download = %q %v", raw[:n], dl.Header)
	}
	if code, _ := e.get("carol", "/ui/orgs/acme/agreement/accepted/"+rec.ID.String()); code != http.StatusNotFound {
		t.Errorf("outsider sees the accepted text: %d", code)
	}
	if code, page := e.get("alice", "/ui/orgs/acme/agreement?version=v1"); code != 200 || !regexp.MustCompile(`SHA-256 <span class="mono">`+v.SHA256).MatchString(page) {
		t.Errorf("version page = %d", code)
	}
}

// TestAgreementForYou: an owner of an organization without the agreement
// sees an item on the For you page linking to the Agreement section.
func TestAgreementForYou(t *testing.T) {
	e := newAdminEnv(t)
	withAgreement(t, e)
	_, page := e.get("alice", "/ui/for-you")
	if !strings.Contains(page, "fy-agreement") || !strings.Contains(page, `href="/ui/orgs/acme#agreement"`) {
		t.Errorf("For you lacks the agreement item")
	}
	if _, page := e.get("bob", "/ui/for-you"); strings.Contains(page, "fy-agreement") {
		t.Error("a member gets the agreement item")
	}
}
