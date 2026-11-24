package service

// Single sign-on through an organization's identity provider (OpenID
// Connect: Google Workspace, Okta, Microsoft Entra ID, or any issuer with
// discovery).
//
//   - Owners configure the issuer, client id and secret (sealed with the
//     server key), the email domains it may sign in, and whether members
//     must use it.
//   - Sign-in is the authorization code flow with PKCE; the ID token is
//     verified (JWKS signature, issuer, audience, expiry, nonce) and its
//     nonce can be used once.
//   - People are linked by (issuer, subject). A first sign-in with an email
//     that already belongs to someone asks them to sign in to that account
//     and connect instead of creating a second one.
//   - A token remembers which organizations' sign-on it came through
//     (store.token_sso); tokens made from it inherit that. When an
//     organization requires single sign-on, its members and admins (owners
//     are exempt, so a broken provider cannot lock everyone out) reach its
//     repositories only with such a token: Authorize calls requireSSO.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/oidc"
	"github.com/TerracodeBA/vx/internal/secret"
	"github.com/TerracodeBA/vx/internal/store"
)

// OIDCCallbackPath is where identity providers send the browser back; it
// is the redirect URI to register with the provider.
const OIDCCallbackPath = "/auth/oidc/callback"

// oidcClient talks to identity providers; it caches discovery documents
// and signing keys by issuer.
var oidcClient = &oidc.Client{}

const ssoSecretPurpose = "org sso client secret"

// SSOConfig is an organization's single sign-on as shown to its members
// (never the client secret).
type SSOConfig struct {
	Org         string
	Issuer      string
	ClientID    string
	Domains     []string
	Require     bool
	UpdatedAt   time.Time
	UpdatedBy   string // ref, when known
	CallbackURL string // the redirect URI to register with the provider
}

// SSOConfigInput is what an owner sets. An empty ClientSecret keeps the
// saved one.
type SSOConfigInput struct {
	Issuer, ClientID, ClientSecret string
	Domains                        []string
	Require                        bool
}

// SSORequiredError says an organization requires single sign-on and the
// request's token did not come through it.
type SSORequiredError struct {
	Org string // handle
}

func (e *SSORequiredError) Error() string {
	return "organization " + e.Org + " requires single sign-on"
}

// AccountExistsError is returned when a first sign-in (with GitHub or an
// organization's single sign-on) is for an email that already belongs to
// a person: they sign in to that account and connect instead of getting a
// second one.
type AccountExistsError struct {
	Email  string
	Handle string // the existing person
	Via    string // "GitHub" or "ORG single sign-on"
	Org    string // the organization, for single sign-on
	// GitHubLogin is the GitHub account the existing person signs in
	// with, when they have one.
	GitHubLogin string
}

func (e *AccountExistsError) Error() string {
	return fmt.Sprintf("an account with %s already exists here (%s): sign in to it first, then connect %s to it, so you do not end up with two accounts", e.Email, "user:"+e.Handle, e.Via)
}

// SSOCallbackURL is the redirect URI to register with identity providers.
func (s *Service) SSOCallbackURL() (string, error) {
	base := s.publicURL()
	if base == "" {
		return "", errf(CodeUnavailable, "this server does not know its public address, so an identity provider cannot send you back to it: the server admin sets VX_PUBLIC_URL (for example https://vx.example.com)")
	}
	return base + OIDCCallbackPath, nil
}

func (s *Service) ssoView(ctx context.Context, org store.Org, c store.OrgSSO) SSOConfig {
	v := SSOConfig{Org: org.Handle, Issuer: c.Issuer, ClientID: c.ClientID, Domains: c.AllowedDomains, Require: c.RequireSSO, UpdatedAt: c.UpdatedAt}
	if c.UpdatedBy != nil {
		if p, err := s.Store.PrincipalByID(ctx, *c.UpdatedBy); err == nil {
			v.UpdatedBy = p.Ref()
		}
	}
	v.CallbackURL, _ = s.SSOCallbackURL()
	return v
}

