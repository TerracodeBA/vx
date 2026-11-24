package web_test

import (
	"strings"
	"testing"
)

// Before the server has a GitHub App, the welcome page gives site admins
// the button to create it; everyone else is told who sets it up.
func TestWelcomeOffersGitHubSetupToSiteAdmins(t *testing.T) {
	e := newAdminEnv(t)
	code, page := e.get("alice", "/ui/welcome")
	if code != 200 || !strings.Contains(page, `href="/ui/admin/github"`) || !strings.Contains(page, "Set up the GitHub App") {
		t.Fatalf("site admin's welcome page = %d, lacks the setup button:\n%s", code, page)
	}
	code, page = e.get("bob", "/ui/welcome")
	if code != 200 || strings.Contains(page, "Set up the GitHub App") || !strings.Contains(page, "A site admin sets it up") {
		t.Fatalf("other people's welcome page = %d:\n%s", code, page)
	}
}
