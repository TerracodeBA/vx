package service

// Sign in with GitHub on the web: GitHub App user authorization (the OAuth
// web flow) with state and PKCE, sign-up by signup mode, invites, and
// linking GitHub to a person created earlier with a token.
//
// The web layer keeps state and verifier in a short-lived cookie bound to
// the browser and checks state itself; the service checks everything that
// needs GitHub or the database.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// GitHubSignInEnabled reports whether people can sign in with GitHub here:
// the server has a GitHub App.
func (s *Service) GitHubSignInEnabled(ctx context.Context) bool {
	_, err := s.GitHubClient(ctx)
	return err == nil
}

// SignupModeOrDefault is the signup mode in effect: invite unless set.
func (s *Service) SignupModeOrDefault() SignupMode {
	switch s.GitHub.Signup {
	case SignupOpen, SignupClosed:
		return s.GitHub.Signup
	}
	return SignupInvite
}

// ParseSignupMode reads a signup mode from configuration (VX_SIGNUP).
func ParseSignupMode(v string) (SignupMode, error) {
	switch m := SignupMode(strings.ToLower(strings.TrimSpace(v))); m {
	case "":
		return SignupInvite, nil
	case SignupInvite, SignupOpen, SignupClosed:
		return m, nil
	}
	return "", fmt.Errorf("signup mode %q: use invite, open or closed", v)
}

// gitHubCallbackURL is the redirect URI registered with the App.
func (s *Service) gitHubCallbackURL() (string, error) {
	base := s.publicURL()
	if base == "" {
		return "", errf(CodeUnavailable, "this server does not know its public address, so GitHub cannot send you back to it: the server admin sets VX_PUBLIC_URL (for example https://vx.example.com)")
	}
	return base + GitHubCallbackPath, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// PKCEChallenge is the S256 code challenge for a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Service) gitHubAPI(ctx context.Context) (GitHubAPI, error) {
	api, err := s.GitHubClient(ctx)
	if errors.Is(err, ErrGitHubNotConfigured) {
		return nil, &Error{Code: CodeUnavailable, Msg: ErrGitHubNotConfigured.Error(), Err: ErrGitHubNotConfigured}
	}
	return api, err
}

