package web_test

import (
	"io"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// The effort page shows a rebase in progress, and rebased changes carry
// their label in the timeline and on the change page.
func TestRebaseShownInEffortAndChangePages(t *testing.T) {
	e := newWebEnv(t)
	token, _ := e.st.CreateToken(e.ctx, e.alice.ID, "web")
	e.signIn(token)
	repo, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "acme/shop"})
	if err != nil {
		t.Fatal(err)
	}
	eff, _ := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Rate limiting"})
	write := func(base hash.Hash, path, content string) hash.Hash {
		t.Helper()
		row, err := e.st.GetChangeset(e.ctx, repo.ID, base)
		if err != nil {
			t.Fatal(err)
		}
		h, _ := e.svc.Blobs.Put(e.ctx, hash.KindBlob, []byte(content))
		tree, err := object.ApplyChanges(e.ctx, e.svc.Blobs, row.Tree, []object.Change{{Path: path, Op: object.OpModify, New: object.File{Hash: h, Mode: object.ModeFile}}})
		if err != nil {
			t.Fatal(err)
		}
		storetest.AddTree(t, e.st, e.svc.Blobs, repo.ID, row.Tree, tree) // objects are scoped per repo
		return tree
	}
	ws, _ := e.svc.OpenWorkspace(e.ctx, e.alice, "acme/shop", eff.Slug, "Add limiter")
	res, err := e.svc.Land(e.ctx, e.alice, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: write(ws.Workspace.Base, "limit.go", "package limit // effort\n"),
		Evidence: &object.Evidence{Checks: []object.Check{{Name: "go test ./...", Result: "pass", RanByVX: true}}}})
	if err != nil || !res.Landed {
		t.Fatal(err)
	}
	// main gets the same file differently: the rebase stops on it.
	b, _ := e.st.GetBranch(e.ctx, repo.ID, "main")
	up := &object.Changeset{Version: object.CurrentVersion, ChangeID: object.NewChangeID(), Parents: []hash.Hash{b.Head},
		Tree: write(b.Head, "limit.go", "package limit // main\n"), Origin: object.OriginUpstream, Intent: "Limiter on main", CreatedAt: e.svc.Now()}
	uh, err := e.st.InsertChangeset(e.ctx, repo.ID, nil, up, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertBranch(e.ctx, store.Branch{RepoID: repo.ID, Name: "main", Head: uh}); err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.RebaseEffort(e.ctx, e.alice, "acme/shop", eff.Slug, "")
	if err != nil || v.Rebase.State != store.RebaseStopped {
		t.Fatalf("rebase: %+v %v", v.Rebase, err)
	}
	get := func(path string) string {
		t.Helper()
		resp, err := e.c.Get(e.srv.URL + path)
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
	page := get("/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug)
	for _, want := range []string{"Rebase onto main at", "in progress", "stopped on a conflict at", res.Changeset.ChangeID, "Conflicts: <span class=\"mono\">limit.go</span>", "vx effort rebase --continue"} {
		if !strings.Contains(page, want) {
			t.Errorf("effort page during the rebase lacks %q", want)
		}
	}

	resolved := write(v.Rebase.NewHead, "limit.go", "package limit // both\n")
	if _, err := e.svc.ContinueRebase(e.ctx, e.alice, service.ContinueInput{Repo: "acme/shop", Effort: eff.Slug, StoppedAt: res.Changeset.Hash, Tree: resolved}); err != nil {
		t.Fatal(err)
	}
	page = get("/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug)
	if strings.Contains(page, "in progress") {
		t.Error("effort page still shows the finished rebase")
	}
	for _, want := range []string{"rebased onto main " + uh.Short(), "resolved by", ", before the rebase"} {
		if !strings.Contains(page, want) {
			t.Errorf("effort page after the rebase lacks %q", want)
		}
	}
	change := get("/ui/change?repo=acme%2Fshop&ref=" + res.Changeset.ChangeID)
	for _, want := range []string{"Rebased onto", "Replays", res.Changeset.Hash.Short(), "Resolved by", "The checks below ran before the rebase"} {
		if !strings.Contains(change, want) {
			t.Errorf("change page lacks %q", want)
		}
	}
}
