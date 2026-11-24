package service_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// TestGitHubSignInFindsExistingAccount: a first GitHub sign-in whose
// verified email belongs to a person does not create a second account; the
// person signs in, connects GitHub, and later sign-ins find them.
func TestGitHubSignInFindsExistingAccount(t *testing.T) {
	svc, gh, ctx := signinEnv(t, service.SignupOpen)
	alice, err := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", Email: "Alice@Example.com"})
	if err != nil {
		t.Fatal(err)
	}
	gh.AddUser("alice-gh")
	gh.SetProfile("alice-gh", "Alice", "alice@example.com")
	_, _, _, err = signIn(t, svc, gh, "alice-gh")
	var exists *service.AccountExistsError
	if !errors.As(err, &exists) || exists.Handle != "alice" || exists.Via != "GitHub" || code(err) != service.CodeExists ||
		!strings.Contains(err.Error(), "sign in to it first, then connect GitHub") {
		t.Fatalf("first GitHub sign-in = %v", err)
	}
	people, _ := svc.Store.ListPeople(ctx)
	if len(people) != 1 {
		t.Fatalf("a second account was created: %+v", people)
	}
	// Signed in as alice, she connects GitHub; from then on GitHub signs her in.
	actor, _ := svc.ResolveActor(ctx, alice)
	authURL, _, verifier, err := svc.GitHubSignInStart(ctx, "/ui/settings")
	if err != nil {
		t.Fatal(err)
	}
	_ = authURL
	code := gh.OAuthCode("alice-gh", testPublicURL+service.GitHubCallbackPath, service.PKCEChallenge(verifier))
	if _, err := svc.GitHubLinkFinish(ctx, actor, code, verifier); err != nil {
		t.Fatal(err)
	}
	p, _, isNew, err := signIn(t, svc, gh, "alice-gh")
	if err != nil || isNew || p.ID != alice.ID {
		t.Fatalf("after connecting: %+v new=%v %v", p, isNew, err)
	}
	// A GitHub no-reply address never matches anyone.
	if _, err := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "noreply", Email: "1+x@users.noreply.github.com"}); err != nil {
		t.Fatal(err)
	}
	gh.AddUser("x")
	gh.SetProfile("x", "", "1+x@users.noreply.github.com")
	if _, _, isNew, err := signIn(t, svc, gh, "x"); err != nil || !isNew {
		t.Fatalf("no-reply email: new=%v %v", isNew, err)
	}
}

