package service_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

const testPublicURL = "https://vx.example.com"

func signinEnv(t *testing.T, mode service.SignupMode) (*service.Service, *githubtest.Server, context.Context) {
	t.Helper()
	st := storetest.New(t)
	svc := service.New(st, nil, nil)
	gh := githubtest.New(t)
	svc.GitHub.PublicURL, svc.GitHub.Signup = testPublicURL, mode
	gh.Configure(t, svc)
	return svc, gh, context.Background()
}

// signIn runs the whole web flow against the fake for login: start, approve
// on "GitHub", finish.
func signIn(t *testing.T, svc *service.Service, gh *githubtest.Server, login string) (store.Principal, string, bool, error) {
	t.Helper()
	ctx := context.Background()
	authURL, state, verifier, err := svc.GitHubSignInStart(ctx, "/ui")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("state") != state || q.Get("code_challenge") != service.PKCEChallenge(verifier) || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != testPublicURL+service.GitHubCallbackPath || !strings.HasPrefix(authURL, gh.URL+"/login/oauth/authorize?") {
		t.Fatalf("authorize URL %s", authURL)
	}
	if strings.Contains(authURL, verifier) {
		t.Fatal("the PKCE verifier must never leave the server")
	}
	code := gh.OAuthCode(login, q.Get("redirect_uri"), q.Get("code_challenge"))
	return svc.GitHubSignInFinish(ctx, code, verifier)
}

func TestGitHubSignInCreatesPerson(t *testing.T) {
	svc, gh, ctx := signinEnv(t, service.SignupOpen)
	acct := gh.AddUser("Octo_Cat")
	gh.SetProfile("Octo_Cat", "Mona Octocat", "mona@example.com")

	p, token, isNew, err := signIn(t, svc, gh, "Octo_Cat")
	if err != nil || !isNew {
		t.Fatalf("first sign-in: new=%v err=%v", isNew, err)
	}
	if p.Handle != "octo_cat" || p.DisplayName != "Mona Octocat" || p.Email != "mona@example.com" || p.Kind != store.KindHuman || p.SiteAdmin {
		t.Fatalf("person = %+v", p)
	}
	if got, err := svc.Store.Authenticate(ctx, token); err != nil || got.ID != p.ID {
		t.Fatalf("session token does not authenticate: %v", err)
	}
	link, ok, _ := svc.Store.GitHubLinkOf(ctx, p.ID)
	if !ok || link.UserID != acct.ID || link.Login != "Octo_Cat" {
		t.Fatalf("link = %+v %v", link, ok)
	}
	if _, err := svc.Store.OrgByHandle(ctx, "octo_cat"); err != nil {
		t.Fatalf("personal organization: %v", err)
	}

	// Second sign-in after a rename on GitHub: same person, found by id.
	gh.RenameAccount("Octo_Cat", "mona")
	gh.SetProfile("mona", "Mona Lisa", "")
	p2, _, isNew, err := signIn(t, svc, gh, "mona")
	if err != nil || isNew || p2.ID != p.ID {
		t.Fatalf("second sign-in: %+v new=%v err=%v", p2, isNew, err)
	}
	if p2.Handle != "octo_cat" || p2.DisplayName != "Mona Lisa" {
		t.Fatalf("after rename: %+v", p2)
	}
	if link, _, _ := svc.Store.GitHubLinkOf(ctx, p.ID); link.Login != "mona" {
		t.Fatalf("login not updated: %+v", link)
	}
	ops := opsOf(t, svc, p.ID, "person.")
	if strings.Join(ops, ",") != "person.signup,person.signin" {
		t.Fatalf("op log = %v", ops)
	}
}