// GitHubSignInStart returns the GitHub authorize URL for a web sign-in and
// the state and PKCE verifier the web layer keeps in a short-lived cookie.
// next is kept by the web layer too; it is not sent to GitHub.
func (s *Service) GitHubSignInStart(ctx context.Context, next string) (authorizeURL, state, verifier string, err error) {
	if _, err := s.gitHubAPI(ctx); err != nil {
		return "", "", "", err
	}
	app, err := s.GitHubApp(ctx)
	if err != nil {
		return "", "", "", err
	}
	redirect, err := s.gitHubCallbackURL()
	if err != nil {
		return "", "", "", err
	}
	if state, err = randomToken(32); err != nil {
		return "", "", "", err
	}
	if verifier, err = randomToken(32); err != nil { // 43 characters, within RFC 7636's 43..128
		return "", "", "", err
	}
	q := url.Values{
		"client_id":             {app.ClientID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {PKCEChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	return app.AuthorizeURL + "?" + q.Encode(), state, verifier, nil
}

// GitHubSignupRefused is returned when a GitHub account has no vx person
// and the signup mode does not let it create one.
type GitHubSignupRefused struct {
	Login string
	Mode  SignupMode
}

func (e *GitHubSignupRefused) Error() string {
	if e.Mode == SignupClosed {
		return "this vx server does not take new accounts; ask a superadmin to add you"
	}
	return fmt.Sprintf("GitHub user %s is not invited to this vx server; ask a superadmin to invite %s (Admin > People, or vx-server invite %s)", e.Login, e.Login, strings.ToLower(e.Login))
}

// gitHubUser exchanges a sign-in code and reads the GitHub user it is for.
func (s *Service) gitHubUser(ctx context.Context, code, verifier string) (GitHubUser, error) {
	api, err := s.gitHubAPI(ctx)
	if err != nil {
		return GitHubUser{}, err
	}
	redirect, err := s.gitHubCallbackURL()
	if err != nil {
		return GitHubUser{}, err
	}
	if strings.TrimSpace(code) == "" || verifier == "" {
		return GitHubUser{}, errf(CodeInvalid, "GitHub sent no sign-in code back; start the sign-in again")
	}
	userToken, err := api.ExchangeOAuthCode(ctx, code, redirect, verifier)
	if err != nil {
		return GitHubUser{}, &Error{Code: CodeInvalid, Err: err,
			Msg: "GitHub did not accept this sign-in: the code may have expired or been used already. Start the sign-in again"}
	}
	u, err := api.User(ctx, userToken)
	if err != nil {
		return GitHubUser{}, &Error{Code: CodeUnavailable, Err: err, Msg: "could not read your GitHub account: " + err.Error() + ". Try again in a moment"}
	}
	if u.ID == 0 || u.Login == "" {
		return GitHubUser{}, errf(CodeUnavailable, "GitHub returned an account without id or login; try again in a moment")
	}
	return u, nil
}

// GitHubSignInFinish completes a web sign-in: it links or creates the
// person (per the signup mode), syncs their organizations, and returns the
// person and a new web-session token. isNew is true on first sign-in.
func (s *Service) GitHubSignInFinish(ctx context.Context, code, verifier string) (p store.Principal, token string, isNew bool, err error) {
	u, err := s.gitHubUser(ctx, code, verifier)
	if err != nil {
		return store.Principal{}, "", false, err
	}
	p, isNew, renamedFrom, err := s.gitHubPerson(ctx, u)
	if err != nil {
		return store.Principal{}, "", false, err
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if token, err = tx.CreateToken(ctx, p.ID, "web session (GitHub)"); err != nil {
			return err
		}
		if isNew {
			return nil // person.signup is logged with the person
		}
		after := map[string]any{"github_id": u.ID, "github_login": u.Login}
		if renamedFrom != "" {
			after["renamed_from"] = renamedFrom
		}
		return tx.LogOp(ctx, nil, &p.ID, "person.signin", p.Ref(), nil, after)
	})
	if err != nil {
		return store.Principal{}, "", false, classify(err)
	}
	if err := s.SyncGitHubOrgs(ctx, p); err != nil {
		// Memberships refresh on the next sign-in or webhook; signing in
		// still works.
		slog.Default().Warn("github sign-in: syncing organizations", "person", p.Ref(), "err", err)
	}
	// An installation on the person's own GitHub account waits for them to
	// exist; bring its repositories in now.
	if err := s.SyncGitHubAccount(ctx, u.ID); err != nil {
		slog.Default().Warn("github sign-in: syncing personal installations", "person", p.Ref(), "err", err)
	}
	return p, token, isNew, nil
}

// gitHubPerson finds the person linked to u, keeping their GitHub login
// and display name current, or creates one when the signup mode allows.
func (s *Service) gitHubPerson(ctx context.Context, u GitHubUser) (p store.Principal, isNew bool, renamedFrom string, err error) {
	p, err = s.Store.PrincipalByGitHubID(ctx, u.ID)
	if err == nil {
		renamedFrom, err = s.refreshGitHubPerson(ctx, &p, u)
		return p, false, renamedFrom, err
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Principal{}, false, "", err
	}
	if err := s.gitHubDuplicate(ctx, u); err != nil {
		return store.Principal{}, false, "", err
	}
	p, err = s.gitHubSignup(ctx, u)
	if errors.Is(err, errLinkedMeanwhile) {
		// The same GitHub user signed up in another tab a moment ago.
		p, err = s.Store.PrincipalByGitHubID(ctx, u.ID)
		return p, false, "", classify(err)
	}
	return p, err == nil, "", err
}

// refreshGitHubPerson updates the stored GitHub login and display name of p
// when they changed on GitHub; it returns the old login after a rename.
func (s *Service) refreshGitHubPerson(ctx context.Context, p *store.Principal, u GitHubUser) (renamedFrom string, err error) {
	link, _, err := s.Store.GitHubLinkOf(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if link.Login != u.Login {
		if err := s.Store.LinkGitHub(ctx, p.ID, u.ID, u.Login); err != nil {
			return "", classify(err)
		}
		renamedFrom = link.Login
	}
	if name := strings.TrimSpace(u.Name); name != "" && name != p.DisplayName {
		if err := s.Store.SetDisplayName(ctx, p.ID, name); err != nil {
			return "", err
		}
		p.DisplayName = name
	}
	return renamedFrom, nil
}

var errLinkedMeanwhile = errors.New("GitHub user was linked meanwhile")

// gitHubDuplicate refuses a first GitHub sign-in whose verified email
// already belongs to a person: they sign in to that account and connect
// GitHub there instead of getting a second account.
func (s *Service) gitHubDuplicate(ctx context.Context, u GitHubUser) error {
	email := strings.TrimSpace(u.Email)
	if email == "" || strings.HasSuffix(strings.ToLower(email), "@users.noreply.github.com") {
		return nil
	}
	people, err := s.Store.PeopleByEmail(ctx, email)
	if err != nil || len(people) == 0 {
		return err
	}
	e := &AccountExistsError{Email: strings.ToLower(email), Handle: people[0].Handle, Via: "GitHub"}
	if l, ok, _ := s.Store.GitHubLinkOf(ctx, people[0].ID); ok {
		e.GitHubLogin = l.Login
	}
	return &Error{Code: CodeExists, Err: e, Msg: e.Error()}
}

// gitHubSignup creates a person for a GitHub user, per the signup mode.
func (s *Service) gitHubSignup(ctx context.Context, u GitHubUser) (store.Principal, error) {
	mode := s.SignupModeOrDefault()
	if mode == SignupClosed {
		return store.Principal{}, &GitHubSignupRefused{Login: u.Login, Mode: mode}
	}
	login := strings.ToLower(u.Login)
	var invite store.SignupInvite
	if mode == SignupInvite {
		inv, ok, err := s.Store.OpenInvite(ctx, login)
		if err != nil {
			return store.Principal{}, err
		}
		if !ok {
			return store.Principal{}, &GitHubSignupRefused{Login: u.Login, Mode: mode}
		}
		invite = inv
	}
	if err := s.AllowSignup(ctx); err != nil {
		return store.Principal{}, err
	}
	email := strings.TrimSpace(u.Email)
	if email == "" {
		email = fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)
	}
	name := strings.TrimSpace(u.Name)
	if name == "" {
		name = u.Login
	}
	base := GitHubHandle(u.Login, u.ID)
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
			used, err := tx.UseInvite(ctx, login)
			if err != nil {
				return err
			}
			if mode == SignupInvite && !used {
				return &GitHubSignupRefused{Login: u.Login, Mode: mode} // used up in another tab
			}
			if p, err = tx.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: handle, DisplayName: name, Email: email, SiteAdmin: invite.SiteAdmin}); err != nil {
				return err
			}
			if err := tx.LinkGitHub(ctx, p.ID, u.ID, u.Login); err != nil {
				if errors.Is(err, store.ErrExists) {
					return errLinkedMeanwhile
				}
				return err
			}
			via := string(mode)
			if used {
				via = "invite"
			}
			return tx.LogOp(ctx, nil, &p.ID, "person.signup", p.Ref(), nil, map[string]any{
				"github_id": u.ID, "github_login": u.Login, "via": via, "site_admin": p.SiteAdmin})
		})
		switch {
		case err == nil:
			return p, nil
		case errors.Is(err, errLinkedMeanwhile):
			return store.Principal{}, err
		case errors.Is(err, store.ErrExists):
			continue // the handle was taken a moment ago
		}
		return store.Principal{}, classify(err)
	}
	return store.Principal{}, errf(CodeExists, "could not find a free handle for GitHub user %s; ask a superadmin to create your account", u.Login)
}