// OrgSSO returns an organization's single sign-on for its members; ok is
// false when it has none.
func (s *Service) OrgSSO(ctx context.Context, actor Actor, org string) (SSOConfig, bool, error) {
	a, err := s.loadOrg(ctx, nil, actor, org)
	if err != nil {
		return SSOConfig{}, false, err
	}
	c, ok, err := s.Store.OrgSSOOf(ctx, a.org.ID)
	if err != nil || !ok {
		return SSOConfig{}, false, classify(err)
	}
	return s.ssoView(ctx, a.org, c), true, nil
}

// ssoOwner loads an organization for changing its single sign-on: owners
// only (not admins, not superadmins: the provider decides who can reach
// the organization's repositories).
func (s *Service) ssoOwner(ctx context.Context, tx *store.Tx, actor Actor, org string) (store.Org, error) {
	if err := peopleOnly(actor, "change single sign-on"); err != nil {
		return store.Org{}, err
	}
	a, err := s.loadOrg(ctx, tx, actor, org)
	if err != nil {
		return store.Org{}, err
	}
	if a.role != OrgOwner {
		if a.role == "" {
			return store.Org{}, orgNotFound(org)
		}
		return store.Org{}, errf(CodePermission, "only owners of organization %s change its single sign-on; %s is a %s. Ask an owner (%s)",
			a.org.Handle, actor.Human().Ref(), a.role, s.orgOwnerRefs(ctx, a.org.ID))
	}
	if a.org.Personal {
		return store.Org{}, errf(CodeInvalid, "%s is a personal organization; single sign-on is for team organizations (vx org create)", a.org.Handle)
	}
	return a.org, nil
}

// normalizeDomains lowercases and checks email domains ("acme.com",
// "@acme.com" and "acme.com, acme.de" all work).
func normalizeDomains(in []string) ([]string, error) {
	var out []string
	for _, d := range in {
		for _, part := range strings.FieldsFunc(d, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
			part = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(part)), "@")
			if part == "" {
				continue
			}
			if !strings.Contains(part, ".") || strings.ContainsAny(part, "/:@") || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") {
				return nil, errf(CodeInvalid, "%q is not an email domain; use domains like acme.com", part)
			}
			if !slices.Contains(out, part) {
				out = append(out, part)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// SetOrgSSO saves an organization's single sign-on (owners). vx reads the
// provider's discovery document first, so a mistyped issuer fails here and
// not at the next sign-in. Requiring single sign-on needs the owner to
// have signed in through it once, so they know it works.
func (s *Service) SetOrgSSO(ctx context.Context, actor Actor, org string, in SSOConfigInput) (SSOConfig, error) {
	key := s.GitHub.SecretKey
	if len(key) == 0 {
		return SSOConfig{}, errf(CodeUnavailable, "this server has no key to keep the client secret encrypted: the server admin sets VX_SECRET_KEY (see docs/deploy.md), then you can set up single sign-on")
	}
	if _, err := s.SSOCallbackURL(); err != nil {
		return SSOConfig{}, err
	}
	issuer := oidc.NormalizeIssuer(in.Issuer)
	clientID := strings.TrimSpace(in.ClientID)
	if issuer == "" || clientID == "" {
		return SSOConfig{}, errf(CodeInvalid, "single sign-on needs the provider's issuer URL and the client id of the app you registered for vx there")
	}
	if err := oidc.CheckIssuerURL(issuer); err != nil {
		return SSOConfig{}, errf(CodeInvalid, "%v", err)
	}
	domains, err := normalizeDomains(in.Domains)
	if err != nil {
		return SSOConfig{}, err
	}
	if _, err := oidcClient.Discover(ctx, issuer); err != nil {
		return SSOConfig{}, &Error{Code: CodeInvalid, Err: err, Msg: "could not use " + issuer + " as an OpenID Connect provider: " + err.Error()}
	}
	var out store.OrgSSO
	var o store.Org
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if o, err = s.ssoOwner(ctx, tx, actor, org); err != nil {
			return err
		}
		old, had, err := tx.OrgSSOOf(ctx, o.ID)
		if err != nil {
			return err
		}
		enc := old.ClientSecretEnc
		if strings.TrimSpace(in.ClientSecret) != "" {
			if enc, err = secret.SealString(key, ssoSecretPurpose, strings.TrimSpace(in.ClientSecret)); err != nil {
				return err
			}
		} else if !had || old.ClientID != clientID || old.Issuer != issuer {
			return errf(CodeInvalid, "enter the client secret of the app you registered for vx at your provider")
		}
		if in.Require && (!had || old.Issuer != issuer || !s.signedInThrough(ctx, tx, actor, o.ID)) {
			return errf(CodeFailedPrecondition, "sign in through %s's single sign-on once before requiring it, so you know it works: save without \"require\", choose Sign in with single sign-on on this page, then require it", o.Handle)
		}
		if had && old.Issuer != issuer {
			// Another provider: earlier sign-ins no longer count.
			if err := tx.ForgetOrgSSOProofs(ctx, o.ID); err != nil {
				return err
			}
		}
		if out, err = tx.PutOrgSSO(ctx, store.OrgSSO{OrgID: o.ID, Issuer: issuer, ClientID: clientID, ClientSecretEnc: enc,
			AllowedDomains: domains, RequireSSO: in.Require, UpdatedBy: &actor.Principal.ID}); err != nil {
			return err
		}
		var before any
		if had {
			before = map[string]any{"issuer": old.Issuer, "client_id": old.ClientID, "domains": old.AllowedDomains, "require": old.RequireSSO}
		}
		return tx.LogOp(ctx, nil, &actor.ID, "org.sso_update", o.Handle, before,
			map[string]any{"issuer": issuer, "client_id": clientID, "domains": domains, "require": in.Require, "secret_changed": in.ClientSecret != ""})
	})
	if err != nil {
		return SSOConfig{}, classify(err)
	}
	return s.ssoView(ctx, o, out), nil
}

