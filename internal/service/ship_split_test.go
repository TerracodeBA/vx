package service_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	gitobject "github.com/go-git/go-git/v5/plumbing/object"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// splitPR is a pull request on the fake GitHub.
type splitPR struct {
	Number                  int
	Head, Base, Title, Body string
	Draft                   bool
}

// splitGitHub is a GitHub API fake for pull requests: create (422 when
// one is open for the head), find by head, update title, body and base.
type splitGitHub struct {
	mu               sync.Mutex
	prs              map[int]*splitPR
	created, updated int
}

func (f *splitGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer gh-test-token" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	byHead := func(head string) *splitPR {
		for _, p := range f.prs {
			if p.Head == head {
				return p
			}
		}
		return nil
	}
	link := func(n int) string { return "https://github.com/acme/shop/pull/" + strconv.Itoa(n) }
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/shop/pulls":
		if byHead(str("head")) != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for acme:`+str("head")+`."}]}`)
			return
		}
		f.created++
		n := 40 + f.created
		draft, _ := in["draft"].(bool)
		f.prs[n] = &splitPR{Number: n, Head: str("head"), Base: str("base"), Title: str("title"), Body: str("body"), Draft: draft}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": n, "html_url": link(n)})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/shop/pulls":
		var out []map[string]any
		if p := byHead(strings.TrimPrefix(r.URL.Query().Get("head"), "acme:")); p != nil {
			out = append(out, map[string]any{"number": p.Number, "html_url": link(p.Number)})
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/shop/pulls/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/acme/shop/pulls/"))
		p := f.prs[n]
		if p == nil {
			http.NotFound(w, r)
			return
		}
		f.updated++
		p.Title, p.Body, p.Base = str("title"), str("body"), str("base")
		json.NewEncoder(w).Encode(map[string]any{"number": n})
	default:
		http.NotFound(w, r)
	}
}

func (f *splitGitHub) pr(n int) splitPR {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.prs[n]; p != nil {
		return *p
	}
	return splitPR{}
}

// splitEnv is a repository acme/shop on a local git upstream that stands
// in for GitHub, with the pull request fake, and an effort to split.
type splitEnv struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	svc    *service.Service
	blobs  blob.Store
	gh     *splitGitHub
	git    *git.Repository
	wt     *git.Worktree
	dir    string
	human  service.Actor
	agent  service.Actor
	effort store.Effort
	n      int
}

func newSplitEnv(t *testing.T, files map[string]string) *splitEnv {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	blobs := blob.NewMem()
	e := &splitEnv{t: t, ctx: ctx, st: st, blobs: blobs, gh: &splitGitHub{prs: map[int]*splitPR{}}}
	e.dir = t.TempDir()
	r, err := git.PlainInitWithOptions(e.dir, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")}})
	if err != nil {
		t.Fatal(err)
	}
	e.git = r
	e.wt, _ = r.Worktree()
	e.commit("Initial", files)
	gitBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gitBase, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.dir, filepath.Join(gitBase, "acme", "shop.git")); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(e.gh)
	t.Cleanup(api.Close)
	br := gitbridge.New(st, blobs, t.TempDir(), func() string { return "gh-test-token" })
	br.GitHubAPI = api.URL
	br.GitHubGitBase = gitBase
	e.svc = service.New(st, blobs, br)
	alice, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice Doe", Email: "alice@example.com"})
	ag, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/backend", ParentID: &alice.ID})
	e.human, _ = e.svc.ResolveActor(ctx, alice)
	e.agent, _ = e.svc.ResolveActor(ctx, ag)
	if _, err := e.svc.CreateRepo(ctx, e.human, service.CreateRepoInput{Name: "acme/shop", Provider: store.ProviderGitHub, Remote: "acme/shop"}); err != nil {
		t.Fatal(err)
	}
	e.effort, err = e.svc.CreateEffort(ctx, e.human, service.CreateEffortInput{Repo: "acme/shop", Name: "Rate limiting", Goal: "Limit login attempts."})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// commit writes files on main of the upstream ("" deletes).
func (e *splitEnv) commit(msg string, files map[string]string) plumbing.Hash {
	e.t.Helper()
	for p, c := range files {
		full := filepath.Join(e.dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			e.t.Fatal(err)
		}
		if _, err := e.wt.Add(p); err != nil {
			e.t.Fatal(err)
		}
	}
	e.n++
	h, err := e.wt.Commit(msg, &git.CommitOptions{Author: &gitobject.Signature{Name: "Bob", Email: "bob@example.com", When: time.Date(2026, 11, 1, 10, e.n, 0, 0, time.UTC)}})
	if err != nil {
		e.t.Fatal(err)
	}
	return h
}

// land lands one change into the effort and returns its change id.
func (e *splitEnv) land(intent string, files map[string]string, deps ...string) string {
	e.t.Helper()
	ws, err := e.svc.OpenWorkspace(e.ctx, e.agent, "acme/shop", e.effort.Slug, intent)
	if err != nil {
		e.t.Fatal(err)
	}
	row, _ := e.st.GetChangeset(e.ctx, ws.Repo.ID, ws.Workspace.Base)
	var changes []object.Change
	for p, c := range files {
		h, _ := e.blobs.Put(e.ctx, hash.KindBlob, []byte(c))
		changes = append(changes, object.Change{Path: p, Op: object.OpModify, New: object.File{Hash: h, Mode: object.ModeFile}})
	}
	tree, err := object.ApplyChanges(e.ctx, e.blobs, row.Tree, changes)
	if err != nil {
		e.t.Fatal(err)
	}
	storetest.AddTree(e.t, e.st, e.blobs, ws.Repo.ID, row.Tree, tree)
	res, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree, DependsOn: deps,
		Evidence: &object.Evidence{Checks: []object.Check{{Name: "go test ./...", Result: "pass", RanByVX: true}}}})
	if err != nil || !res.Landed {
		e.t.Fatalf("land %q: %+v %v", intent, res, err)
	}
	return res.Changeset.ChangeID
}

