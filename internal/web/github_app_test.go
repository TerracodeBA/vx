package web_test

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
)

// A site admin creates the GitHub App from Admin > GitHub: the page's form
// posts the manifest to GitHub, GitHub sends the browser back, one click
// finishes, and the page then shows the install link.
func TestGitHubAdminCreatesApp(t *testing.T) {
	e := newAdminEnv(t)
	gh := githubtest.New(t)
	gh.Prepare(t, e.svc)
	e.svc.GitHub.PublicURL = e.srv.URL

	// Only site admins; agents see the page but cannot create.
	if code, _ := e.get("bob", "/ui/admin/github"); code != http.StatusNotFound {
		t.Fatalf("bob: %d", code)
	}
	if _, body := e.get("agent", "/ui/admin/github"); !strings.Contains(body, "Agents cannot set up the App") {
		t.Fatalf("agent page:\n%s", body)
	}
	code, body := e.get("alice", "/ui/admin/github?org=acme")
	if code != 200 || !strings.Contains(body, "Create the GitHub App") {
		t.Fatalf("admin page %d:\n%s", code, body)
	}
	action := regexp.MustCompile(`<form method="post" action="([^"]+)" id="gh-create">`).FindStringSubmatch(body)
	manifest := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindStringSubmatch(body)
	if action == nil || manifest == nil {
		t.Fatalf("no manifest form:\n%s", body)
	}
	formURL := html.UnescapeString(action[1])
	if !strings.HasPrefix(formURL, gh.URL+"/organizations/acme/settings/apps/new?state=") {
		t.Fatalf("form posts to %s", formURL)
	}
	// The browser posts the form to GitHub (the org form path redirects
	// like the personal one in the fake: post to the personal path).
	u, _ := url.Parse(formURL)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.PostForm(gh.URL+"/settings/apps/new?"+u.RawQuery, url.Values{"manifest": {html.UnescapeString(manifest[1])}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(back.String(), e.srv.URL+"/ui/admin/github/created?") {
		t.Fatalf("GitHub sends the browser to %s", back)
	}
	// The return page needs no session (it arrives cross-site).
	resp, err = http.Get(back.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("return page %d", resp.StatusCode)
	}
	finish := url.Values{"code": {back.Query().Get("code")}, "state": {back.Query().Get("state")}}
	// Someone else's session cannot use alice's state, and a cross-site post is refused.
	if resp, _ := e.post("bob", "/ui/admin/github/finish", finish, ""); resp.StatusCode == http.StatusSeeOther {
		t.Fatal("bob finished alice's setup")
	}
	if resp, _ := e.post("alice", "/ui/admin/github/finish", finish, "https://evil.example"); resp.StatusCode == http.StatusSeeOther || resp.StatusCode == 200 {
		t.Fatalf("cross-site finish: %d", resp.StatusCode)
	}
	resp, body = e.post("alice", "/ui/admin/github/finish", finish, "")
	if resp.Request.URL.Path != "/ui/admin/github" || !strings.Contains(body, "The GitHub App is set up") {
		t.Fatalf("finish: %d %s\n%s", resp.StatusCode, resp.Request.URL, body)
	}
	creds, _ := gh.App()
	if !strings.Contains(body, creds.HTMLURL+"/installations/new") || !strings.Contains(body, creds.ClientID) {
		t.Fatalf("no install link:\n%s", body)
	}
	if strings.Contains(body, creds.ClientSecret) || strings.Contains(body, creds.WebhookSecret) {
		t.Fatal("the page shows a secret")
	}
}

func TestGitHubWebhookEndpoint(t *testing.T) {
	e := newAdminEnv(t)
	gh := githubtest.New(t)
	hook := e.srv.URL + "/hooks/github"
	// Without an App the endpoint says so.
	if code := gh.DeliverRaw(t, hook, githubtest.Delivery{Event: "ping", Body: []byte(`{}`), Signature: "sha256=00"}); code != http.StatusServiceUnavailable {
		t.Fatalf("no App: %d", code)
	}
	gh.Configure(t, e.svc)
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	if code := gh.DeliverRaw(t, hook, githubtest.Delivery{Event: "ping", Body: body}); code != 200 {
		t.Fatalf("good signature: %d", code)
	}
	if code := gh.DeliverRaw(t, hook, githubtest.Delivery{Event: "ping", Body: body, Signature: "-"}); code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", code)
	}
	if code := gh.DeliverRaw(t, hook, githubtest.Delivery{Event: "ping", Body: body, Signature: githubtest.Sign("wrong", body)}); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", code)
	}
}

// GitHub's setup URL brings a new installation in at once, without a
// session, and asks GitHub only for installations vx does not know.
func TestGitHubInstalledPage(t *testing.T) {
	e := newAdminEnv(t)
	gh := githubtest.New(t)
	gh.Configure(t, e.svc)
	gh.AddOrg("globex")
	gh.AddRepo("globex/app", true)
	id := gh.Install("globex")
	page := fmt.Sprintf("%s/ui/github/installed?installation_id=%d&setup_action=install", e.srv.URL, id)
	for range 2 {
		resp, err := http.Get(page)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("installed page %d", resp.StatusCode)
		}
	}
	if _, err := e.st.RepoByName(e.ctx, "globex/app"); err != nil {
		t.Fatalf("installation not synced: %v", err)
	}
	if n := gh.Calls(fmt.Sprintf("GET /app/installations/%d", id)); n != 1 {
		t.Fatalf("asked GitHub %d times", n)
	}
}
