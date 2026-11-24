package service_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/oidc"
	"github.com/TerracodeBA/vx/internal/oidc/oidctest"
	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// ssoEnv is a server with a public URL and a secret key, people alice
// (owner of acme) and bob (member), the repository acme/api, and a fake
// identity provider.
type ssoEnv struct {
	t          *testing.T
	ctx        context.Context
	svc        *service.Service
	idp        *oidctest.Provider
	alice, bob service.Actor
}

func newSSOEnv(t *testing.T) *ssoEnv {
	t.Helper()
	ctx := context.Background()
	svc := service.New(storetest.New(t), blob.NewMem(), nil)
	svc.GitHub.PublicURL, svc.GitHub.SecretKey, svc.GitHub.Signup = testPublicURL, bytes.Repeat([]byte{7}, 32), service.SignupInvite
	e := &ssoEnv{t: t, ctx: ctx, svc: svc, idp: oidctest.New(t)}
	e.alice = e.personWithToken("alice", "alice@acme.test")
	e.bob = e.personWithToken("bob", "bob@acme.test")
	if _, err := svc.CreateOrg(ctx, e.alice, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetMember(ctx, e.alice, "acme", "bob", service.OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	return e
}

// personWithToken creates a person and returns them as a request with a
// token (signed in with a token or GitHub, not single sign-on).
func (e *ssoEnv) personWithToken(handle, email string) service.Actor {
	e.t.Helper()
	p, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: handle, Email: email})
	if err != nil {
		e.t.Fatal(err)
	}
	tok, _, err := e.svc.Store.IssueToken(e.ctx, p.ID, "test")
	if err != nil {
		e.t.Fatal(err)
	}
	a, _ := e.svc.ResolveActor(e.ctx, p)
	a.TokenID = tok.ID
	return a
}

// actorFor resolves a token the way the API does.
func (e *ssoEnv) actorFor(token string) service.Actor {
	e.t.Helper()
	p, id, err := e.svc.Store.AuthenticateToken(e.ctx, token)
	if err != nil {
		e.t.Fatalf("token does not authenticate: %v", err)
	}
	a, _ := e.svc.ResolveActor(e.ctx, p)
	a.TokenID = id
	return a
}