func (e *splitEnv) plan(mode service.SplitMode, groups ...[]string) service.ShipPlan {
	e.t.Helper()
	p, err := e.svc.PlanSplit(e.ctx, e.human, service.SplitInput{Repo: "acme/shop", Effort: e.effort.Slug, Mode: mode, Groups: groups})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// branch returns the head commit of an upstream branch.
func (e *splitEnv) branch(name string) *gitobject.Commit {
	e.t.Helper()
	ref, err := e.git.Reference(plumbing.NewBranchReferenceName(name), true)
	if err != nil {
		e.t.Fatalf("branch %s: %v", name, err)
	}
	c, err := e.git.CommitObject(ref.Hash())
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *splitEnv) file(c *gitobject.Commit, path string) string {
	e.t.Helper()
	tree, _ := c.Tree()
	f, err := tree.File(path)
	if err != nil {
		return ""
	}
	s, _ := f.Contents()
	return s
}

func groupIDs(g service.ShipGroup) []string {
	var out []string
	for _, st := range g.Steps {
		out = append(out, st.Changeset.ChangeID)
	}
	return out
}

func sameIDs(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// Independent: changes sharing a file, or tied by depends_on (a symbol
// another change edits, or its change id), share a part; the rest ship
// apart, each replayed onto main on its own.
func TestSplitIndependentGroupsByFilesAndDependsOn(t *testing.T) {
	e := newSplitEnv(t, map[string]string{
		"auth.go":   "package auth\n\nfunc Login() {\n}\n\nfunc Logout() {\n}\n",
		"README.md": "# shop\n",
	})
	a := e.land("Add limiter", map[string]string{"limit.go": "package auth\n\nfunc limit() {}\n"})
	b := e.land("Document the limiter", map[string]string{"README.md": "# shop\n\nLogins are limited.\n"})
	c := e.land("Wire limiter into Login", map[string]string{
		"auth.go":  "package auth\n\nfunc Login() {\n\tlimit()\n}\n\nfunc Logout() {\n}\n",
		"limit.go": "package auth\n\nfunc limit() { /* per IP */ }\n",
	})
	d := e.land("Count limited logins", map[string]string{"metrics.go": "package auth\n\nvar limited int\n"}, "auth.Login")
	f := e.land("Add changelog", map[string]string{"CHANGELOG.md": "- limiter\n"}, b)
	g := e.land("Log logouts", map[string]string{"logout.go": "package auth\n\nfunc logLogout() {}\n"}, "auth.Logout")

	p := e.plan(service.SplitIndependent)
	if len(p.Problems) > 0 || !p.Suggested || len(p.Groups) != 3 {
		t.Fatalf("plan: problems %v, %d groups", p.Problems, len(p.Groups))
	}
	sameIDs(t, "part 1", groupIDs(p.Groups[0]), []string{a, c, d})
	sameIDs(t, "part 2", groupIDs(p.Groups[1]), []string{b, f})
	// auth.Logout is not changed by any change: g stands alone.
	sameIDs(t, "part 3", groupIDs(p.Groups[2]), []string{g})
	why := strings.Join(p.Groups[0].Why, "\n")
	for _, want := range []string{a + " and " + c + " both change limit.go", d + " depends on auth.Login, which " + c + " changes"} {
		if !strings.Contains(why, want) {
			t.Errorf("part 1 why lacks %q:\n%s", want, why)
		}
	}
	if !strings.Contains(strings.Join(p.Groups[1].Why, "\n"), f+" depends on "+b) {
		t.Errorf("part 2 why: %v", p.Groups[1].Why)
	}
	for i, want := range []string{"vx/rate-limiting/1-add-limiter", "vx/rate-limiting/2-document-the-limiter", "vx/rate-limiting/3-log-logouts"} {
		if g := p.Groups[i]; g.Branch != want || g.Base != "main" {
			t.Errorf("part %d: branch %s base %s, want %s onto main", i+1, g.Branch, g.Base, want)
		}
	}
	if p.Groups[0].Title != "Add limiter (+2 more)" || strings.Join(p.Groups[1].Files, ",") != "CHANGELOG.md,README.md" {
		t.Errorf("part titles and files: %q %v", p.Groups[0].Title, p.Groups[1].Files)
	}

	// Ship: three branches, each straight on main, three pull requests
	// that list each other.
	main := e.branch("main").Hash
	res, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitIndependent})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Parts) != 3 || e.gh.created != 3 {
		t.Fatalf("parts %d, created %d", len(res.Parts), e.gh.created)
	}
	for i, part := range res.Parts {
		n := 41 + i
		if part.PR.Number != n || part.Shipment.Part != i+1 || part.Shipment.Split != "independent" || part.Shipment.Target != "main" {
			t.Fatalf("part %d: pr %+v shipment %+v", i+1, part.PR, part.Shipment)
		}
		pr := e.gh.pr(n)
		if pr.Base != "main" || pr.Draft {
			t.Fatalf("pr %d base %s", n, pr.Base)
		}
		head := e.branch(part.Shipment.Branch)
		// Walk down to main: every commit of the part, nothing else.
		k := len(part.Shipment.Changes)
		cur := head
		for j := 0; j < k; j++ {
			if !strings.Contains(cur.Message, "Vx-Change-Id: "+part.Shipment.Changes[k-1-j].ChangeID) {
				t.Fatalf("part %d commit %d: %s", i+1, j, cur.Message)
			}
			cur, _ = cur.Parent(0)
		}
		if cur.Hash != main {
			t.Fatalf("part %d does not sit on main", i+1)
		}
	}
	body := e.gh.pr(42).Body
	for _, want := range []string{"**Part 2 of 3** of vx effort `rate-limiting`. The parts are independent: they merge in any order.", "1. #41 Add limiter (+2 more)", "2. Document the limiter (+1 more) (this pull request)", "3. #43 Log logouts", "Document the limiter (`" + b + "`", "## Evidence"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR #42 body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Wire limiter") {
		t.Errorf("PR #42 lists another part's change:\n%s", body)
	}
	// The README part carries nothing of the limiter.
	if got := e.file(e.branch("vx/rate-limiting/2-document-the-limiter"), "limit.go"); got != "" {
		t.Fatalf("part 2 has limit.go: %q", got)
	}
	if got := e.file(e.branch("vx/rate-limiting/1-add-limiter"), "auth.go"); !strings.Contains(got, "limit()") {
		t.Fatalf("part 1 auth.go: %q", got)
	}

	// Another change joins part 3 (it touches logout.go): shipping again
	// updates the same three branches and pull requests.
	h := e.land("Log logouts with the user", map[string]string{"logout.go": "package auth\n\nfunc logLogout(user string) {}\n"})
	p = e.plan(service.SplitIndependent)
	if len(p.Groups) != 3 || p.Groups[2].Previous == nil || p.Groups[2].Previous.ProviderRef != "43" {
		t.Fatalf("replan: %d groups, part 3 previous %+v", len(p.Groups), p.Groups[2].Previous)
	}
	sameIDs(t, "part 3 again", groupIDs(p.Groups[2]), []string{g, h})
	res, err = e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitIndependent})
	if err != nil {
		t.Fatal(err)
	}
	if e.gh.created != 3 || !res.Parts[2].PR.Updated || res.Parts[2].PR.Number != 43 || len(res.Parts[2].Shipment.Changes) != 2 {
		t.Fatalf("re-ship: created %d, part 3 %+v", e.gh.created, res.Parts[2])
	}
	if !strings.HasPrefix(e.branch("vx/rate-limiting/3-log-logouts").Message, "Log logouts with the user") {
		t.Fatal("part 3 branch was not updated")
	}
	list, _ := e.svc.ListShipments(e.ctx, e.human, "acme/shop", e.effort.Slug)
	if len(list) != 3 {
		t.Fatalf("shipments: %d", len(list))
	}

	// Part 2 is merged on GitHub (its commits keep their trailers): that
	// part alone becomes merged, the effort stays open.
	merge := func(part service.ShipPart) {
		t.Helper()
		files := map[string]string{}
		msg := part.Group.Title + "\n\n"
		for _, ch := range part.Shipment.Changes {
			msg += "Vx-Change-Id: " + ch.ChangeID + "\n"
		}
		head := e.branch(part.Shipment.Branch)
		tree, _ := head.Tree()
		for _, p := range part.Group.Files {
			f, err := tree.File(p)
			if err != nil {
				t.Fatal(err)
			}
			files[p], _ = f.Contents()
		}
		e.commit(msg, files)
		if _, _, err := e.svc.SyncBranch(e.ctx, e.human, "acme/shop", "main"); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (string, []string) {
		t.Helper()
		v, err := e.svc.GetEffort(e.ctx, e.human, "acme/shop", e.effort.Slug)
		if err != nil {
			t.Fatal(err)
		}
		list, _ := e.svc.ListShipments(e.ctx, e.human, "acme/shop", e.effort.Slug)
		var states []string
		for _, s := range list {
			states = append(states, s.State)
		}
		return string(v.Effort.State), states
	}
	merge(res.Parts[1])
	if eff, states := state(); eff != "open" || strings.Join(states, ",") != "open,merged,open" {
		t.Fatalf("after merging part 2: effort %s, shipments %v", eff, states)
	}
	// The next plan leaves part 2's changes out: main has them.
	p = e.plan(service.SplitIndependent)
	if len(p.Groups) != 2 || len(p.Notes) != 2 {
		t.Fatalf("plan after part 2 merged: %d groups, notes %v", len(p.Groups), p.Notes)
	}
	merge(res.Parts[0])
	merge(res.Parts[2])
	if eff, states := state(); eff != "merged" || strings.Join(states, ",") != "merged,merged,merged" {
		t.Fatalf("after merging every part: effort %s, shipments %v", eff, states)
	}
}

