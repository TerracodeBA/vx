package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// recordingUpstream stands in for the git bridge: it records imports.
type recordingUpstream struct {
	mu    sync.Mutex
	syncs []string // repo name @ branch
}

func (u *recordingUpstream) DefaultBranch(ctx context.Context, repo store.Repo) (string, error) {
	return "main", nil
}

func (u *recordingUpstream) SyncBranch(ctx context.Context, repo store.Repo, branch string) (store.Branch, int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.syncs = append(u.syncs, repo.Name+"@"+branch+" from "+repo.Remote)
	return store.Branch{RepoID: repo.ID, Name: branch, Head: hash.Hash{}}, 0, nil
}

func (u *recordingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.syncs)
}

type hookEnv struct {
	*env
	gh *githubtest.Server
	up *recordingUpstream
}

func newHookEnv(t *testing.T) *hookEnv {
	e := &hookEnv{env: newEnv(t), gh: githubtest.New(t), up: &recordingUpstream{}}
	e.svc.Upstream = e.up
	e.gh.Configure(t, e.svc)
	return e
}

// deliver applies a signed delivery of payload, as GitHub sends it.
func (e *hookEnv) deliver(event string, payload any) {
	e.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.svc.HandleGitHubWebhook(e.ctx, event, githubtest.NewDeliveryID(), e.gh.Sign(body), body); err != nil {
		e.t.Fatalf("%s delivery: %v", event, err)
	}
}

func (e *hookEnv) repo(name string) store.Repo {
	e.t.Helper()
	r, err := e.svc.Store.RepoByName(e.ctx, name)
	if err != nil {
		e.t.Fatalf("repo %s: %v", name, err)
	}
	return r
}