func (e *ssoEnv) configure(require bool, domains ...string) error {
	_, err := e.svc.SetOrgSSO(e.ctx, e.alice, "acme", service.SSOConfigInput{
		Issuer: e.idp.Issuer, ClientID: e.idp.ClientID, ClientSecret: e.idp.ClientSecret, Domains: domains, Require: require})
	return err
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// authorize plays the browser at the provider: it returns the code the
// provider sends back, after checking state and PKCE parameters.
func (e *ssoEnv) authorize(st service.SSOStart) string {
	e.t.Helper()
	u, _ := url.Parse(st.URL)
	q := u.Query()
	if q.Get("state") != st.State || q.Get("nonce") != st.Nonce || q.Get("code_challenge") != oidc.Challenge(st.Verifier) ||
		q.Get("redirect_uri") != testPublicURL+service.OIDCCallbackPath || strings.Contains(st.URL, st.Verifier) {
		e.t.Fatalf("authorize URL %s", st.URL)
	}
	resp, err := noRedirect.Get(st.URL)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := resp.Location()
	if loc == nil || loc.Query().Get("state") != st.State {
		e.t.Fatalf("provider answered %d %v", resp.StatusCode, loc)
	}
	return loc.Query().Get("code")
}

// signIn runs a whole single sign-on; link is the signed-in person, if any.
func (e *ssoEnv) signIn(user oidctest.User, link *service.Actor) (service.SSOResult, error) {
	e.t.Helper()
	e.idp.SetUser(user)
	st, err := e.svc.SSOSignInStart(e.ctx, "acme")
	if err != nil {
		e.t.Fatal(err)
	}
	f := service.SSOFlow{Org: st.Org, State: st.State, Nonce: st.Nonce, Verifier: st.Verifier}
	if link != nil {
		f.LinkPerson, f.LinkToken = link.Human().ID, link.TokenID
	}
	return e.svc.SSOSignInFinish(e.ctx, f, e.authorize(st))
}

func user(sub, email string) oidctest.User {
	return oidctest.User{Subject: sub, Email: email, EmailVerified: oidctest.Verified(true), Name: strings.Split(email, "@")[0]}
}

// TestSSOSignInAndRequire is single sign-on end to end at the service:
// owners configure it, a person connects it, members who must use it are
// stopped on repositories until they do, new people are created and join
// the organization, and tokens made from a single sign-on session carry
// it over.
func TestSSOSignInAndRequire(t *testing.T) {
	e := newSSOEnv(t)
	ctx, svc := e.ctx, e.svc

	// Only owners configure; requiring needs one sign-in through it first.
	if _, err := svc.SetOrgSSO(ctx, e.bob, "acme", service.SSOConfigInput{Issuer: e.idp.Issuer, ClientID: "x", ClientSecret: "y"}); code(err) != service.CodePermission {
		t.Fatalf("member configures: %v", err)
	}
	if err := e.configure(true, "acme.test"); code(err) != service.CodeFailedPrecondition {
		t.Fatalf("require before a first sign-in: %v", err)
	}
	if _, err := svc.SetOrgSSO(ctx, e.alice, "acme", service.SSOConfigInput{Issuer: "https://nope.invalid", ClientID: "x", ClientSecret: "y"}); code(err) != service.CodeInvalid {
		t.Fatalf("bad issuer: %v", err)
	}
	if err := e.configure(false, "@Acme.test, acme.test"); err != nil {
		t.Fatal(err)
	}
	cfg, ok, err := svc.OrgSSO(ctx, e.bob, "acme")
	if err != nil || !ok || cfg.Issuer != e.idp.Issuer || strings.Join(cfg.Domains, ",") != "acme.test" || cfg.CallbackURL != testPublicURL+"/auth/oidc/callback" {
		t.Fatalf("config = %+v %v %v", cfg, ok, err)
	}

	// Alice connects her identity while signed in; her session is marked.
	res, err := e.signIn(user("alice-sub", "alice@acme.test"), &e.alice)
	if err != nil || !res.Linked || res.Token != "" || res.Person.ID != e.alice.ID {
		t.Fatalf("alice connects: %+v %v", res, err)
	}
	if err := e.configure(true, "acme.test"); err != nil {
		t.Fatalf("require after signing in through it: %v", err)
	}

	// Bob, a member, is stopped until he signs in through it.
	_, _, err = svc.Authorize(ctx, e.bob, "acme/api", service.RoleRead)
	var req *service.SSORequiredError
	if !errors.As(err, &req) || req.Org != "acme" || code(err) != service.CodePermission || !strings.Contains(err.Error(), "vx login --server "+testPublicURL) {
		t.Fatalf("bob without single sign-on: %v", err)
	}
	// The owner is exempt (a broken provider cannot lock everyone out);
	// calls without a token (the server itself) too.
	owner := e.personWithToken("olga", "olga@acme.test")
	if _, err := svc.SetMember(ctx, e.alice, "acme", "olga", service.OrgOwner); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authorize(ctx, owner, "acme/api", service.RoleRead); err != nil {
		t.Fatalf("owner: %v", err)
	}
	// A superadmin without a role still gets "not found" (#33), never a
	// single sign-on prompt that would reveal the repository.
	root := e.personWithToken("root", "root@vx.test")
	if _, _, err := svc.ConsoleSetSuperadmin(ctx, "root", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authorize(ctx, root, "acme/api", service.RoleRead); code(err) != service.CodeNotFound {
		t.Fatalf("superadmin: %v", err)
	}
	internal := e.bob
	internal.TokenID = [16]byte{}
	if _, _, err := svc.Authorize(ctx, internal, "acme/api", service.RoleRead); err != nil {
		t.Fatalf("no token: %v", err)
	}

	// Bob's first sign-in without a session: his email has an account, so
	// vx asks him to sign in to it and connect instead of making another.
	_, err = e.signIn(user("bob-sub", "Bob@acme.test"), nil)
	var exists *service.AccountExistsError
	if !errors.As(err, &exists) || exists.Handle != "bob" || exists.Org != "acme" || code(err) != service.CodeExists {
		t.Fatalf("bob without a session: %v", err)
	}
	if res, err := e.signIn(user("bob-sub", "bob@acme.test"), &e.bob); err != nil || !res.Linked {
		t.Fatalf("bob connects: %+v %v", res, err)
	}
	if _, _, err := svc.Authorize(ctx, e.bob, "acme/api", service.RoleRead); err != nil {
		t.Fatalf("bob after single sign-on: %v", err)
	}
	// Next time bob signs in without a session he gets a marked session.
	res, err = e.signIn(user("bob-sub", "bob@acme.test"), nil)
	if err != nil || res.Person.ID != e.bob.ID || res.Token == "" || res.IsNew {
		t.Fatalf("bob signs in: %+v %v", res, err)
	}
	bobSSO := e.actorFor(res.Token)
	if _, _, err := svc.Authorize(ctx, bobSSO, "acme/api", service.RoleRead); err != nil {
		t.Fatalf("bob's single sign-on session: %v", err)
	}

	// vx ui: the browser session it opens inherits the CLI token's mark.
	loginCode, _, err := svc.CreateLoginCode(ctx, bobSSO)
	if err != nil {
		t.Fatal(err)
	}
	if _, webTok, err := svc.RedeemLoginCode(ctx, loginCode); err != nil {
		t.Fatal(err)
	} else if _, _, err := svc.Authorize(ctx, e.actorFor(webTok), "acme/api", service.RoleRead); err != nil {
		t.Fatalf("vx ui session from a single sign-on token: %v", err)
	}

	// Tokens made from a marked session inherit it; others do not.
	_, agentTok, err := svc.CreateAgent(ctx, bobSSO, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authorize(ctx, e.actorFor(agentTok), "acme/api", service.RoleRead); err != nil {
		t.Fatalf("agent made from a single sign-on session: %v", err)
	}
	_, plainBobTok, err := svc.Store.IssueToken(ctx, e.bob.ID, "another session, not single sign-on")
	if err != nil {
		t.Fatal(err)
	}
	_, _, plainAgentTok, err := svc.CreateToken(ctx, e.actorFor(plainBobTok), "", "plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authorize(ctx, e.actorFor(plainAgentTok), "acme/api", service.RoleRead); !errors.As(err, &req) {
		t.Fatalf("token made from a plain session: %v", err)
	}
	_, _, cliTok, err := svc.CreateToken(ctx, bobSSO, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if a := e.actorFor(cliTok); a.Kind != store.KindAgent {
		t.Fatalf("agent token is for %s", a.Ref())
	} else if _, _, err := svc.Authorize(ctx, a, "acme/api", service.RoleRead); err != nil {
		t.Fatalf("agent token from a single sign-on session: %v", err)
	}

	// Someone new: an account, membership in acme, a marked session.
	res, err = e.signIn(user("carol-sub", "carol@acme.test"), nil)
	if err != nil || !res.IsNew || res.Person.Handle != "carol" || res.Person.Email != "carol@acme.test" || res.Token == "" {
		t.Fatalf("carol signs up: %+v %v", res, err)
	}
	if role, err := svc.Store.OrgRole(ctx, mustOrg(t, svc, "acme").ID, res.Person.ID); err != nil || role != "member" {
		t.Fatalf("carol's role in acme = %q %v", role, err)
	}
	if _, _, err := svc.Authorize(ctx, e.actorFor(res.Token), "acme/api", service.RoleRead); err != nil {
		t.Fatalf("carol: %v", err)
	}
	// A second sign-in finds her by (issuer, subject), even with a new email.
	if res2, err := e.signIn(user("carol-sub", "carol.c@acme.test"), nil); err != nil || res2.Person.ID != res.Person.ID || res2.IsNew {
		t.Fatalf("carol again: %+v %v", res2, err)
	}
	// Someone else's identity cannot be connected to bob.
	if _, err := e.signIn(user("carol-sub", "carol@acme.test"), &e.bob); code(err) != service.CodeExists {
		t.Fatalf("carol's identity onto bob: %v", err)
	}
}

// TestSSOTokenChecks: what the provider sends is checked: signatures,
// audience, expiry, rotated keys, email rules, and a nonce works once.
func TestSSOTokenChecks(t *testing.T) {
	e := newSSOEnv(t)
	if err := e.configure(false, "acme.test"); err != nil {
		t.Fatal(err)
	}
	dave := user("dave-sub", "dave@acme.test")
	refused := func(name, want string) {
		t.Helper()
		if _, err := e.signIn(dave, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	e.idp.BadSignature(true)
	refused("bad signature", "signature does not verify")
	e.idp.BadSignature(false)
	e.idp.Tamper(func(c map[string]any) { c["aud"] = "another-client" })
	refused("wrong audience", "for another client")
	e.idp.Tamper(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() })
	refused("expired", "has expired")
	e.idp.Tamper(func(c map[string]any) { c["iss"] = "https://evil.example" })
	refused("other issuer", "another issuer")
	e.idp.Tamper(nil)

	// Email rules.
	if _, err := e.signIn(user("x-sub", "x@other.test"), nil); err == nil || !strings.Contains(err.Error(), "only signs in acme.test addresses") {
		t.Errorf("other domain: %v", err)
	}
	unverified := user("u-sub", "u@acme.test")
	unverified.EmailVerified = oidctest.Verified(false)
	if _, err := e.signIn(unverified, nil); err == nil || !strings.Contains(err.Error(), "is not verified") {
		t.Errorf("unverified: %v", err)
	}
	if _, err := e.signIn(oidctest.User{Subject: "n-sub"}, nil); err == nil || !strings.Contains(err.Error(), "sent no email") {
		t.Errorf("no email: %v", err)
	}
	// Without email_verified the email counts only within named domains.
	silent := user("s-sub", "s@acme.test")
	silent.EmailVerified = nil
	if _, err := e.signIn(silent, nil); err != nil {
		t.Errorf("silent provider within the domains: %v", err)
	}
	if err := e.configure(false); err != nil {
		t.Fatal(err)
	}
	silent.Subject = "s2-sub"
	if _, err := e.signIn(silent, nil); err == nil || !strings.Contains(err.Error(), "does not say whether") {
		t.Errorf("silent provider without domains: %v", err)
	}
	if err := e.configure(false, "acme.test"); err != nil {
		t.Fatal(err)
	}

	// Key rotation: new keys work without a restart.
	if _, err := e.signIn(dave, nil); err != nil {
		t.Fatalf("dave: %v", err)
	}
	e.idp.Rotate(false)
	if res, err := e.signIn(dave, nil); err != nil || res.IsNew {
		t.Fatalf("after key rotation: %+v %v", res, err)
	}

	// Nonce replay: two codes for one started sign-in (same nonce); the
	// second ID token is refused.
	e.idp.SetUser(dave)
	st, err := e.svc.SSOSignInStart(e.ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	first, second := e.authorize(st), e.authorize(st)
	f := service.SSOFlow{Org: st.Org, State: st.State, Nonce: st.Nonce, Verifier: st.Verifier}
	if _, err := e.svc.SSOSignInFinish(e.ctx, f, first); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SSOSignInFinish(e.ctx, f, second); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("replayed nonce: %v", err)
	}
	// A code is good once at the provider too.
	if _, err := e.svc.SSOSignInFinish(e.ctx, f, first); err == nil || !strings.Contains(err.Error(), "did not accept this sign-in") {
		t.Fatalf("reused code: %v", err)
	}
	// A wrong verifier (PKCE) is refused by the provider.
	st, _ = e.svc.SSOSignInStart(e.ctx, "acme")
	f = service.SSOFlow{Org: st.Org, State: st.State, Nonce: st.Nonce, Verifier: "not-the-verifier"}
	if _, err := e.svc.SSOSignInFinish(e.ctx, f, e.authorize(st)); err == nil || !strings.Contains(err.Error(), "PKCE") {
		t.Fatalf("wrong verifier: %v", err)
	}

	// Sign-up closed: nobody new, people with accounts still sign in.
	e.svc.GitHub.Signup = service.SignupClosed
	if _, err := e.signIn(user("new-sub", "new@acme.test"), nil); err == nil || !strings.Contains(err.Error(), "does not take new accounts") {
		t.Fatalf("closed sign-up: %v", err)
	}
	if _, err := e.signIn(dave, nil); err != nil {
		t.Fatalf("dave with sign-up closed: %v", err)
	}
}

// TestSSOOrgFor: the sign-in page finds the organization by email domain
// or handle.
func TestSSOOrgFor(t *testing.T) {
	e := newSSOEnv(t)
	if _, err := e.svc.SSOOrgFor(e.ctx, "alice@acme.test"); code(err) != service.CodeNotFound {
		t.Fatalf("before configuring: %v", err)
	}
	if err := e.configure(false, "acme.test"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"alice@acme.test", "ACME", " acme "} {
		if org, err := e.svc.SSOOrgFor(e.ctx, who); err != nil || org != "acme" {
			t.Errorf("SSOOrgFor(%q) = %q %v", who, org, err)
		}
	}
	if _, err := e.svc.SSOOrgFor(e.ctx, "someone@else.test"); code(err) != service.CodeNotFound {
		t.Errorf("unknown domain: %v", err)
	}
	if err := e.svc.RemoveOrgSSO(e.ctx, e.bob, "acme"); code(err) != service.CodePermission {
		t.Errorf("member removes: %v", err)
	}
	if err := e.svc.RemoveOrgSSO(e.ctx, e.alice, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SSOOrgFor(e.ctx, "acme"); code(err) != service.CodeNotFound {
		t.Errorf("after removing: %v", err)
	}
}

// TestSSOSignupRateLimited: creating accounts through single sign-on takes
// from the same sign-up rate as GitHub (r3-ops' AllowSignup); people who
// already have an account still sign in when it is spent.
func TestSSOSignupRateLimited(t *testing.T) {
	e := newSSOEnv(t)
	if err := e.configure(false, "acme.test"); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	l := ratelimit.New(ratelimit.Config{ratelimit.SignUp: {PerSecond: 1.0 / 60, Burst: 1}})
	l.Now = func() time.Time { return now }
	e.svc.Limits = l
	e.ctx = service.WithClientIP(e.ctx, "203.0.113.9")
	first, err := e.signIn(user("one-sub", "one@acme.test"), nil)
	if err != nil || !first.IsNew {
		t.Fatalf("first sign-up: %+v %v", first, err)
	}
	_, err = e.signIn(user("two-sub", "two@acme.test"), nil)
	if code(err) != service.CodeTooMany || !strings.Contains(err.Error(), "slow down: too many new accounts from your address") {
		t.Fatalf("second sign-up from the same address: %v", err)
	}
	if _, err := e.svc.Store.PrincipalByHandle(e.ctx, "two"); err == nil {
		t.Fatal("a refused sign-up created the person")
	}
	if again, err := e.signIn(user("one-sub", "one@acme.test"), nil); err != nil || again.IsNew {
		t.Fatalf("existing person while the rate is spent: %+v %v", again, err)
	}
}