// opsOf lists the operations a person performed, oldest first, that start
// with prefix.
func opsOf(t *testing.T, svc *service.Service, who uuid.UUID, prefix string) []string {
	t.Helper()
	ops, err := svc.Store.ListActorOps(context.Background(), who, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(ops) - 1; i >= 0; i-- {
		if strings.HasPrefix(ops[i].Op, prefix) {
			out = append(out, ops[i].Op)
		}
	}
	return out
}

func TestGitHubSignInHandlesAndEmail(t *testing.T) {
	svc, gh, ctx := signinEnv(t, service.SignupOpen)
	// "alice" is taken by a person made with a token, "alice-2" by an
	// organization: the GitHub user alice becomes alice-3.
	if _, err := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"}); err != nil {
		t.Fatal(err)
	}
	owner, _ := svc.Store.PrincipalByHandle(ctx, "alice")
	if _, err := svc.Store.CreateOrg(ctx, "alice-2", "", owner.ID); err != nil {
		t.Fatal(err)
	}
	acct := gh.AddUser("Alice")
	gh.SetProfile("Alice", "", "") // no name, no public email
	p, _, _, err := signIn(t, svc, gh, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	wantEmail := fmt.Sprintf("%d+Alice@users.noreply.github.com", acct.ID)
	if p.Handle != "alice-3" || p.Email != wantEmail || p.DisplayName != "Alice" {
		t.Fatalf("person = %+v, want alice-3 with %s", p, wantEmail)
	}
	// The person made with a token is untouched.
	if l, ok, _ := svc.Store.GitHubLinkOf(ctx, owner.ID); ok {
		t.Fatalf("token person got linked: %+v", l)
	}
}

func TestGitHubHandle(t *testing.T) {
	long := strings.Repeat("a", 39)
	for _, c := range []struct {
		login string
		want  string
	}{
		{"Octocat", "octocat"},
		{"mona-lisa", "mona-lisa"},
		{"-weird-", "weird"},
		{"üser", "ser"},
		{"a+b", "a-b"},
		{"---", "github-7"},
		{long, long},
	} {
		if got := service.GitHubHandle(c.login, 7); got != c.want {
			t.Errorf("GitHubHandle(%q) = %q, want %q", c.login, got, c.want)
		}
		if err := store.ValidateHandle(store.KindHuman, service.GitHubHandle(c.login, 7)); err != nil {
			t.Errorf("GitHubHandle(%q) is not a valid handle: %v", c.login, err)
		}
	}
}

func TestGitHubHandleCollisionAtMaxLength(t *testing.T) {
	svc, gh, ctx := signinEnv(t, service.SignupOpen)
	long := strings.Repeat("b", 39)
	if _, err := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: long}); err != nil {
		t.Fatal(err)
	}
	gh.AddUser(long)
	p, _, _, err := signIn(t, svc, gh, long)
	if err != nil || p.Handle != strings.Repeat("b", 37)+"-2" {
		t.Fatalf("handle = %q err=%v", p.Handle, err)
	}
}

func TestGitHubSignupModes(t *testing.T) {
	t.Run("invite", func(t *testing.T) {
		svc, gh, ctx := signinEnv(t, "") // empty means invite
		gh.AddUser("bob")
		_, _, _, err := signIn(t, svc, gh, "bob")
		var refused *service.GitHubSignupRefused
		if !errors.As(err, &refused) || refused.Mode != service.SignupInvite || refused.Login != "bob" {
			t.Fatalf("uninvited sign-in: %v", err)
		}
		if _, err := svc.InviteGitHubLogin(ctx, nil, "@Bob", "waitlist", false); err != nil {
			t.Fatal(err)
		}
		p, _, isNew, err := signIn(t, svc, gh, "bob")
		if err != nil || !isNew || p.Handle != "bob" {
			t.Fatalf("invited sign-in: %+v %v", p, err)
		}
		if inv, _ := svc.Store.ListInvites(ctx, false); len(inv) != 0 {
			t.Fatalf("invite not used up: %+v", inv)
		}
		// Signing in again needs no invite; inviting them again is refused.
		if _, _, isNew, err := signIn(t, svc, gh, "bob"); err != nil || isNew {
			t.Fatalf("second sign-in: %v", err)
		}
		if _, err := svc.InviteGitHubLogin(ctx, nil, "bob", "", false); err == nil {
			t.Fatal("inviting someone who has an account must fail")
		}
		// A used invite does not let a second GitHub account with a reused
		// login in either.
		gh.RenameAccount("bob", "robert")
		gh.AddUser("bob")
		gh.SetProfile("bob", "", "") // GitHub accounts never share a verified email
		if _, _, _, err := signIn(t, svc, gh, "bob"); !errors.As(err, &refused) {
			t.Fatalf("a used invite worked twice: %v", err)
		}
	})
	t.Run("invite makes site admin from the console", func(t *testing.T) {
		svc, gh, _ := signinEnv(t, service.SignupInvite)
		gh.AddUser("root")
		if _, err := svc.InviteGitHubLogin(context.Background(), nil, "root", "", true); err != nil {
			t.Fatal(err)
		}
		p, _, _, err := signIn(t, svc, gh, "root")
		if err != nil || !p.SiteAdmin {
			t.Fatalf("console invite with --admin: %+v %v", p, err)
		}
	})
	t.Run("closed", func(t *testing.T) {
		svc, gh, ctx := signinEnv(t, service.SignupClosed)
		gh.AddUser("carol")
		svc.InviteGitHubLogin(ctx, nil, "carol", "", false)
		_, _, _, err := signIn(t, svc, gh, "carol")
		var refused *service.GitHubSignupRefused
		if !errors.As(err, &refused) || refused.Mode != service.SignupClosed {
			t.Fatalf("closed sign-up: %v", err)
		}
		// People who already have an account still sign in.
		dave, _ := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "dave"})
		acct := gh.AddUser("dave")
		if err := svc.Store.LinkGitHub(ctx, dave.ID, acct.ID, "dave"); err != nil {
			t.Fatal(err)
		}
		if p, _, isNew, err := signIn(t, svc, gh, "dave"); err != nil || isNew || p.ID != dave.ID {
			t.Fatalf("existing person on a closed server: %v", err)
		}
	})
}

