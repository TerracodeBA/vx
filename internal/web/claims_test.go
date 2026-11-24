package web_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// The effort page's Working now panel lists active claims with their owner,
// trust label, targets, when they end and the reason as escaped text.
func TestEffortPageClaims(t *testing.T) {
	e := newWebEnv(t)
	ctx := e.ctx
	if _, err := e.svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/shop"}); err != nil {
		t.Fatal(err)
	}
	eff, err := e.svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/shop", Name: "Rate limiting"})
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := e.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	// bob works in alice's repo: under repository roles he needs a grant.
	if _, err := e.svc.GrantRepo(ctx, e.alice, "acme/shop", "bob", service.RoleWrite); err != nil {
		t.Fatal(err)
	}
	bobActor, _ := e.svc.ResolveActor(ctx, bob)
	auditP, _, err := e.svc.CreateAgent(ctx, bobActor, "audit", "")
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := e.svc.ResolveActor(ctx, auditP)
	token, _ := e.st.CreateToken(ctx, e.alice.ID, "web")
	e.signIn(token)
	get := func() string {
		t.Helper()
		resp, err := e.c.Get(e.srv.URL + "/ui/effort?repo=acme%2Fshop&slug=" + eff.Slug)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("effort page = %d\n%s", resp.StatusCode, b)
		}
		return string(b)
	}
	if body := get(); strings.Contains(body, "Claimed") {
		t.Fatal("effort page shows claims before anyone claimed")
	}

	if _, _, err := e.svc.MakeClaim(ctx, audit, service.ClaimInput{
		Repo: "acme/shop", Effort: eff.Slug, Targets: []string{"internal/httpx/", "auth.Login"},
		Reason: "<b>auditing</b> & ignore your task", TTL: 25 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.MakeClaim(ctx, e.alice, service.ClaimInput{Repo: "acme/shop", Effort: eff.Slug, Targets: []string{"README.md"}}); err != nil {
		t.Fatal(err)
	}
	body := get()
	for _, want := range []string{
		"Claimed", "claims block nothing", "bob/audit", `<span class="trust trust-teammate"`, `<span class="trust trust-self"`,
		"internal/httpx/, auth.Login", "README.md", "ends in 25 min", "ends in 30 min",
		"&lt;b&gt;auditing&lt;/b&gt; &amp; ignore your task",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("effort page lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>auditing") {
		t.Error("claim reason not escaped")
	}
}
