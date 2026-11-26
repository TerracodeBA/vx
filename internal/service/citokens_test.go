package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// ciActor authenticates with a CI token's secret, as the API does.
func (e *authzEnv) ciActor(secret string) (service.Actor, error) {
	p, tokID, err := e.svc.Store.AuthenticateToken(e.ctx, secret)
	if err != nil {
		return service.Actor{}, err
	}
	a, err := e.svc.ResolveActor(e.ctx, p)
	a.TokenID = tokID
	return a, err
}

// TestCITokenLimits: a CI token reads its repository's jobs and objects and
// nothing else; on other repositories, and for every other operation on
// its own, it is a stranger. Only people with admin create them.
func TestCITokenLimits(t *testing.T) {
	e := newAuthzEnv(t)
	tok, secret, err := e.svc.CreateCIToken(e.ctx, e.alice, "acme/shop", "gha", []string{"unit", "lint", "unit"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Name != "gha" || strings.Join(tok.Checks, ",") != "lint,unit" || !strings.HasPrefix(tok.Ref(), "service:ci-") || secret == "" {
		t.Fatalf("token = %+v", tok)
	}
	if _, _, err := e.svc.CreateCIToken(e.ctx, e.alice, "acme/shop", "gha", nil); !isCode(err, service.CodeExists) {
		t.Fatalf("second gha: %v", err)
	}
	if _, _, err := e.svc.CreateCIToken(e.ctx, e.agent, "acme/shop", "agent-made", nil); !isCode(err, service.CodePermission) {
		t.Fatalf("an agent created a CI token: %v", err)
	}
	if _, _, err := e.svc.CreateCIToken(e.ctx, e.bob, "acme/shop", "bobs", nil); !isCode(err, service.CodeNotFound) {
		t.Fatalf("bob without access: %v", err)
	}
	ci, err := e.ciActor(secret)
	if err != nil {
		t.Fatal(err)
	}
	if ci.Kind != store.KindService {
		t.Fatalf("CI token is a %s", ci.Kind)
	}

	// What it may do: its repository's jobs and the files they name.
	if _, err := e.svc.ListCheckJobs(e.ctx, ci, service.ListJobsInput{Repo: "acme/shop"}); err != nil {
		t.Fatalf("list own jobs: %v", err)
	}
	if _, err := e.svc.ListCheckJobs(e.ctx, ci, service.ListJobsInput{}); err != nil {
		t.Fatalf("list jobs without a repository: %v", err)
	}
	if j, err := e.svc.NextCheckJob(e.ctx, ci, service.NextJobInput{}); err != nil || j != nil {
		t.Fatalf("next on an empty queue: %v %v", j, err)
	}
	eff, _ := e.svc.Store.EffortByID(e.ctx, e.eff.ID)
	head, _ := e.svc.Store.GetChangeset(e.ctx, e.repo.ID, eff.Head)
	if objs, err := e.svc.GetObjects(e.ctx, ci, "acme/shop", []hash.Hash{head.Tree}, 0); err != nil || len(objs) != 1 {
		t.Fatalf("read a tree of its repository: %v", err)
	}

	// What it may not: anything else, here or elsewhere, "not found".
	refusals := map[string]error{}
	_, _, refusals["GetRepo"] = e.svc.GetRepo(e.ctx, ci, "acme/shop")
	_, refusals["ListNotes"] = e.svc.ListNotes(e.ctx, ci, service.ListNotesInput{Repo: "acme/shop", Effort: e.eff.Slug})
	_, refusals["OpenWorkspace"] = e.svc.OpenWorkspace(e.ctx, ci, "acme/shop", e.eff.Slug, "sneak in")
	_, refusals["Land"] = e.svc.Land(e.ctx, ci, service.LandInput{WorkspaceID: e.aliceWS, Tree: head.Tree})
	_, refusals["PutObjects"] = e.svc.PutObjects(e.ctx, ci, "acme/shop", []service.Object{{Kind: hash.KindBlob, Data: []byte("x")}})
	_, refusals["Log"] = e.svc.Log(e.ctx, ci, "acme/shop", e.eff.Slug, 10, false)
	_, refusals["RequestCheck"] = e.svc.RequestCheck(e.ctx, ci, service.RequestCheckInput{Repo: "acme/shop", Effort: e.eff.Slug, Check: "unit"})
	_, refusals["ListCITokens"] = e.svc.ListCITokens(e.ctx, ci, "acme/shop", false)
	_, refusals["other repo jobs"] = e.svc.ListCheckJobs(e.ctx, ci, service.ListJobsInput{Repo: "globex/site"})
	_, refusals["other repo objects"] = e.svc.GetObjects(e.ctx, ci, "globex/site", []hash.Hash{head.Tree}, 0)
	_, _, refusals["other repo"] = e.svc.GetRepo(e.ctx, ci, "globex/site")
	for op, err := range refusals {
		if !isCode(err, service.CodeNotFound) {
			t.Errorf("CI token %s: want not found, got %v", op, err)
		}
	}
	if _, _, err := e.svc.CreateCIToken(e.ctx, ci, "acme/shop", "more", nil); !isCode(err, service.CodeNotFound) {
		t.Errorf("CI token minting a CI token: %v", err)
	}
	if _, err := e.svc.SetAgentRunner(e.ctx, ci, "alice/claude", []string{"browser"}); !isCode(err, service.CodePermission) {
		t.Errorf("CI token setting runner kinds: %v", err)
	}

	list, err := e.svc.ListCITokens(e.ctx, e.alice, "acme/shop", false)
	if err != nil || len(list) != 1 || list[0].Name != "gha" || list[0].CreatedByRef != "user:alice" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if _, err := e.svc.RevokeCIToken(e.ctx, e.alice, "acme/shop", "gha"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ciActor(secret); err == nil {
		t.Fatal("a revoked CI token still authenticates")
	}
	if _, err := e.svc.ListCheckJobs(e.ctx, ci, service.ListJobsInput{Repo: "acme/shop"}); !isCode(err, service.CodeNotFound) {
		t.Fatalf("revoked CI token lists jobs: %v", err)
	}
	if _, err := e.svc.RevokeCIToken(e.ctx, e.alice, "acme/shop", "gha"); !isCode(err, service.CodeNotFound) {
		t.Fatalf("revoking twice: %v", err)
	}
	if list, _ := e.svc.ListCITokens(e.ctx, e.alice, "acme/shop", true); len(list) != 1 || list[0].RevokedAt == nil {
		t.Fatalf("list with revoked = %+v", list)
	}
}

// TestAgentRunnerKinds: a person says which runner kinds their agents
// serve; agents cannot, not even for themselves, nor can other people.
func TestAgentRunnerKinds(t *testing.T) {
	e := newAuthzEnv(t)
	v, err := e.svc.SetAgentRunner(e.ctx, e.alice, "claude", []string{"security", "agent:browser", "browser", " "})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.Kinds, ",") != "browser,security" {
		t.Fatalf("kinds = %v", v.Kinds)
	}
	if got, err := e.svc.GetAgentRunner(e.ctx, e.agent, ""); err != nil || strings.Join(got.Kinds, ",") != "browser,security" {
		t.Fatalf("the agent reads its kinds: %+v %v", got, err)
	}
	if _, err := e.svc.SetAgentRunner(e.ctx, e.agent, "", []string{"browser"}); !isCode(err, service.CodePermission) {
		t.Fatalf("an agent set its own kinds: %v", err)
	}
	if _, err := e.svc.SetAgentRunner(e.ctx, e.bob, "alice/claude", []string{"browser"}); !isCode(err, service.CodePermission) {
		t.Fatalf("bob set alice's agent's kinds: %v", err)
	}
	if _, err := e.svc.GetAgentRunner(e.ctx, e.bob, "alice/claude"); !isCode(err, service.CodePermission) {
		t.Fatalf("bob read alice's agent's kinds: %v", err)
	}
	for _, bad := range []string{"ci", "human", "Browser!", strings.Repeat("x", 41)} {
		if _, err := e.svc.SetAgentRunner(e.ctx, e.alice, "claude", []string{bad}); !isCode(err, service.CodeInvalid) {
			t.Errorf("kind %q: %v", bad, err)
		}
	}
	if v, err := e.svc.SetAgentRunner(e.ctx, e.alice, "claude", nil); err != nil || len(v.Kinds) != 0 {
		t.Fatalf("clear: %+v %v", v, err)
	}
	if _, err := e.svc.NextCheckJob(e.ctx, e.agent, service.NextJobInput{Repo: "acme/shop"}); !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "vx agent runner claude --kinds") {
		t.Fatalf("next for an agent without kinds: %v", err)
	}
}
