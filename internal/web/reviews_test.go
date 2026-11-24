package web_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// landFiles lands files from a new workspace of actor and returns the
// result.
func (e *webEnv) landFiles(actor service.Actor, slug, intent string, files map[string]string, ev *object.Evidence) service.LandResult {
	e.t.Helper()
	ws, err := e.svc.OpenWorkspace(e.ctx, actor, "acme/shop", slug, intent)
	if err != nil {
		e.t.Fatal(err)
	}
	base, err := e.st.GetChangeset(e.ctx, ws.Repo.ID, ws.Workspace.Base)
	if err != nil {
		e.t.Fatal(err)
	}
	var changes []object.Change
	for p, c := range files {
		h, _ := e.svc.Blobs.Put(e.ctx, hash.KindBlob, []byte(c))
		changes = append(changes, object.Change{Path: p, Op: object.OpModify, New: object.File{Hash: h, Mode: object.ModeFile}})
	}
	tree, err := object.ApplyChanges(e.ctx, e.svc.Blobs, base.Tree, changes)
	if err != nil {
		e.t.Fatal(err)
	}
	storetest.AddTree(e.t, e.st, e.svc.Blobs, ws.Repo.ID, base.Tree, tree)
	res, err := e.svc.Land(e.ctx, actor, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree, Evidence: ev})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// The Reviews page lists changes waiting for a person; the change page
// shows a review with Approve and Reject, which refuse cross-origin posts
// and agents; approving lands it and the effort page stops listing it.
func TestReviewsInWebUI(t *testing.T) {
	e := newWebEnv(t)
	ctx := e.ctx
	if _, err := e.svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/shop"}); err != nil {
		t.Fatal(err)
	}
	eff, err := e.svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Schema"})
	if err != nil {
		t.Fatal(err)
	}
	agentP, agentToken, err := e.svc.CreateAgent(ctx, e.alice, "backend", "")
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := e.svc.ResolveActor(ctx, agentP)
	e.landFiles(agent, eff.Slug, "Rules", map[string]string{"vx.toml": "[[policy]]\npaths = [\"db/**\"]\nmode = \"human\"\n"}, nil)
	res := e.landFiles(agent, eff.Slug, "Users table <b>", map[string]string{"db/002_users.sql": "create table users ();\n"}, nil)
	if res.Review == nil {
		t.Fatalf("not pending: %+v", res)
	}
	id := res.Review.ID.String()

	token, _ := e.st.CreateToken(ctx, e.alice.ID, "web")
	e.signIn(token)
	get := func(c *http.Client, path string) string {
		t.Helper()
		resp, err := c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d\n%s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	body := get(e.c, "/ui/reviews")
	for _, want := range []string{"Waiting for a person", "Users table &lt;b&gt;", "db/002_users.sql", "can approve", "review=" + id, `href="/ui/reviews"`} {
		if !strings.Contains(body, want) {
			t.Errorf("reviews page lacks %q", want)
		}
	}
	effortPath := "/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug
	if body := get(e.c, effortPath); !strings.Contains(body, `id="reviews"`) || !strings.Contains(body, "review="+id) {
		t.Errorf("effort page does not list the pending review")
	}
	changePath := "/ui/change?repo=acme%2Fshop&review=" + id
	body = get(e.c, changePath)
	for _, want := range []string{
		"Waiting for a person to review: db/002_users.sql (policy in vx.toml)", `action="/ui/review"`,
		`name="decision" value="approve"`, `name="decision" value="reject"`, `name="reason"`, `fd-path">db/002_users.sql`, `[[policy]] paths = [&#34;db/**&#34;]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review page lacks %q", want)
		}
	}
	if strings.Contains(body, ` style="`) || strings.Contains(body, `<script>`) {
		t.Error("review page: inline style or script breaks the CSP")
	}

	// Cross-origin posts are refused.
	approve := url.Values{"review": {id}, "decision": {"approve"}}
	for _, h := range []map[string]string{{}, {"Origin": "http://evil.example"}} {
		if code, _ := e.post("/ui/review", approve, h); code != http.StatusForbidden {
			t.Errorf("approve with %v = %d, want 403", h, code)
		}
	}
	// Agents cannot decide in the web UI.
	agentEnv := newWebEnvShared(e)
	agentEnv.signIn(agentToken)
	if code, _ := agentEnv.post("/ui/review", approve, map[string]string{"Origin": e.srv.URL}); code != http.StatusForbidden {
		t.Errorf("agent approves in the web UI = %d, want 403", code)
	}
	if body := get(agentEnv.c, changePath); strings.Contains(body, `value="approve"`) || !strings.Contains(body, "only people approve") {
		t.Errorf("an agent's view offers Approve")
	}
	if rv, _ := e.svc.GetReview(ctx, e.alice, id); rv.State != store.ReviewPending {
		t.Fatalf("refused posts decided the review: %s", rv.State)
	}
	// Alice approves: it lands and the page says so.
	code, page := e.post("/ui/review", approve, map[string]string{"Origin": e.srv.URL})
	if code != 200 || !strings.Contains(page, "Approved by alice") {
		t.Fatalf("approve = %d\n%s", code, page)
	}
	rv, _ := e.svc.GetReview(ctx, e.alice, id)
	if body := get(e.c, "/ui/change?repo=acme%2Fshop&ref="+url.QueryEscape(rv.Landed.String())); !strings.Contains(body, `<span class="pill ok">approved</span><span class="small">by alice</span>`) {
		t.Error("the landed change's evidence does not say who approved it")
	}
	if body := get(e.c, effortPath); strings.Contains(body, `id="reviews"`) {
		t.Error("effort page still lists the approved review")
	}

	// Reject needs a reason, then shows it.
	res = e.landFiles(agent, eff.Slug, "Drop users", map[string]string{"db/003_drop.sql": "drop table users;\n"}, nil)
	id2 := res.Review.ID.String()
	if code, _ := e.post("/ui/review", url.Values{"review": {id2}, "decision": {"reject"}}, map[string]string{"Origin": e.srv.URL}); code == 200 {
		t.Error("reject without a reason succeeded")
	}
	code, page = e.post("/ui/review", url.Values{"review": {id2}, "decision": {"reject"}, "reason": {"Never drop tables"}}, map[string]string{"Origin": e.srv.URL})
	if code != 200 || !strings.Contains(page, "Rejected by alice") || !strings.Contains(page, "Never drop tables") {
		t.Fatalf("reject = %d\n%s", code, page)
	}
	if body := get(e.c, "/ui/reviews"); !strings.Contains(body, "Recently decided") || !strings.Contains(body, "Drop users") {
		t.Error("reviews page does not list decided reviews")
	}
}