// Stacked: one part per change by default, each based on the branch of
// the part before; bodies say what each depends on.
func TestSplitStackedBasesAndBodies(t *testing.T) {
	e := newSplitEnv(t, map[string]string{"README.md": "# shop\n"})
	a := e.land("Add limiter", map[string]string{"limit.go": "package auth\n\nfunc limit() {}\n"})
	b := e.land("Limit per IP", map[string]string{"limit.go": "package auth\n\nfunc limit(ip string) {}\n"})
	c := e.land("Document the limiter", map[string]string{"README.md": "# shop\n\nLimited.\n"})

	p := e.plan(service.SplitStacked)
	if len(p.Problems) > 0 || len(p.Groups) != 3 {
		t.Fatalf("plan: %v, %d groups", p.Problems, len(p.Groups))
	}
	for i, want := range [][]string{{a}, {b}, {c}} {
		sameIDs(t, "stacked part", groupIDs(p.Groups[i]), want)
	}
	if p.Groups[0].Base != "main" || p.Groups[1].Base != p.Groups[0].Branch || p.Groups[2].Base != p.Groups[1].Branch {
		t.Fatalf("bases: %s %s %s", p.Groups[0].Base, p.Groups[1].Base, p.Groups[2].Base)
	}
	// Part 2 shows only its own change on top of part 1.
	if strings.Join(p.Groups[1].Files, ",") != "limit.go" || len(p.Groups[1].Commits()) != 1 {
		t.Fatalf("part 2 files %v", p.Groups[1].Files)
	}

	res, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitStacked, Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	main := e.branch("main").Hash
	b1, b2, b3 := e.branch(p.Groups[0].Branch), e.branch(p.Groups[1].Branch), e.branch(p.Groups[2].Branch)
	if b1.ParentHashes[0] != main || b2.ParentHashes[0] != b1.Hash || b3.ParentHashes[0] != b2.Hash {
		t.Fatal("stacked branches do not build on each other")
	}
	if e.file(b3, "limit.go") != "package auth\n\nfunc limit(ip string) {}\n" {
		t.Fatal("top of the stack lacks the lower parts")
	}
	if pr := e.gh.pr(42); pr.Base != p.Groups[0].Branch || !pr.Draft {
		t.Fatalf("PR #42 base %s draft %v", pr.Base, pr.Draft)
	}
	if pr := e.gh.pr(43); pr.Base != p.Groups[1].Branch {
		t.Fatalf("PR #43 base %s", pr.Base)
	}
	if res.Parts[1].Shipment.Target != p.Groups[0].Branch || res.Parts[1].Shipment.State != "draft" || res.Parts[1].Shipment.BaseGitSHA != b1.Hash.String() {
		t.Fatalf("part 2 shipment %+v", res.Parts[1].Shipment)
	}
	for n, wants := range map[int][]string{
		41: {"**Part 1 of 3** of vx effort `rate-limiting`, stacked: merge it first", "2. #42 Limit per IP", "3. #43 Document the limiter"},
		42: {"**Part 2 of 3** of vx effort `rate-limiting`, stacked: depends on #41.", "1. #41 Add limiter", "2. Limit per IP (this pull request)"},
		43: {"**Part 3 of 3**", "depends on #42"},
	} {
		body := e.gh.pr(n).Body
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("PR #%d body lacks %q:\n%s", n, w, body)
			}
		}
	}
	// Bodies were filled in with numbers after the PRs existed: #41 and
	// #42 needed one update each.
	if e.gh.created != 3 || e.gh.updated != 2 {
		t.Fatalf("created %d, updated %d", e.gh.created, e.gh.updated)
	}

	// Re-ship: same branches and PRs, nothing new created.
	before := e.gh.updated
	if _, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitStacked}); err != nil {
		t.Fatal(err)
	}
	if e.gh.created != 3 || e.gh.updated != before+3 {
		t.Fatalf("re-ship: created %d, updated %d", e.gh.created, e.gh.updated-before)
	}

	// A person's stack that puts "Limit per IP" below "Add limiter" is
	// refused: it builds on that change's lines.
	p = e.plan(service.SplitStacked, []string{b}, []string{a}, []string{c})
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "builds on lines that change "+a+" (Add limiter) wrote in limit.go, which is in part 2 above it") {
		t.Fatalf("problems: %v", p.Problems)
	}
	if _, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitStacked, Groups: [][]string{{b}, {a}, {c}}}); err == nil || !strings.Contains(err.Error(), "this split cannot ship") {
		t.Fatalf("open the refused stack: %v", err)
	}
	// Prefixes work, and two changes may share a part.
	p = e.plan(service.SplitStacked, []string{a[:5], b}, []string{c})
	if len(p.Problems) > 0 || len(p.Groups) != 2 || p.Groups[0].Previous == nil || p.Groups[0].Previous.ProviderRef != "41" {
		t.Fatalf("two-part stack: %v %d", p.Problems, len(p.Groups))
	}
	if len(p.Stale) != 1 || p.Stale[0].ProviderRef != "42" || p.Groups[1].Previous == nil || p.Groups[1].Previous.ProviderRef != "43" {
		t.Fatalf("stale: %+v", p.Stale)
	}
	// Switching to independent parts reuses the branch both modes name
	// alike (and its pull request); the others are left for a person.
	p = e.plan(service.SplitIndependent)
	if len(p.Groups) != 2 || p.Groups[0].Previous == nil || p.Groups[0].Previous.ProviderRef != "41" || p.Groups[1].Previous != nil || len(p.Stale) != 2 {
		t.Fatalf("independent after stacked: %d groups, stale %d", len(p.Groups), len(p.Stale))
	}
}