// maxHandle is the longest handle ValidateHandle accepts.
const maxHandle = 39

// GitHubHandle turns a GitHub login into a vx handle: lowercased,
// characters handles do not allow become "-", then leading and trailing
// separators are trimmed. A login with nothing left becomes github-<id>.
func GitHubHandle(login string, id int64) string {
	var b strings.Builder
	for _, r := range strings.ToLower(login) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	h := b.String()
	if len(h) > maxHandle {
		h = h[:maxHandle]
	}
	h = strings.Trim(h, "-._")
	if h == "" {
		return fmt.Sprintf("github-%d", id)
	}
	return h
}

// handleWithSuffix is base for n == 1 and base-n after that, shortened to
// stay a valid handle.
func handleWithSuffix(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := fmt.Sprintf("-%d", n)
	if len(base)+len(suffix) > maxHandle {
		base = strings.TrimRight(base[:maxHandle-len(suffix)], "-._")
	}
	return base + suffix
}

// gitHubLogin accepts a GitHub login as people paste it: with or without
// @, or as a github.com profile URL.
var gitHubLogin = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)

// NormalizeGitHubLogin returns the lowercased login, or an error saying
// what a login looks like.
func NormalizeGitHubLogin(v string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(v))
	for _, p := range []string{"https://github.com/", "http://github.com/", "github.com/", "@"} {
		l = strings.TrimPrefix(l, p)
	}
	l = strings.TrimSuffix(l, "/")
	if !gitHubLogin.MatchString(l) {
		return "", errf(CodeInvalid, "%q is not a GitHub login: use letters, digits and single dashes, like octocat", strings.TrimSpace(v))
	}
	return l, nil
}