// signedInThrough reports whether the actor's current token came through
// org's single sign-on.
func (s *Service) signedInThrough(ctx context.Context, q *store.Tx, actor Actor, org uuid.UUID) bool {
	if actor.TokenID == uuid.Nil {
		return false
	}
	orgs, err := q.TokenSSOOrgs(ctx, actor.TokenID)
	return err == nil && slices.Contains(orgs, org)
}

// RemoveOrgSSO turns an organization's single sign-on off (owners).
// Linked identities stay, so turning it on again keeps people's accounts.
func (s *Service) RemoveOrgSSO(ctx context.Context, actor Actor, org string) error {
	return classify(s.Store.InTx(ctx, func(tx *store.Tx) error {
		o, err := s.ssoOwner(ctx, tx, actor, org)
		if err != nil {
			return err
		}
		ok, err := tx.DeleteOrgSSO(ctx, o.ID)
		if err != nil {
			return err
		}
		if !ok {
			return errf(CodeNotFound, "organization %s has no single sign-on to turn off", o.Handle)
		}
		return tx.LogOp(ctx, nil, &actor.ID, "org.sso_remove", o.Handle, nil, nil)
	}))
}

// SSOOrgFor finds the organization a person means on the sign-in page: an
// organization handle, or a work email whose domain one organization's
// single sign-on allows.
func (s *Service) SSOOrgFor(ctx context.Context, who string) (string, error) {
	who = strings.ToLower(strings.TrimSpace(who))
	if who == "" {
		return "", errf(CodeInvalid, "enter your work email or your organization's handle")
	}
	if _, domain, ok := strings.Cut(who, "@"); ok {
		orgs, err := s.Store.OrgsBySSODomain(ctx, domain)
		if err != nil {
			return "", err
		}
		switch len(orgs) {
		case 0:
			return "", errf(CodeNotFound, "no organization on this server signs in %s addresses with single sign-on; sign in with GitHub or a token, or enter your organization's handle", domain)
		case 1:
			return orgs[0].Handle, nil
		}
		return "", errf(CodeInvalid, "several organizations sign in %s addresses; enter your organization's handle instead", domain)
	}
	o, err := s.Store.OrgByHandle(ctx, who)
	if err == nil {
		if _, ok, err := s.Store.OrgSSOOf(ctx, o.ID); err == nil && ok {
			return o.Handle, nil
		}
	}
	return "", errf(CodeNotFound, "organization %s has no single sign-on here (or does not exist); check the handle, or sign in with GitHub or a token", who)
}