// A person's independent grouping is checked: parts that need each other's
// lines, or depend on each other, are refused with the reason; changes in
// one file that touch lines far apart may ship apart.
func TestSplitValidationLineDependencies(t *testing.T) {
	long := ""
	for i := 0; i < 30; i++ {
		long += "line " + strconv.Itoa(i) + "\n"
	}
	e := newSplitEnv(t, map[string]string{"notes.txt": long, "README.md": "# shop\n"})
	top := e.land("Edit the top", map[string]string{"notes.txt": strings.Replace(long, "line 1\n", "line one\n", 1)})
	cur := strings.Replace(long, "line 1\n", "line one\n", 1)
	bottom := e.land("Edit the bottom", map[string]string{"notes.txt": strings.Replace(cur, "line 28\n", "line twenty-eight\n", 1)})
	cur = strings.Replace(cur, "line 28\n", "line twenty-eight\n", 1)
	onTop := e.land("Build on the top", map[string]string{"notes.txt": strings.Replace(cur, "line one\n", "line one, edited\n", 1)})
	dep := e.land("Readme for the top", map[string]string{"README.md": "# shop\n\nSee notes.\n"}, top)

	// vx keeps one file's changes together.
	p := e.plan(service.SplitIndependent)
	if len(p.Groups) != 1 {
		t.Fatalf("suggested: %d groups", len(p.Groups))
	}
	// Far-apart lines may ship apart, in either order.
	p = e.plan(service.SplitIndependent, []string{top, onTop, dep}, []string{bottom})
	if len(p.Problems) > 0 || len(p.Groups) != 2 {
		t.Fatalf("top/bottom split: %v", p.Problems)
	}
	// A change that edits lines another part wrote cannot.
	p = e.plan(service.SplitIndependent, []string{top, bottom, dep}, []string{onTop})
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "change "+onTop+" (Build on the top) builds on lines that change "+top+" (Edit the top) wrote in notes.txt, which is in part 1: put them in one part") {
		t.Fatalf("line dependency: %v", p.Problems)
	}
	// depends_on across independent parts is refused too.
	p = e.plan(service.SplitIndependent, []string{top, bottom, onTop}, []string{dep})
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "change "+dep+" (Readme for the top) depends on "+top+", which change "+top+" (Edit the top) in part 1 changes: independent parts must not depend on each other") {
		t.Fatalf("depends_on: %v", p.Problems)
	}
	// Bad groupings say what is wrong.
	for groups, want := range map[string][][]string{
		"is in no part: add it to one":                      {{top, bottom, onTop}},
		"is in part 1 and part 2":                           {{top, bottom, onTop, dep}, {dep}},
		"which is not a change of this effort left to ship": {{top, bottom, onTop, dep, "zzzz-zzzz"}},
	} {
		_, err := e.svc.PlanSplit(e.ctx, e.human, service.SplitInput{Repo: "acme/shop", Effort: e.effort.Slug, Mode: service.SplitIndependent, Groups: want})
		if err == nil || !strings.Contains(err.Error(), groups) {
			t.Errorf("grouping %v: %v", want, err)
		}
	}
}