// InviteGitHubUser allows a GitHub login to sign up while sign-up is
// invite-only (superadmins).
func (s *Service) InviteGitHubUser(ctx context.Context, actor Actor, login, note string) error {
	if err := peopleOnly(actor, "invite people"); err != nil {
		return err
	}
	if !s.IsSuperadmin(ctx, actor) {
		return errf(CodePermission, "only superadmins can invite people; %s is not one", actor.Ref())
	}
	_, err := s.InviteGitHubLogin(ctx, &actor.Principal, login, note, false)
	return err
}

// InviteGitHubLogin invites a GitHub login on behalf of by (nil: the server
// console, vx-server invite). siteAdmin makes the person a superadmin when
// they sign up; only the console can ask for that.
func (s *Service) InviteGitHubLogin(ctx context.Context, by *store.Principal, login, note string, siteAdmin bool) (store.SignupInvite, error) {
	l, err := NormalizeGitHubLogin(login)
	if err != nil {
		return store.SignupInvite{}, err
	}
	note = strings.TrimSpace(note)
	if len(note) > 200 {
		return store.SignupInvite{}, errf(CodeInvalid, "the note is %d characters; keep it under 200", len(note))
	}
	var byID *uuid.UUID
	if by != nil {
		byID = &by.ID
	}
	var inv store.SignupInvite
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.PrincipalByGitHubLogin(ctx, l); err == nil {
			return errf(CodeExists, "GitHub user %s already has an account here; they can sign in with GitHub", l)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		var err error
		inv, err = tx.CreateInvite(ctx, l, byID, note, siteAdmin)
		if errors.Is(err, store.ErrExists) {
			return errf(CodeExists, "the invite for %s was already used; they can sign in with GitHub", l)
		}
		if err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, byID, "person.invite", "github:"+l, nil, map[string]any{"note": note, "site_admin": siteAdmin})
	})
	return inv, classify(err)
}

// ListGitHubInvites lists invites (superadmins): open ones, or all.
func (s *Service) ListGitHubInvites(ctx context.Context, actor Actor, all bool) ([]store.SignupInvite, error) {
	if !s.IsSuperadmin(ctx, actor) {
		return nil, errf(CodePermission, "only superadmins can see invites; %s is not one", actor.Human().Ref())
	}
	inv, err := s.Store.ListInvites(ctx, all)
	return inv, classify(err)
}