// SSOStart is a started single sign-on: where to send the browser and what
// the web layer keeps in its short-lived cookie.
type SSOStart struct {
	URL                    string
	Org                    string
	State, Nonce, Verifier string
}

// SSOSignInStart begins signing in through org's provider.
func (s *Service) SSOSignInStart(ctx context.Context, org string) (SSOStart, error) {
	redirect, err := s.SSOCallbackURL()
	if err != nil {
		return SSOStart{}, err
	}
	o, c, err := s.ssoOf(ctx, org)
	if err != nil {
		return SSOStart{}, err
	}
	m, err := oidcClient.Discover(ctx, c.Issuer)
	if err != nil {
		return SSOStart{}, &Error{Code: CodeUnavailable, Err: err, Msg: "could not reach " + o.Handle + "'s identity provider: " + err.Error() + ". Try again in a moment, or ask an owner of " + o.Handle + " to check its single sign-on"}
	}
	st := SSOStart{Org: o.Handle}
	for _, p := range []*string{&st.State, &st.Nonce, &st.Verifier} {
		if *p, err = randomToken(32); err != nil {
			return SSOStart{}, err
		}
	}
	st.URL = oidc.AuthorizeURL(m, c.ClientID, redirect, st.State, st.Nonce, oidc.Challenge(st.Verifier))
	return st, nil
}

func (s *Service) ssoOf(ctx context.Context, org string) (store.Org, store.OrgSSO, error) {
	o, err := s.Store.OrgByHandle(ctx, strings.ToLower(strings.TrimSpace(org)))
	if err != nil {
		return store.Org{}, store.OrgSSO{}, errf(CodeNotFound, "organization %s has no single sign-on here (or does not exist)", org)
	}
	c, ok, err := s.Store.OrgSSOOf(ctx, o.ID)
	if err != nil {
		return store.Org{}, store.OrgSSO{}, err
	}
	if !ok {
		return store.Org{}, store.OrgSSO{}, errf(CodeNotFound, "organization %s has no single sign-on here (or does not exist)", o.Handle)
	}
	return o, c, nil
}

// SSOFlow is what the web layer kept between start and callback.
type SSOFlow struct {
	Org, State, Nonce, Verifier string
	// LinkPerson and LinkToken are set when a signed-in person signs in
	// through the provider: the identity is connected to them and their
	// current session is marked, instead of starting a new one.
	LinkPerson, LinkToken uuid.UUID
}

// SSOResult is a finished single sign-on.
type SSOResult struct {
	Person store.Principal
	Org    string
	// Token is a new web session; empty when the flow marked the signed-in
	// person's session instead.
	Token  string
	IsNew  bool // the account was created now
	Linked bool // the identity was connected to an existing person now
}