// An effort shipped as one pull request cannot also ship as parts (git
// cannot hold vx/<slug> next to vx/<slug>/1-...), and the other way round.
func TestSplitAndSinglePullRequestExcludeEachOther(t *testing.T) {
	e := newSplitEnv(t, map[string]string{"README.md": "# shop\n"})
	e.land("Add limiter", map[string]string{"limit.go": "package auth\n"})
	e.land("Document it", map[string]string{"README.md": "# shop\n\nLimited.\n"})
	if _, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug}); err != nil {
		t.Fatal(err)
	}
	p := e.plan(service.SplitIndependent)
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "the effort is shipped as one pull request (#41 from vx/rate-limiting)") {
		t.Fatalf("problems: %v", p.Problems)
	}
	_, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitIndependent, Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "every part gets its own") {
		t.Fatalf("title with a split: %v", err)
	}

	f := newSplitEnv(t, map[string]string{"README.md": "# shop\n"})
	f.land("Add limiter", map[string]string{"limit.go": "package auth\n"})
	f.land("Document it", map[string]string{"README.md": "# shop\n\nLimited.\n"})
	if _, err := f.svc.OpenShipment(f.ctx, f.human, service.ShipInput{Repo: "acme/shop", Effort: f.effort.Slug, Split: service.SplitStacked}); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.OpenShipment(f.ctx, f.human, service.ShipInput{Repo: "acme/shop", Effort: f.effort.Slug})
	if err == nil || !strings.Contains(err.Error(), "is shipped as 2 pull requests") || !strings.Contains(err.Error(), "vx ship open --split stacked") {
		t.Fatalf("one PR over a split: %v", err)
	}
}