func i64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func TestGitHubInstallationSync(t *testing.T) {
	e := newHookEnv(t)
	ctx, svc, gh := e.ctx, e.svc, e.gh
	acme := gh.AddOrg("Acme")
	shop := gh.AddRepo("Acme/Shop", true)
	site := gh.AddRepo("Acme/Site", false)
	id := gh.Install("Acme")

	e.deliver("installation", gh.InstallationEvent("created", id))
	org, err := svc.Store.OrgByHandle(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if i64(org.GitHubOrgID) != acme.ID || org.GitHubLogin != "Acme" || org.Personal {
		t.Fatalf("org %+v", org)
	}
	r := e.repo("acme/shop")
	if r.Provider != store.ProviderGitHub || r.Remote != "Acme/Shop" || r.OrgID != org.ID || i64(r.GitHubRepoID) != shop.ID || i64(r.GitHubInstallationID) != id {
		t.Fatalf("repo %+v", r)
	}
	if i64(e.repo("acme/site").GitHubRepoID) != site.ID || e.up.count() != 2 {
		t.Fatalf("site not linked or imports %v", e.up.syncs)
	}
	row, err := svc.Store.GitHubInstallationByID(ctx, id)
	if err != nil || row.OrgID == nil || *row.OrgID != org.ID || row.RepositorySelection != "all" || row.AccountType != "Organization" {
		t.Fatalf("installation row %+v %v", row, err)
	}
	// Syncing again changes nothing and imports nothing again.
	if err := svc.SyncInstallation(ctx, id); err != nil || e.up.count() != 2 {
		t.Fatalf("resync: %v, imports %v", err, e.up.syncs)
	}

	// Removed from the installation: kept, but unlinked from it.
	gh.RemoveInstallationRepos(id, "Acme/Site")
	e.deliver("installation_repositories", gh.InstallationReposEvent("removed", id, "Acme/Site"))
	if r := e.repo("acme/site"); r.GitHubInstallationID != nil || i64(r.GitHubRepoID) != site.ID {
		t.Fatalf("removed repo %+v", r)
	}
	// Added: created and imported.
	gh.AddRepo("Acme/New", true)
	gh.AddInstallationRepos(id, "Acme/New")
	e.deliver("installation_repositories", gh.InstallationReposEvent("added", id, "Acme/New"))
	if r := e.repo("acme/new"); i64(r.GitHubInstallationID) != id || e.up.count() != 3 {
		t.Fatalf("added repo %+v, imports %v", r, e.up.syncs)
	}
	// A full sync also unlinks what GitHub no longer lists.
	gh.RemoveInstallationRepos(id, "Acme/New")
	if err := svc.SyncInstallation(ctx, id); err != nil {
		t.Fatal(err)
	}
	if e.repo("acme/new").GitHubInstallationID != nil {
		t.Fatal("repo GitHub no longer lists is still linked")
	}

	// Renamed and transferred: the vx name stays, the remote follows.
	gh.RenameRepo("Acme/Shop", "Store")
	e.deliver("repository", gh.RepoRenamedEvent(id, "Acme/Store", "Shop"))
	if r := e.repo("acme/shop"); r.Remote != "Acme/Store" {
		t.Fatalf("after rename: %+v", r)
	}

	// A push to the default branch imports it; other branches do not.
	n := e.up.count()
	e.deliver("push", gh.PushEvent(id, "Acme/Store", "refs/heads/main", "a", "b"))
	if e.up.count() != n+1 || e.up.syncs[n] != "acme/shop@main from Acme/Store" {
		t.Fatalf("push imports %v", e.up.syncs)
	}
	e.deliver("push", gh.PushEvent(id, "Acme/Store", "refs/heads/topic", "a", "b"))
	e.deliver("push", gh.PushEvent(id, "Acme/Store", "refs/tags/v1", "a", "b"))
	if e.up.count() != n+1 {
		t.Fatalf("push to another branch imported: %v", e.up.syncs)
	}

	// Collaborator and visibility changes drop cached roles of the repo.
	shopRepo := e.repo("acme/shop")
	cache := func() int {
		m, err := svc.Store.GitHubRoleCache(ctx, e.alice.ID, []uuid.UUID{shopRepo.ID})
		if err != nil {
			t.Fatal(err)
		}
		return len(m)
	}
	put := func() {
		if err := svc.Store.PutGitHubRole(ctx, shopRepo.ID, e.alice.ID, "write", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		event   string
		payload map[string]any
	}{
		{"member", gh.MemberEvent(id, "added", "Acme/Store", "Acme")},
		{"repository", gh.RepositoryEvent("privatized", id, mustRepo(t, gh, "Acme/Store"))},
		{"team_add", gh.TeamAddEvent(id, "Acme", "devs", "Acme/Store")},
		{"team", gh.TeamEvent(id, "edited", "Acme", "devs", "")},
		{"organization", gh.OrgMemberEvent(id, "member_invited", "Acme", "Acme", "member")},
	} {
		put()
		e.deliver(c.event, c.payload)
		if cache() != 0 {
			t.Errorf("%s %v kept the cached role", c.event, c.payload["action"])
		}
	}

	// Deleted on GitHub: kept and unlinked.
	gone := gh.DeleteRepo("Acme/Store")
	e.deliver("repository", gh.RepositoryEvent("deleted", id, gone))
	if r := e.repo("acme/shop"); r.GitHubInstallationID != nil || i64(r.GitHubRepoID) != shop.ID {
		t.Fatalf("deleted repo %+v", r)
	}

	// Suspended, unsuspended, deleted.
	gh.SuspendInstallation(id, true)
	e.deliver("installation", gh.InstallationEvent("suspend", id))
	if row, _ := svc.Store.GitHubInstallationByID(ctx, id); row.SuspendedAt == nil {
		t.Fatal("suspension not recorded")
	}
	gh.SuspendInstallation(id, false)
	e.deliver("installation", gh.InstallationEvent("unsuspend", id))
	if row, _ := svc.Store.GitHubInstallationByID(ctx, id); row.SuspendedAt != nil {
		t.Fatal("unsuspension not recorded")
	}
	payload := gh.InstallationEvent("deleted", id)
	gh.DeleteInstallation(id)
	e.deliver("installation", payload)
	if _, err := svc.Store.GitHubInstallationByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted installation: %v", err)
	}
	if r := e.repo("acme/site"); i64(r.GitHubRepoID) != site.ID {
		t.Fatal("repos lost their GitHub link")
	}
}

func mustRepo(t *testing.T, gh *githubtest.Server, full string) githubtest.Repo {
	r, ok := gh.Repo(full)
	if !ok {
		t.Fatalf("no repo %s", full)
	}
	return r
}

// An organization made in vx before the App came is linked, not
// duplicated; its existing repo for the same GitHub repository is linked.
func TestGitHubInstallationLinksExistingOrg(t *testing.T) {
	e := newHookEnv(t)
	ctx, svc, gh := e.ctx, e.svc, e.gh
	if _, err := svc.CreateOrg(ctx, e.alice, "beta", "Beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.CreateRepo(ctx, store.Repo{Name: "beta/api", Provider: store.ProviderGitHub, Remote: "Beta/API", OrgID: mustOrg(t, svc, "beta").ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.CreateRepo(ctx, store.Repo{Name: "beta/web", Provider: store.ProviderNone, OrgID: mustOrg(t, svc, "beta").ID}); err != nil {
		t.Fatal(err)
	}
	beta := gh.AddOrg("Beta")
	api := gh.AddRepo("Beta/API", true)
	gh.AddRepo("Beta/web", true)
	id := gh.Install("Beta")
	err := svc.SyncInstallation(ctx, id)
	// beta/web exists in vx without GitHub: refused for that repo only.
	if code(err) != service.CodeExists {
		t.Fatalf("sync: %v", err)
	}
	if o := mustOrg(t, svc, "beta"); i64(o.GitHubOrgID) != beta.ID {
		t.Fatalf("existing org not linked: %+v", o)
	}
	if r := e.repo("beta/api"); i64(r.GitHubRepoID) != api.ID || e.up.count() != 0 {
		t.Fatalf("existing repo %+v, imports %v", r, e.up.syncs)
	}
	if r := e.repo("beta/web"); r.GitHubRepoID != nil {
		t.Fatal("unrelated repo linked")
	}
}

func mustOrg(t *testing.T, svc *service.Service, handle string) store.Org {
	t.Helper()
	o, err := svc.Store.OrgByHandle(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// A personal account's installation waits for its person; their repos
// come into their personal organization when they sign in.
func TestGitHubUserInstallation(t *testing.T) {
	e := newHookEnv(t)
	ctx, svc, gh := e.ctx, e.svc, e.gh
	carolGH := gh.AddUser("Carol-GH")
	gh.AddRepo("Carol-GH/dots", false)
	id := gh.Install("Carol-GH")
	e.deliver("installation", gh.InstallationEvent("created", id))
	row, err := svc.Store.GitHubInstallationByID(ctx, id)
	if err != nil || row.OrgID != nil || row.AccountType != "User" {
		t.Fatalf("row %+v %v", row, err)
	}
	carol := e.person("carol")
	if err := svc.Store.LinkGitHub(ctx, carol.ID, carolGH.ID, "Carol-GH"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncGitHubAccount(ctx, carolGH.ID); err != nil {
		t.Fatal(err)
	}
	r := e.repo("carol/dots")
	if r.Org != "carol" || r.Remote != "Carol-GH/dots" || i64(r.GitHubInstallationID) != id {
		t.Fatalf("repo %+v", r)
	}
}

// Organization membership webhooks reach the linked vx organization.
func TestGitHubOrgMembershipWebhook(t *testing.T) {
	e := newHookEnv(t)
	ctx, svc, gh := e.ctx, e.svc, e.gh
	gh.AddOrg("acme")
	dan := gh.AddUser("dan")
	id := gh.Install("acme")
	e.deliver("installation", gh.InstallationEvent("created", id))
	p := e.person("dan")
	if err := svc.Store.LinkGitHub(ctx, p.ID, dan.ID, "dan"); err != nil {
		t.Fatal(err)
	}
	org := mustOrg(t, svc, "acme")
	gh.SetOrgMember("acme", "dan", "member")
	e.deliver("organization", gh.OrgMemberEvent(id, "member_added", "acme", "dan", "member"))
	if role, _ := svc.Store.OrgRole(ctx, org.ID, p.ID); role != "member" {
		t.Fatalf("after member_added: %q", role)
	}
	e.deliver("organization", gh.OrgMemberEvent(id, "member_removed", "acme", "dan", "member"))
	if role, _ := svc.Store.OrgRole(ctx, org.ID, p.ID); role != "" {
		t.Fatalf("after member_removed: %q", role)
	}
}