// SSOSignInFinish completes a sign-in through org's provider: it exchanges
// the code, verifies the ID token, uses up its nonce, checks the email
// rules, finds, connects or creates the person, makes them a member of the
// organization, and returns a web session marked as signed in through it.
func (s *Service) SSOSignInFinish(ctx context.Context, f SSOFlow, code string) (SSOResult, error) {
	if strings.TrimSpace(code) == "" || f.Verifier == "" || f.Nonce == "" {
		return SSOResult{}, errf(CodeInvalid, "the identity provider sent no sign-in code back; start the sign-in again")
	}
	redirect, err := s.SSOCallbackURL()
	if err != nil {
		return SSOResult{}, err
	}
	o, c, err := s.ssoOf(ctx, f.Org)
	if err != nil {
		return SSOResult{}, err
	}
	clientSecret, err := secret.OpenString(s.GitHub.SecretKey, ssoSecretPurpose, c.ClientSecretEnc)
	if err != nil {
		return SSOResult{}, &Error{Code: CodeUnavailable, Err: err, Msg: "the client secret of " + o.Handle + "'s single sign-on cannot be read with this server's key (was VX_SECRET_KEY changed?); an owner of " + o.Handle + " enters it again"}
	}
	m, err := oidcClient.Discover(ctx, c.Issuer)
	if err != nil {
		return SSOResult{}, &Error{Code: CodeUnavailable, Err: err, Msg: "could not reach " + o.Handle + "'s identity provider: " + err.Error()}
	}
	raw, err := oidcClient.Exchange(ctx, m, c.ClientID, clientSecret, code, redirect, f.Verifier)
	if err != nil {
		return SSOResult{}, &Error{Code: CodeInvalid, Err: err, Msg: o.Handle + "'s identity provider did not accept this sign-in (" + err.Error() + "): the code may have expired or been used already. Start the sign-in again"}
	}
	claims, err := oidcClient.Verify(ctx, m, raw, oidc.Expected{Issuer: c.Issuer, ClientID: c.ClientID, Nonce: f.Nonce})
	if err != nil {
		slog.Default().Warn("sso: ID token refused", "org", o.Handle, "issuer", c.Issuer, "err", err)
		return SSOResult{}, &Error{Code: CodePermission, Err: err, Msg: "vx could not verify the sign-in from " + o.Handle + "'s identity provider (" + err.Error() + "), so nobody was signed in. Start again; if it keeps happening, an owner of " + o.Handle + " checks the issuer and client id"}
	}
	fresh, err := s.Store.UseNonce(ctx, c.Issuer+"\x00"+c.ClientID+"\x00"+claims.Nonce, claims.Expiry.Add(oidc.Skew))
	if err != nil {
		return SSOResult{}, err
	}
	if !fresh {
		return SSOResult{}, errf(CodePermission, "this sign-in was already used; start the sign-in again")
	}
	email, err := ssoEmail(o.Handle, c, claims)
	if err != nil {
		return SSOResult{}, err
	}
	res := SSOResult{Org: o.Handle}
	link := f.LinkPerson != uuid.Nil
	p, err := s.Store.PersonByIdentity(ctx, c.Issuer, claims.Subject)
	switch {
	case err == nil:
		if link && p.ID != f.LinkPerson {
			return SSOResult{}, errf(CodeExists, "this %s sign-in (%s) is already connected to %s; sign out and sign in with single sign-on to use that account", o.Handle, email, p.Ref())
		}
		_ = s.Store.TouchIdentity(ctx, c.Issuer, claims.Subject, email)
	case !errors.Is(err, store.ErrNotFound):
		return SSOResult{}, err
	case link:
		if p, err = s.Store.PrincipalByID(ctx, f.LinkPerson); err != nil {
			return SSOResult{}, classify(err)
		}
		err := s.Store.InTx(ctx, func(tx *store.Tx) error {
			if err := tx.LinkIdentity(ctx, p.ID, c.Issuer, claims.Subject, email); err != nil {
				if errors.Is(err, store.ErrExists) {
					return errf(CodeExists, "%s already has another %s identity connected; vx keeps one identity per person and provider. Sign in with that one", p.Ref(), o.Handle)
				}
				return err
			}
			return tx.LogOp(ctx, nil, &p.ID, "person.sso_link", p.Ref(), nil, map[string]any{"org": o.Handle, "issuer": c.Issuer, "subject": claims.Subject, "email": email})
		})
		if err != nil {
			return SSOResult{}, classify(err)
		}
		res.Linked = true
	default:
		if existing, err := s.Store.PeopleByEmail(ctx, email); err != nil {
			return SSOResult{}, err
		} else if len(existing) > 0 {
			e := &AccountExistsError{Email: email, Handle: existing[0].Handle, Via: o.Handle + " single sign-on", Org: o.Handle}
			if l, ok, _ := s.Store.GitHubLinkOf(ctx, existing[0].ID); ok {
				e.GitHubLogin = l.Login
			}
			return SSOResult{}, &Error{Code: CodeExists, Err: e, Msg: e.Error()}
		}
		if p, err = s.ssoSignup(ctx, o, c, claims, email); err != nil {
			return SSOResult{}, err
		}
		res.IsNew = true
	}
	res.Person = p
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		role, err := tx.OrgRole(ctx, o.ID, p.ID)
		if err != nil {
			return err
		}
		if role == "" {
			// The provider vouches for them: they join as a member.
			if err := tx.SetOrgMember(ctx, o.ID, p.ID, string(OrgMember)); err != nil {
				return err
			}
			if err := tx.LogOp(ctx, nil, &p.ID, "org.member_join", o.Handle, nil, map[string]any{"person": p.Ref(), "role": OrgMember, "via": "single sign-on"}); err != nil {
				return err
			}
		}
		if link {
			owner, active, err := tx.TokenOwner(ctx, f.LinkToken)
			if err != nil || !active || owner != p.ID {
				return errf(CodePermission, "your vx session ended while you were signing in; sign in again")
			}
			if err := tx.MarkTokenSSO(ctx, f.LinkToken, o.ID); err != nil {
				return err
			}
		} else {
			tok, secretTok, err := tx.IssueToken(ctx, p.ID, "web session ("+o.Handle+" single sign-on)")
			if err != nil {
				return err
			}
			if err := tx.MarkTokenSSO(ctx, tok.ID, o.ID); err != nil {
				return err
			}
			res.Token = secretTok
		}
		if res.IsNew {
			return nil // person.signup is logged with the person
		}
		return tx.LogOp(ctx, nil, &p.ID, "person.sso_signin", p.Ref(), nil, map[string]any{"org": o.Handle, "issuer": c.Issuer, "email": email})
	})
	if err != nil {
		return SSOResult{}, classify(err)
	}
	return res, nil
}

