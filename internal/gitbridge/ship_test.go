package gitbridge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// fakeGitHub records pull request calls.
type fakeGitHub struct {
	mu      sync.Mutex
	open    map[string]int // head branch -> PR number
	bodies  map[int]string
	created int
	updated int
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer gh-test-token" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/shop/pulls":
		head := in["head"].(string)
		if _, ok := f.open[head]; ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for acme:`+head+`."}]}`)
			return
		}
		f.created++
		n := 40 + f.created
		f.open[head] = n
		f.bodies[n] = in["body"].(string)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"number": n, "html_url": "https://github.com/acme/shop/pull/" + itoa(n)})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/shop/pulls":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "acme:")
		var out []map[string]any
		if n, ok := f.open[head]; ok {
			out = append(out, map[string]any{"number": n, "html_url": "https://github.com/acme/shop/pull/" + itoa(n)})
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/shop/pulls/"):
		f.updated++
		var n int
		for _, v := range f.open {
			n = v
		}
		f.bodies[n] = in["body"].(string)
		json.NewEncoder(w).Encode(map[string]any{"number": n})
	default:
		http.NotFound(w, r)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestShipToGitHub(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	blobs := blob.NewMem()

	// "github.com/acme/shop" is a local repository.
	gitBase := t.TempDir()
	up := newUpstream(t)
	up.commit("Initial", map[string]string{"auth.go": "package auth\n\nfunc Login() {\n}\n", "README.md": "# shop\n"})
	if err := os.MkdirAll(filepath.Join(gitBase, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(up.dir, filepath.Join(gitBase, "acme", "shop.git")); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{open: map[string]int{}, bodies: map[int]string{}}
	api := httptest.NewServer(gh)
	defer api.Close()

	br := gitbridge.New(st, blobs, t.TempDir(), func() string { return "gh-test-token" })
	br.GitHubAPI = api.URL
	br.GitHubGitBase = gitBase
	svc := service.New(st, blobs, br)

	alice, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice Doe", Email: "alice@example.com"})
	ag, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/backend", ParentID: &alice.ID})
	human, _ := svc.ResolveActor(ctx, alice)
	agent, _ := svc.ResolveActor(ctx, ag)

	if _, err := svc.CreateRepo(ctx, human, service.CreateRepoInput{Name: "acme/shop", Provider: store.ProviderGitHub, Remote: "acme/shop"}); err != nil {
		t.Fatal(err)
	}
	eff, err := svc.CreateEffort(ctx, human, service.CreateEffortInput{Repo: "acme/shop", Name: "Rate limiting", Goal: "Limit login attempts per IP."})
	if err != nil {
		t.Fatal(err)
	}

	land := func(intent string, files map[string]string, ev *object.Evidence) {
		t.Helper()
		ws, err := svc.OpenWorkspace(ctx, agent, "acme/shop", eff.Slug, intent)
		if err != nil {
			t.Fatal(err)
		}
		repo := ws.Repo
		row, _ := st.GetChangeset(ctx, repo.ID, ws.Workspace.Base)
		var changes []object.Change
		for p, c := range files {
			h, _ := blobs.Put(ctx, hash.KindBlob, []byte(c))
			changes = append(changes, object.Change{Path: p, Op: object.OpModify, New: object.File{Hash: h, Mode: object.ModeFile}})
		}
		tree, err := object.ApplyChanges(ctx, blobs, row.Tree, changes)
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.Land(ctx, agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree, Evidence: ev, Model: "claude-opus-5-5"})
		if err != nil || !res.Landed {
			t.Fatalf("land %q: %+v %v", intent, res, err)
		}
	}
	land("Add rate limit middleware", map[string]string{"ratelimit.go": "package auth // limiter\n"},
		&object.Evidence{Checks: []object.Check{{Name: "go test ./...", Result: "pass", Passed: 12}}})

	// Meanwhile someone pushes to main on GitHub.
	newMain := up.commit("Docs update", map[string]string{"README.md": "# shop\n\nNow with docs.\n"})

	res, err := svc.OpenShipment(ctx, human, service.ShipInput{Repo: "acme/shop", Effort: eff.Slug})
	if err != nil {
		t.Fatal(err)
	}
	if res.PR.Number != 41 || res.Shipment.Branch != "vx/rate-limiting" || res.Shipment.BaseGitSHA != newMain.String() {
		t.Fatalf("shipment = %+v, pr = %+v", res.Shipment, res.PR)
	}
	body := gh.bodies[41]
	for _, want := range []string{"Limit login attempts per IP.", "Add rate limit middleware", "agent:alice/backend for user:alice", "go test ./... pass (12 passed, 0 failed)"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}

	// The pushed branch sits on the new main and has both changes.
	ref, err := up.repo.Reference(plumbing.NewBranchReferenceName("vx/rate-limiting"), true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := up.repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ParentHashes) != 1 || c.ParentHashes[0] != newMain {
		t.Fatalf("commit parent = %v, want %s", c.ParentHashes, newMain)
	}
	if c.Author.Email != "alice@example.com" || c.Author.Name != "Alice Doe" {
		t.Fatalf("author = %+v", c.Author)
	}
	for _, want := range []string{"Add rate limit middleware\n\nVx-Change-Id: ", "Vx-Agent: agent:alice/backend", "Vx-On-Behalf-Of: user:alice", "Vx-Model: claude-opus-5-5", "Vx-Evidence: go test ./... pass"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("commit message missing %q:\n%s", want, c.Message)
		}
	}
	tree, _ := c.Tree()
	for p, want := range map[string]string{"ratelimit.go": "package auth // limiter\n", "README.md": "# shop\n\nNow with docs.\n"} {
		f, err := tree.File(p)
		if err != nil {
			t.Fatalf("%s missing from pushed tree: %v", p, err)
		}
		got, _ := f.Contents()
		if got != want {
			t.Fatalf("%s = %q, want %q", p, got, want)
		}
	}

	// Another land, ship again: same branch and PR, now two commits.
	land("Wire limiter into Login", map[string]string{"auth.go": "package auth\n\nfunc Login() {\n\tlimit()\n}\n"}, nil)
	res, err = svc.OpenShipment(ctx, human, service.ShipInput{Repo: "acme/shop", Effort: eff.Slug})
	if err != nil {
		t.Fatal(err)
	}
	if !res.PR.Updated || res.PR.Number != 41 || gh.created != 1 || gh.updated != 1 || len(res.Shipment.Changes) != 2 {
		t.Fatalf("second ship: pr %+v, created %d, updated %d, changes %d", res.PR, gh.created, gh.updated, len(res.Shipment.Changes))
	}
	ref, _ = up.repo.Reference(plumbing.NewBranchReferenceName("vx/rate-limiting"), true)
	head, _ := up.repo.CommitObject(ref.Hash())
	if !strings.HasPrefix(head.Message, "Wire limiter into Login") || head.ParentHashes[0].String() != res.Shipment.Changes[0].GitSHA {
		t.Fatalf("second commit = %q parent %v", head.Message, head.ParentHashes)
	}
	// main itself was never touched.
	mainRef, _ := up.repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if mainRef.Hash() != newMain {
		t.Fatal("vx must never move main")
	}
	shipments, err := svc.ListShipments(ctx, "acme/shop", eff.Slug)
	if err != nil || len(shipments) != 1 || shipments[0].ProviderRef != "41" {
		t.Fatalf("shipments = %+v, %v", shipments, err)
	}
}

func TestExportRefusesNonVXBranch(t *testing.T) {
	br := gitbridge.New(nil, blob.NewMem(), t.TempDir(), nil)
	_, err := br.ExportBranch(context.Background(), store.Repo{Provider: store.ProviderGit, Remote: "/nowhere"}, "abc", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "vx only pushes") {
		t.Fatalf("err = %v", err)
	}
}
