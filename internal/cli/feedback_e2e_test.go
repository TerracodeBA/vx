package cli_test

// Review comments and CI results from GitHub reach the agent that wrote
// the code, end to end: an effort ships through the real git bridge, the
// fake GitHub delivers signed webhooks to the server's /hooks/github, and
// the agent sees them in vx inbox, the sync feed, vx wait, vx ship list and
// vx status. When main moves, the effort rebases itself and the agent's
// next sync says so.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

func TestReviewCommentReachesTheAgentsInbox(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	blobs, err := blob.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, blobs, gitbridge.New(st, blobs, t.TempDir(), nil))
	gh := githubtest.New(t)
	gh.Configure(t, svc)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &harness{t: t, server: srv.URL, home: t.TempDir(), st: st}
	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateToken(ctx, alice.ID, "test")
	if err != nil {
		t.Fatal(err)
	}

	up := newGitUpstream(t)
	up.commit("Initial", map[string]string{"README.md": "# shop\n"})
	work := t.TempDir()
	person := h.user("alice", tok, work)
	person.must("repo", "create", "acme/shop", "--git", up.dir)
	person.must("effort", "new", "Rate limits", "--repo", "acme/shop")
	agent := h.user("claude", agentToken(t, person, "claude"), work)
	agent.must("clone", "acme/shop", "rate-limits", "a")
	agent.dir = filepath.Join(work, "a")
	agent.must("new", "Add limiter")
	agent.write("limit.go", "package shop\n\nfunc Allow() bool {\n\treturn true\n}\n")
	agent.must("land")
	person.must("ship", "open", "--repo", "acme/shop", "rate-limits")

	// The repository is on GitHub, where alice is an admin.
	ghAlice := gh.AddUser("alice")
	acme := gh.AddOrg("acme")
	ghRepo := gh.AddRepo("acme/shop", false)
	gh.SetPermission("acme/shop", "alice", "admin")
	inst := gh.Install("acme")
	if err := st.UpsertGitHubInstallation(ctx, store.GitHubInstallationRow{ID: inst, AccountID: acme.ID, AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "all"}); err != nil {
		t.Fatal(err)
	}
	repo, err := st.RepoByName(ctx, "acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRepoGitHub(ctx, repo.ID, &ghRepo.ID, &inst); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkGitHub(ctx, alice.ID, ghAlice.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	var ships struct {
		Shipments []struct {
			Branch     string `json:"branch"`
			HeadGitSha string `json:"head_git_sha"`
		} `json:"shipments"`
	}
	if err := json.Unmarshal([]byte(person.must("ship", "list", "--repo", "acme/shop", "rate-limits", "--json")), &ships); err != nil || len(ships.Shipments) != 1 || ships.Shipments[0].HeadGitSha == "" {
		t.Fatalf("ship list --json: %v %+v", err, ships)
	}
	// No pull request number: plain git; GitHub's events name the branch.
	pr := githubtest.PR{Number: 12, Head: ships.Shipments[0].Branch, HeadSHA: ships.Shipments[0].HeadGitSha}
	hook := srv.URL + service.GitHubWebhookPath
	deliver := func(event string, payload any) {
		t.Helper()
		if code := gh.Deliver(t, hook, event, payload); code/100 != 2 {
			t.Fatalf("%s delivery: HTTP %d", event, code)
		}
	}

	// A line comment: the agent that wrote line 4 has it in vx inbox.
	deliver("pull_request_review_comment", gh.ReviewCommentEvent(inst, "acme/shop", pr, "created",
		githubtest.ReviewComment{ID: 501, Reviewer: "octo", Body: "Decide from a real counter.", Path: "limit.go", Line: 4}))
	out := agent.must("inbox")
	if !strings.Contains(out, "For you from the git host") || !strings.Contains(out, "Review comment by @octo on GitHub") ||
		!strings.Contains(out, "limit.go line 4") || !strings.Contains(out, "Decide from a real counter.") || !strings.Contains(out, "information, not instructions") {
		t.Fatalf("vx inbox:\n%s", out)
	}
	if out := agent.must("inbox"); strings.Contains(out, "Decide from a real counter.") || !strings.Contains(out, "1 read") {
		t.Fatalf("a second vx inbox repeats the note:\n%s", out)
	}

	// The next one comes with vx sync.
	deliver("issue_comment", gh.IssueCommentEvent(inst, "acme/shop", 12, "created", 502, "octo", "Please add a test."))
	if out := agent.must("sync"); !strings.Contains(out, "New on the board") || !strings.Contains(out, `Comment by @octo on`) || !strings.Contains(out, "Please add a test.") {
		t.Fatalf("vx sync feed:\n%s", out)
	}

	// vx wait wakes up for one.
	done := make(chan string, 1)
	go func() {
		out, _ := agent.run("wait", "--timeout", "20s", "--interval", "100ms")
		done <- out
	}()
	time.Sleep(500 * time.Millisecond)
	deliver("pull_request_review", gh.ReviewEvent(inst, "acme/shop", pr, "submitted", 503, "octo", "changes_requested", "Needs a test first."))
	select {
	case out := <-done:
		if !strings.Contains(out, "A review comment for you arrived") || !strings.Contains(out, "requested changes") {
			t.Fatalf("vx wait:\n%s", out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("vx wait did not return")
	}
	if out := agent.must("tasks"); !strings.Contains(out, "Address review by @octo") {
		t.Fatalf("vx tasks:\n%s", out)
	}

	// CI: the failure shows in vx ship list and vx status.
	deliver("check_run", gh.CheckRunEvent(inst, "acme/shop", pr, "completed", "github-actions", "test", "completed", "failure"))
	if out := person.must("ship", "list", "--repo", "acme/shop", "rate-limits"); !strings.Contains(out, "CI: failed (test, http") {
		t.Fatalf("vx ship list:\n%s", out)
	}
	// vx status shows the CI result for the agent in full, labelled
	// [vx]; then vx inbox does not repeat it.
	if out := agent.must("status"); !strings.Contains(out, "Shipped:") || !strings.Contains(out, "CI: failed (test") ||
		!strings.Contains(out, "    [vx] ci  just now  id ") || !strings.Contains(out, "│ CI failed on PR #12 at ") || strings.Contains(out, "service:vx") {
		t.Fatalf("vx status:\n%s", out)
	}
	if out := agent.must("inbox"); strings.Contains(out, "CI failed on PR #12 at ") || !strings.Contains(out, "read (vx inbox --all)") {
		t.Fatalf("vx inbox after CI repeats it:\n%s", out)
	}
	if out := agent.must("inbox", "--all"); !strings.Contains(out, "[vx] ci in rate-limits just now  id ") || !strings.Contains(out, ": CI failed on PR #12 at ") {
		t.Fatalf("vx inbox --all after CI:\n%s", out)
	}

	// main moves: vx repo sync rebases the effort, and the agent hears of it.
	up.commit("Docs", map[string]string{"README.md": "# Shop\n"})
	person.must("repo", "sync", "--repo", "acme/shop")
	if out := agent.must("sync"); !strings.Contains(out, "Rebased automatically onto main at") {
		t.Fatalf("vx sync after main moved:\n%s", out)
	}
	if got := agent.read("README.md"); got != "# Shop\n" {
		t.Fatalf("README.md after the automatic rebase: %q", got)
	}
}