// ssoEmail applies an organization's email rules to a verified ID token.
func ssoEmail(org string, c store.OrgSSO, cl oidc.Claims) (string, error) {
	email := strings.ToLower(strings.TrimSpace(cl.Email))
	if email == "" {
		return "", errf(CodePermission, "%s's identity provider sent no email address for you; an owner of %s allows the email scope for vx's app there", org, org)
	}
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, "@") {
		return "", errf(CodePermission, "%s's identity provider sent %q, which is not an email address", org, cl.Email)
	}
	if cl.EmailVerified != nil && !*cl.EmailVerified {
		return "", errf(CodePermission, "%s's identity provider says %s is not verified; verify it there, then sign in again", org, email)
	}
	_, domain, _ := strings.Cut(email, "@")
	if len(c.AllowedDomains) > 0 && !slices.Contains(c.AllowedDomains, domain) {
		return "", errf(CodePermission, "%s's single sign-on only signs in %s addresses, and yours is %s; sign in with your work account", org, strings.Join(c.AllowedDomains, ", "), email)
	}
	if cl.EmailVerified == nil && len(c.AllowedDomains) == 0 {
		// Some providers (Microsoft Entra ID) do not say whether the email
		// is verified; trust it only for domains the owners named.
		return "", errf(CodePermission, "%s's identity provider does not say whether %s is verified; an owner of %s lists the organization's email domains in its single sign-on settings", org, email, org)
	}
	return email, nil
}