// A squash merge loses the trailers: the merged pull request's webhook
// marks that one part merged, and the effort once every part is.
func TestSplitSquashMergedPartByHead(t *testing.T) {
	e := newSplitEnv(t, map[string]string{"README.md": "# shop\n"})
	e.land("Add limiter", map[string]string{"limit.go": "package auth\n"})
	e.land("Document it", map[string]string{"README.md": "# shop\n\nLimited.\n"})
	res, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitIndependent})
	if err != nil || len(res.Parts) != 2 {
		t.Fatalf("%v %d", err, len(res.Parts))
	}
	repo, _ := e.st.RepoByName(e.ctx, "acme/shop")
	closed, err := e.svc.MarkShipmentMergedByHead(e.ctx, repo, res.Parts[0].Shipment.Branch)
	if err != nil || closed {
		t.Fatalf("first part: closed %v, %v", closed, err)
	}
	list, _ := e.svc.ListShipments(e.ctx, e.human, "acme/shop", e.effort.Slug)
	if list[0].State != "merged" || list[1].State != "open" {
		t.Fatalf("states %s %s", list[0].State, list[1].State)
	}
	closed, err = e.svc.MarkShipmentMergedByHead(e.ctx, repo, res.Parts[1].Shipment.Branch)
	if err != nil || !closed {
		t.Fatalf("second part: closed %v, %v", closed, err)
	}
	v, _ := e.svc.GetEffort(e.ctx, e.human, "acme/shop", e.effort.Slug)
	if v.Effort.State != store.EffortMerged {
		t.Fatalf("effort %s", v.Effort.State)
	}
}