func TestGitHubSignInCodeChecks(t *testing.T) {
	svc, gh, ctx := signinEnv(t, service.SignupOpen)
	gh.AddUser("erin")
	_, _, verifier, _ := svc.GitHubSignInStart(ctx, "")
	redirect := testPublicURL + service.GitHubCallbackPath

	// A code minted for another verifier's challenge (PKCE) is refused.
	code := gh.OAuthCode("erin", redirect, service.PKCEChallenge("some-other-verifier-of-enough-length-000000"))
	if _, _, _, err := svc.GitHubSignInFinish(ctx, code, verifier); err == nil {
		t.Fatal("a verifier that does not match the challenge must be refused")
	}
	// A code works once.
	code = gh.OAuthCode("erin", redirect, service.PKCEChallenge(verifier))
	if _, _, _, err := svc.GitHubSignInFinish(ctx, code, verifier); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.GitHubSignInFinish(ctx, code, verifier); err == nil || !strings.Contains(err.Error(), "used already") {
		t.Fatalf("replayed code: %v", err)
	}
	if _, _, _, err := svc.GitHubSignInFinish(ctx, "", verifier); err == nil {
		t.Fatal("an empty code must be refused")
	}

	// Without a public URL, it says to set one.
	svc.GitHub.PublicURL = ""
	if _, _, _, err := svc.GitHubSignInStart(ctx, ""); err == nil || !strings.Contains(err.Error(), "VX_PUBLIC_URL") {
		t.Fatalf("no public URL: %v", err)
	}
	// Without an App, sign-in says GitHub is not configured.
	bare := service.New(storetest.New(t), nil, nil)
	gh.Prepare(t, bare)
	if bare.GitHubSignInEnabled(ctx) {
		t.Fatal("sign-in enabled without an App")
	}
	if _, _, _, err := bare.GitHubSignInStart(ctx, ""); err == nil || !errors.Is(err, service.ErrGitHubNotConfigured) {
		t.Fatalf("no App: %v", err)
	}
}