// ssoSignup creates a person for a first single sign-on (unless sign-up is
// closed on this server): handle from the email, the provider's name.
func (s *Service) ssoSignup(ctx context.Context, o store.Org, c store.OrgSSO, cl oidc.Claims, email string) (store.Principal, error) {
	if s.SignupModeOrDefault() == SignupClosed {
		return store.Principal{}, errf(CodePermission, "this vx server does not take new accounts; ask a superadmin to add you, then sign in with single sign-on")
	}
	if err := s.AllowSignup(ctx); err != nil {
		return store.Principal{}, err
	}
	local, _, _ := strings.Cut(email, "@")
	base := GitHubHandle(local, 0)
	if base == "github-0" {
		base = "user"
	}
	name := cl.Name
	if name == "" {
		name = local
	}
	for n := 1; n <= 100; n++ {
		handle := handleWithSuffix(base, n)
		taken, err := s.Store.HandleTaken(ctx, handle)
		if err != nil {
			return store.Principal{}, err
		}
		if taken {
			continue
		}
		var p store.Principal
		err = s.Store.InTx(ctx, func(tx *store.Tx) error {
			var err error
			if p, err = tx.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: handle, DisplayName: name, Email: email}); err != nil {
				return err
			}
			if err := tx.LinkIdentity(ctx, p.ID, c.Issuer, cl.Subject, email); err != nil {
				return err
			}
			return tx.LogOp(ctx, nil, &p.ID, "person.signup", p.Ref(), nil, map[string]any{"via": "single sign-on", "org": o.Handle, "issuer": c.Issuer, "subject": cl.Subject})
		})
		switch {
		case err == nil:
			return p, nil
		case errors.Is(err, store.ErrExists):
			// The handle was taken a moment ago, or the identity was
			// linked in another tab.
			if q, err := s.Store.PersonByIdentity(ctx, c.Issuer, cl.Subject); err == nil {
				return q, nil
			}
			continue
		}
		return store.Principal{}, classify(err)
	}
	return store.Principal{}, errf(CodeExists, "could not find a free handle for %s; ask a site admin to create your account", email)
}

// requireSSO refuses a request whose token did not come through the single
// sign-on that r's organization requires of its members. Owners are
// exempt, and so are calls without a token (the server itself).
func (s *Service) requireSSO(ctx context.Context, actor Actor, r store.Repo, facts store.RoleFacts) error {
	if actor.TokenID == uuid.Nil {
		return nil
	}
	if role := OrgRole(facts.OrgRole); role != OrgMember && role != OrgAdmin {
		return nil
	}
	blocked, err := s.Store.SSOBlocked(ctx, r.OrgID, actor.TokenID)
	if err != nil || !blocked {
		return err
	}
	o, err := s.Store.OrgByID(ctx, r.OrgID)
	if err != nil {
		return err
	}
	server := s.publicURL()
	if server == "" {
		server = "URL"
	}
	msg := fmt.Sprintf("organization %s requires single sign-on, and this session or token was not issued through it. In the web UI, sign in with %s's single sign-on; for the CLI, run vx login --server %s and sign in with single sign-on in the browser", o.Handle, o.Handle, server)
	if actor.Kind == store.KindAgent {
		msg += fmt.Sprintf("; agents need a token their person created after signing in that way (vx agent add or vx token create --agent, from a single sign-on login)")
	}
	return &Error{Code: CodePermission, Msg: msg, Err: &SSORequiredError{Org: o.Handle}}
}

// PersonIdentities lists the single sign-on identities of the actor's
// person, for the settings page.
func (s *Service) PersonIdentities(ctx context.Context, actor Actor) ([]store.Identity, error) {
	ids, err := s.Store.IdentitiesOf(ctx, actor.Human().ID)
	return ids, classify(err)
}

// SSOAvailable reports whether any organization here signs in with single
// sign-on, for the sign-in page.
func (s *Service) SSOAvailable(ctx context.Context) bool {
	ok, err := s.Store.AnyOrgSSO(ctx)
	return err == nil && ok
}