// The Prepare pull requests page: vx's grouping with changes, files and
// evidence; a plain form moves changes, merges and splits parts, shows
// what is wrong, and opens the pull requests. Posts need this site's
// origin.
func TestSplitPreparePullRequestsPage(t *testing.T) {
	e := newSplitEnv(t, map[string]string{"README.md": "# shop\n"})
	a := e.land("Add limiter", map[string]string{"limit.go": "package auth\n\nfunc limit() {}\n"})
	b := e.land("Document the limiter", map[string]string{"README.md": "# shop\n\nLimited.\n"})
	c := e.land("Limit per IP", map[string]string{"limit.go": "package auth\n\nfunc limit(ip string) {}\n"})

	mux := http.NewServeMux()
	if err := web.Mount(mux, e.svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	token, _ := e.st.CreateToken(e.ctx, e.human.ID, "web")
	post := func(path string, form url.Values, origin bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin {
			req.Header.Set("Origin", srv.URL)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := post("/ui/login", url.Values{"token": {token}}, true); code != 200 {
		t.Fatalf("sign in: %d", code)
	}
	get := func(path string) string {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d\n%s", path, resp.StatusCode, body)
		}
		return string(body)
	}
	contains := func(what, page string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(page, w) {
				t.Errorf("%s lacks %q", what, w)
			}
		}
	}

	page := get("/ui/ship?repo=acme%2Fshop&slug=" + e.effort.Slug + "&split=independent")
	contains("suggested page", page, "Independent pull requests", "Grouping suggested by vx", "Part 1: Add limiter (&#43;1 more)", "Part 2: Document the limiter",
		"vx/rate-limiting/1-add-limiter → main", `name="part.`+a+`"`, `name="change" value="`+c+`"`, `<span class="mono">go test ./...</span>`,
		"Together because "+a+" and "+c+" both change limit.go", `<span class="mono">README.md</span>`, `value="merge:2"`, `value="split:1"`, "Open 2 pull requests")
	base := url.Values{"repo": {"acme/shop"}, "slug": {e.effort.Slug}, "split": {"independent"}, "to": {"main"},
		"change": {a, c, b}, "part." + a: {"1"}, "part." + c: {"1"}, "part." + b: {"2"}}
	with := func(kv ...string) url.Values {
		f := url.Values{}
		for k, v := range base {
			f[k] = append([]string(nil), v...)
		}
		for i := 0; i+1 < len(kv); i += 2 {
			f.Set(kv[i], kv[i+1])
		}
		return f
	}

	// Without this site's origin the form is refused.
	if code, _ := post("/ui/ship/split", with("op", "check"), false); code != http.StatusForbidden {
		t.Fatalf("cross-origin post: %d", code)
	}
	// Move "Limit per IP" into its own part: it builds on part 1's lines.
	code, page := post("/ui/ship/split", with("op", "check", "part."+c, "3"), true)
	if code != 200 {
		t.Fatalf("check: %d", code)
	}
	contains("refused grouping", page, "Your grouping", "This grouping cannot ship", "builds on lines that change "+a+" (Add limiter) wrote in limit.go", "Open 3 pull requests</button>")
	if !strings.Contains(page, `value="open" disabled`) {
		t.Error("the open button should be disabled for a refused grouping")
	}
	// Merge part 2 into part 1: one pull request.
	_, page = post("/ui/ship/split", with("op", "merge:2"), true)
	contains("merged parts", page, "Open 1 pull request</button>", "Part 1: Add limiter (&#43;2 more)")
	// Split part 1 of the stacked view: one part per change, in order.
	stacked := with("op", "split:1", "split", "stacked")
	_, page = post("/ui/ship/split", stacked, true)
	contains("split parts", page, "Stacked pull requests", "Part 1: Add limiter", "Part 2: Limit per IP", "Part 3: Document the limiter", "vx/rate-limiting/2-limit-per-ip → vx/rate-limiting/1-add-limiter", "Move up")
	// A valid check says so.
	_, page = post("/ui/ship/split", with("op", "check"), true)
	contains("valid grouping", page, "This grouping ships: 2 pull requests, every part replays cleanly and they merge in any order.")

	// Open: two pull requests, then the page links them.
	noRedirect := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", srv.URL+"/ui/ship/split", strings.NewReader(with("op", "open", "draft", "on").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || e.gh.created != 2 || !e.gh.pr(41).Draft {
		t.Fatalf("open: %d, created %d", resp.StatusCode, e.gh.created)
	}
	page = get(resp.Header.Get("Location"))
	contains("after opening", page, "Pushed every part.", `href="https://github.com/acme/shop/pull/41"`, "#42", "updates pull request #41")
}