// WithdrawGitHubInvite deletes an open invite (superadmins).
func (s *Service) WithdrawGitHubInvite(ctx context.Context, actor Actor, login string) error {
	if err := peopleOnly(actor, "withdraw invites"); err != nil {
		return err
	}
	if !s.IsSuperadmin(ctx, actor) {
		return errf(CodePermission, "only superadmins can withdraw invites; %s is not one", actor.Ref())
	}
	l, err := NormalizeGitHubLogin(login)
	if err != nil {
		return err
	}
	return classify(s.Store.InTx(ctx, func(tx *store.Tx) error {
		ok, err := tx.DeleteInvite(ctx, l)
		if err != nil {
			return err
		}
		if !ok {
			return errf(CodeNotFound, "there is no open invite for %s", l)
		}
		return tx.LogOp(ctx, nil, &actor.ID, "person.uninvite", "github:"+l, nil, nil)
	}))
}

// GitHubLinkFinish links the GitHub account a sign-in code is for to the
// signed-in person, for people created with a token before GitHub sign-in.
func (s *Service) GitHubLinkFinish(ctx context.Context, actor Actor, code, verifier string) (GitHubUser, error) {
	if err := peopleOnly(actor, "connect a GitHub account"); err != nil {
		return GitHubUser{}, err
	}
	u, err := s.gitHubUser(ctx, code, verifier)
	if err != nil {
		return GitHubUser{}, err
	}
	if other, err := s.Store.PrincipalByGitHubID(ctx, u.ID); err == nil {
		if other.ID == actor.ID {
			_, err := s.refreshGitHubPerson(ctx, &other, u)
			return u, err
		}
		return GitHubUser{}, errf(CodeExists, "GitHub user %s is already connected to %s; sign out and sign in with GitHub to use that account", u.Login, other.Ref())
	} else if !errors.Is(err, store.ErrNotFound) {
		return GitHubUser{}, err
	}
	if cur, ok, err := s.Store.GitHubLinkOf(ctx, actor.ID); err != nil {
		return GitHubUser{}, classify(err)
	} else if ok {
		return GitHubUser{}, errf(CodeExists, "%s is already connected to GitHub user %s; vx keeps one GitHub account per person", actor.Ref(), cur.Login)
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.LinkGitHub(ctx, actor.ID, u.ID, u.Login); err != nil {
			return err
		}
		_, _ = tx.UseInvite(ctx, u.Login)
		return tx.LogOp(ctx, nil, &actor.ID, "person.github_link", actor.Ref(), nil, map[string]any{"github_id": u.ID, "github_login": u.Login})
	})
	if errors.Is(err, store.ErrExists) {
		return GitHubUser{}, errf(CodeExists, "GitHub user %s was just connected to another person", u.Login)
	}
	if err != nil {
		return GitHubUser{}, classify(err)
	}
	if err := s.SyncGitHubOrgs(ctx, actor.Principal); err != nil {
		slog.Default().Warn("github link: syncing organizations", "person", actor.Ref(), "err", err)
	}
	if err := s.SyncGitHubAccount(ctx, u.ID); err != nil {
		slog.Default().Warn("github link: syncing personal installations", "person", actor.Ref(), "err", err)
	}
	return u, nil
}

// GitHubLinkOf returns the GitHub account linked to a person; ok is false
// when there is none.
func (s *Service) GitHubLinkOf(ctx context.Context, actor Actor) (store.GitHubLink, bool, error) {
	l, ok, err := s.Store.GitHubLinkOf(ctx, actor.Human().ID)
	return l, ok, classify(err)
}

// ShowWelcome reports whether the welcome page is still linked from the
// navigation for actor's person.
func (s *Service) ShowWelcome(ctx context.Context, actor Actor) bool {
	if actor.Kind != store.KindHuman {
		return false
	}
	dismissed, err := s.Store.WelcomeDismissed(ctx, actor.ID)
	return err == nil && !dismissed
}

// DismissWelcome hides the welcome page from the navigation (or shows it
// again).
func (s *Service) DismissWelcome(ctx context.Context, actor Actor, dismissed bool) error {
	if err := peopleOnly(actor, "dismiss the welcome page"); err != nil {
		return err
	}
	return classify(s.Store.SetWelcomeDismissed(ctx, actor.ID, dismissed))
}

// SyncGitHubOrgs and MyGitHubInstallations are in github_roles.go.