// TestMergePeople: a superadmin merges a duplicate account into the one
// that stays: tokens, agents, memberships (higher role wins), grants and
// the GitHub link move; the merged one is disabled and logged. Superadmins
// cannot merge their own account, and nobody else can merge at all.
func TestMergePeople(t *testing.T) {
	e := newSSOEnv(t)
	ctx, svc := e.ctx, e.svc
	root := e.personWithToken("root", "root@vx.test")
	if _, _, err := svc.ConsoleSetSuperadmin(ctx, "root", true); err != nil {
		t.Fatal(err)
	}
	root, _ = svc.ResolveActor(ctx, mustPerson(t, svc, "root"))
	// bob signed up twice: "bob" (member of acme) and "bob2" (with a token,
	// an agent, admin of acme, a write grant, GitHub).
	bob2 := e.personWithToken("bob2", "bob@acme.test")
	if _, err := svc.SetMember(ctx, e.alice, "acme", "bob2", service.OrgAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GrantRepo(ctx, e.alice, "acme/api", "bob2", service.RoleWrite); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GrantRepo(ctx, e.alice, "acme/api", "bob", service.RoleRead); err != nil {
		t.Fatal(err)
	}
	agent, agentTok, err := svc.CreateAgent(ctx, bob2, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, bob2Tok, err := svc.CreateToken(ctx, bob2, "", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.LinkGitHub(ctx, bob2.ID, 4242, "bob-gh"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.MergePeople(ctx, e.alice, "bob", "bob2"); code(err) != service.CodePermission {
		t.Fatalf("non-superadmin merges: %v", err)
	}
	if _, err := svc.MergePeople(ctx, root, "root", "bob2"); code(err) != service.CodePermission || !strings.Contains(err.Error(), "cannot merge their own account") {
		t.Fatalf("superadmin keeps a customer's account: %v", err)
	}
	if _, err := svc.MergePeople(ctx, root, "bob", "root"); code(err) != service.CodePermission {
		t.Fatalf("superadmin merges themselves into a customer: %v", err)
	}
	if _, err := svc.MergePeople(ctx, root, "bob", "user:bob"); code(err) != service.CodeInvalid {
		t.Fatalf("same person: %v", err)
	}
	if _, err := svc.MergePeople(ctx, root, "bob", "bob2/claude"); code(err) != service.CodeInvalid {
		t.Fatalf("an agent: %v", err)
	}

	res, err := svc.MergePeople(ctx, root, "bob", "user:bob2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved.Tokens < 2 || res.Moved.Agents != 1 || !res.Moved.GitHub || res.Kept.Handle != "bob" || res.Merged.Handle != "bob2" {
		t.Fatalf("merge = %+v", res)
	}
	// bob2's tokens now act as bob; the agent keeps its handle, now bob's.
	if a := e.actorFor(bob2Tok); a.Handle != "bob" {
		t.Fatalf("bob2's token acts as %s", a.Ref())
	}
	if a := e.actorFor(agentTok); a.Handle != agent.Handle || a.Parent == nil || a.Parent.Handle != "bob" {
		t.Fatalf("agent = %s for %v", a.Ref(), a.Parent)
	}
	// Higher role wins: admin of acme, write on acme/api.
	acme := mustOrg(t, svc, "acme")
	if role, _ := svc.Store.OrgRole(ctx, acme.ID, e.bob.ID); role != "admin" {
		t.Fatalf("bob's role in acme = %q", role)
	}
	grants, err := svc.Store.ListRepoGrants(ctx, repoOf(t, svc, "acme/api").ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		if g.Principal != nil && g.Principal.Handle == "bob2" {
			t.Fatalf("grant left on bob2: %+v", g)
		}
		if g.Principal != nil && g.Principal.Handle == "bob" && g.Role != "write" {
			t.Fatalf("bob's grant = %q", g.Role)
		}
	}
	if l, ok, _ := svc.Store.GitHubLinkOf(ctx, e.bob.ID); !ok || l.UserID != 4242 {
		t.Fatalf("GitHub link = %+v %v", l, ok)
	}
	// bob2 is disabled, stays in history, and is not a match for emails.
	if disabled, into, _ := svc.Store.PrincipalDisabled(ctx, bob2.ID); !disabled || into == nil || *into != e.bob.ID {
		t.Fatalf("bob2 disabled=%v into=%v", disabled, into)
	}
	if people, _ := svc.Store.PeopleByEmail(ctx, "bob@acme.test"); len(people) != 1 || people[0].Handle != "bob" {
		t.Fatalf("people with bob's email: %+v", people)
	}
	if _, err := svc.MergePeople(ctx, root, "bob", "bob2"); code(err) != service.CodeFailedPrecondition {
		t.Fatalf("merging again: %v", err)
	}
	if _, _, err := svc.ConsoleSetSuperadmin(ctx, "bob2", true); code(err) != service.CodeFailedPrecondition {
		t.Fatalf("superadmin for a merged account: %v", err)
	}
	ops, _ := svc.Store.ListTargetOps(ctx, "user:bob2", 10)
	if len(ops) == 0 || ops[0].Op != "person.merge" || ops[0].ActorID == nil || *ops[0].ActorID != root.ID || !strings.Contains(string(ops[0].After), `"into": "user:bob"`) && !strings.Contains(string(ops[0].After), `"into":"user:bob"`) {
		t.Fatalf("op log = %+v", ops)
	}
}

func mustPerson(t *testing.T, svc *service.Service, handle string) store.Principal {
	t.Helper()
	p, err := svc.Store.PrincipalByHandle(t.Context(), handle)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func repoOf(t *testing.T, svc *service.Service, name string) store.Repo {
	t.Helper()
	r, err := svc.Store.RepoByName(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
