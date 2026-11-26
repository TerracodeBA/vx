package web_test

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// The organization page shows the organization's checks; owners and
// admins edit them there, as a new version each time; members read them;
// agents and outsiders cannot change them.
func TestOrgChecksPage(t *testing.T) {
	e := newAdminEnv(t)
	src := "[[check]]\nname   = \"security-review\"\nwhen   = \"ship\"\nrunner = \"human\"\nblocks = \"ship\"\n"
	_, page := e.get("alice", "/ui/orgs/acme")
	if !strings.Contains(page, `id="checks"`) || !strings.Contains(page, `action="/ui/orgs/acme/checks"`) || !strings.Contains(page, "No organization checks") {
		t.Fatalf("owner's org page has no checks panel:\n%s", page)
	}
	resp, body := e.post("alice", "/ui/orgs/acme/checks", url.Values{"source": {src}}, "")
	if resp.StatusCode != 200 || !strings.Contains(body, "Organization checks saved as a new version") || !strings.Contains(body, "version 1") || !strings.Contains(body, "security-review") {
		t.Fatalf("save: %d\n%s", resp.StatusCode, body)
	}
	// A member reads them but has no form.
	_, page = e.get("bob", "/ui/orgs/acme")
	if !strings.Contains(page, "security-review") || strings.Contains(page, `action="/ui/orgs/acme/checks"`) {
		t.Fatalf("member's org page:\n%s", page)
	}
	if resp, _ := e.post("bob", "/ui/orgs/acme/checks", url.Values{"source": {""}}, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("member saved org checks: %d", resp.StatusCode)
	}
	if resp, _ := e.post("agent", "/ui/orgs/acme/checks", url.Values{"source": {""}}, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("agent saved org checks: %d", resp.StatusCode)
	}
	if resp, _ := e.post("alice", "/ui/orgs/acme/checks", url.Values{"source": {""}}, "https://evil.example"); resp.StatusCode == http.StatusSeeOther || resp.StatusCode == 200 {
		t.Errorf("cross-site save: %d", resp.StatusCode)
	}
	if code, _ := e.get("carol", "/ui/orgs/acme"); code != http.StatusNotFound {
		t.Errorf("outsider sees the org page: %d", code)
	}
	// A cycle is refused with the reason, on the page.
	resp, body = e.post("alice", "/ui/orgs/acme/checks", url.Values{"source": {"[[check]]\nname = \"a\"\nrun = \"true\"\nafter = [\"a\"]\n"}}, "")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "after names the check itself") {
		t.Errorf("invalid checks: %d\n%s", resp.StatusCode, body)
	}
	_, page = e.get("alice", "/ui/orgs/acme")
	if !strings.Contains(page, "History (1 version)") {
		t.Errorf("no history:\n%s", page)
	}
}

// Artifacts and logs of check results are "not found" to anyone without
// read on the repository, whatever the result id.
func TestCheckArtifactsNeedRead(t *testing.T) {
	e := newAdminEnv(t)
	id := uuid.NewString()
	for _, path := range []string{
		"/ui/checks/artifact?repo=" + url.QueryEscape(adminRepo) + "&result=" + id + "&name=flow.gif",
		"/ui/checks/log?repo=" + url.QueryEscape(adminRepo) + "&result=" + id,
		"/ui/checks/artifact?repo=" + url.QueryEscape(adminRepo) + "&result=not-a-uuid&name=x",
	} {
		if code, body := e.get("carol", path); code != http.StatusNotFound || strings.Contains(body, "flow.gif") {
			t.Errorf("outsider %s: %d\n%s", path, code, body)
		}
	}
}

// The Checks tab and the change page show the pipeline with results: a
// screenshot previewed inline, other artifacts as downloads, the data and
// the skipped check with its reason. Someone without read on the
// repository gets "not found" for the page and for every artifact.
func TestChecksPagesAndArtifacts(t *testing.T) {
	e := newPhoneEnv(t)
	_, page := e.get("/ui/effort?repo=acme%2Fshop&slug=" + e.effort + "&view=checks")
	for _, want := range []string{
		`id="check-unit"`, `class="check-row check-depth-1" id="check-browser-e2e"`, `class="check-row check-depth-2" id="check-deploy-preview"`,
		"skipped: browser-e2e failed", "Signup form: the submit button stays disabled", "agent, accepted",
		`<img class="check-shot" src="/ui/checks/artifact?`, "report.html", `>Log</a>`, "coverage", "81.5", "vx.toml line",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Checks tab lacks %q", want)
		}
	}
	m := regexp.MustCompile(`/ui/checks/artifact\?repo=acme%2fshop&amp;result=([0-9a-f-]{36})&amp;name=login-flow-with-a-long-file-name.png"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no artifact link:\n%s", page)
	}
	art := "/ui/checks/artifact?repo=acme%2Fshop&result=" + m[1] + "&name="
	get := func(path string) *http.Response {
		resp, err := e.c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get(art + "login-flow-with-a-long-file-name.png"); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(r.Header.Get("Content-Disposition"), "inline") || r.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("image: %d %v", r.StatusCode, r.Header)
	}
	if r := get(art + "login-flow-with-a-long-file-name.png&download=1"); !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("download: %v", r.Header)
	}
	if r := get(art + "report.html"); r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("an HTML artifact is not a download: %d %v", r.StatusCode, r.Header)
	}
	if r := get("/ui/checks/log?repo=acme%2Fshop&result=" + m[1]); r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("log: %d %v", r.StatusCode, r.Header)
	}
	// The change page of the effort head shows the same checks.
	_, ef := e.get("/ui/effort?repo=acme%2Fshop&slug=" + e.effort)
	if c := regexp.MustCompile(`href="(/ui/change\?repo=[^"]+&amp;ref=[^"]+)"`).FindStringSubmatch(ef); c != nil {
		if _, cp := e.get(strings.ReplaceAll(c[1], "&amp;", "&")); !strings.Contains(cp, `id="checks"`) {
			t.Errorf("change page without checks")
		}
	}

	// Carol has no role on acme/shop.
	carol, err := e.st.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.st.CreateToken(e.ctx, carol.ID, "web")
	jar, _ := cookiejar.New(nil)
	e.c = &http.Client{Jar: jar}
	e.signIn(tok)
	for _, path := range []string{art + "login-flow-with-a-long-file-name.png", art + "report.html", "/ui/checks/log?repo=acme%2Fshop&result=" + m[1],
		"/ui/checks/artifact?repo=other%2Frepo&result=" + m[1] + "&name=report.html", "/ui/effort?repo=acme%2Fshop&slug=" + e.effort + "&view=checks"} {
		if r := get(path); r.StatusCode != http.StatusNotFound {
			t.Errorf("carol %s: %d", path, r.StatusCode)
		}
	}
}