func TestGitHubLinkExistingPerson(t *testing.T) {
	svc, gh, ctx := signinEnv(t, service.SignupClosed)
	frank, _ := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "frank", DisplayName: "Frank"})
	grace, _ := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "grace"})
	actor, _ := svc.ResolveActor(ctx, frank)
	graceActor, _ := svc.ResolveActor(ctx, grace)
	gh.AddUser("frank-gh")
	redirect := testPublicURL + service.GitHubCallbackPath
	link := func(a service.Actor, login string) error {
		_, _, verifier, err := svc.GitHubSignInStart(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.GitHubLinkFinish(ctx, a, gh.OAuthCode(login, redirect, service.PKCEChallenge(verifier)), verifier)
		return err
	}
	if err := link(actor, "frank-gh"); err != nil {
		t.Fatal(err)
	}
	// Now frank signs in with GitHub, even on a closed server, and keeps
	// his account.
	p, _, isNew, err := signIn(t, svc, gh, "frank-gh")
	if err != nil || isNew || p.ID != frank.ID || p.Handle != "frank" {
		t.Fatalf("sign-in after link: %+v new=%v err=%v", p, isNew, err)
	}
	// The same GitHub account cannot be linked to grace too, and frank
	// cannot link a second one.
	if err := link(graceActor, "frank-gh"); err == nil || !strings.Contains(err.Error(), "already connected to user:frank") {
		t.Fatalf("second link of one GitHub account: %v", err)
	}
	gh.AddUser("frank-alt")
	if err := link(actor, "frank-alt"); err == nil {
		t.Fatal("a person keeps one GitHub account")
	}
	// Agents cannot link.
	ag, _ := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "frank/bot", ParentID: &frank.ID})
	agActor, _ := svc.ResolveActor(ctx, ag)
	if _, err := svc.GitHubLinkFinish(ctx, agActor, "x", "y"); err == nil {
		t.Fatal("agents must not link GitHub accounts")
	}
	if ops := opsOf(t, svc, frank.ID, "person.github_link"); len(ops) != 1 {
		t.Fatalf("op log = %v", ops)
	}
}

func TestInviteGitHubUserPermissions(t *testing.T) {
	svc, _, ctx := signinEnv(t, service.SignupInvite)
	admin, _ := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "root", SiteAdmin: true})
	user, _ := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "user"})
	ag, _ := svc.Store.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "root/bot", ParentID: &admin.ID})
	adminA, _ := svc.ResolveActor(ctx, admin)
	userA, _ := svc.ResolveActor(ctx, user)
	agA, _ := svc.ResolveActor(ctx, ag)
	if err := svc.InviteGitHubUser(ctx, userA, "x", ""); err == nil {
		t.Fatal("people who are not site admins must not invite")
	}
	if err := svc.InviteGitHubUser(ctx, agA, "x", ""); err == nil {
		t.Fatal("agents must not invite")
	}
	if err := svc.InviteGitHubUser(ctx, adminA, "not a login!", ""); err == nil {
		t.Fatal("a bad login must be refused")
	}
	if err := svc.InviteGitHubUser(ctx, adminA, "https://github.com/Mona-Lisa", "from the waitlist"); err != nil {
		t.Fatal(err)
	}
	inv, err := svc.ListGitHubInvites(ctx, adminA, false)
	if err != nil || len(inv) != 1 || inv[0].GitHubLogin != "mona-lisa" || inv[0].Note != "from the waitlist" || inv[0].InvitedBy == nil || *inv[0].InvitedBy != admin.ID {
		t.Fatalf("invites = %+v %v", inv, err)
	}
	if _, err := svc.ListGitHubInvites(ctx, userA, false); err == nil {
		t.Fatal("only site admins list invites")
	}
	if err := svc.WithdrawGitHubInvite(ctx, adminA, "mona-lisa"); err != nil {
		t.Fatal(err)
	}
	if err := svc.WithdrawGitHubInvite(ctx, adminA, "mona-lisa"); err == nil {
		t.Fatal("withdrawing twice must say there is no invite")
	}
}

// Someone installs the App on their own GitHub account before they have a
// vx account. Their first sign-in brings the installation's repositories
// into their personal organization, with their GitHub role.
func TestGitHubSignInBringsInPersonalInstallation(t *testing.T) {
	svc, gh, ctx := signinEnv(t, service.SignupOpen)
	gh.AddUser("Erin")
	gh.AddRepo("Erin/notes", true)
	gh.SetPermission("Erin/notes", "Erin", "admin")
	id := gh.Install("Erin", "Erin/notes")
	if err := svc.SyncInstallation(ctx, id); err != nil {
		t.Fatal(err)
	}
	p, _, isNew, err := signIn(t, svc, gh, "Erin")
	if err != nil || !isNew {
		t.Fatalf("sign in: %v new=%v", err, isNew)
	}
	actor, err := svc.ResolveActor(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	repos, roles, err := svc.ReadableRepos(ctx, actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, r := range repos {
		got = append(got, r.Name+"="+string(roles[i]))
	}
	if strings.Join(got, ",") != "erin/notes=admin" {
		t.Fatalf("repos after first sign-in = %v", got)
	}
}