// A part whose replay needed the merge by declaration says so, like a
// whole-effort ship plan does.
func TestSplitPartCarriesMergeWarnings(t *testing.T) {
	e := newSplitEnv(t, map[string]string{"shop/x.go": "package shop\n\nfunc A() int { return 1 }\nfunc B() int { return 2 }\n"})
	e.land("Change A", map[string]string{"shop/x.go": "package shop\n\nfunc A() int { return 10 }\nfunc B() int { return 2 }\n"})
	e.land("Add notes", map[string]string{"NOTES.md": "notes\n"})
	e.commit("Change B upstream", map[string]string{"shop/x.go": "package shop\n\nfunc A() int { return 1 }\nfunc B() int { return 20 }\n"})
	plan := e.plan(service.SplitIndependent)
	if len(plan.Groups) != 2 {
		t.Fatalf("parts = %d, want 2", len(plan.Groups))
	}
	var found bool
	for _, g := range plan.Groups {
		for _, w := range g.Warnings {
			if w.Kind == engine.WarningMergedByDecl && strings.Contains(w.Detail, "shop/x.go") {
				found = true
			}
		}
		if len(g.Conflicts) > 0 {
			t.Fatalf("part %q conflicts: %v", g.Name, g.Conflicts)
		}
	}
	if !found {
		t.Fatalf("no part says shop/x.go was merged by declaration: %+v", plan.Groups)
	}
}
