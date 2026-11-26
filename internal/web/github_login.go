package web

// Sign in with GitHub. /ui/login/github sends the browser to GitHub with a
// fresh state and a PKCE challenge; the state, the verifier and where to go
// next live in a short-lived cookie bound to this browser. GitHub sends the
// browser back to /auth/github/callback, which checks the state against the
// cookie and lets the service finish: find or create the person, start a
// session. The same round trip links GitHub to a signed-in person who was
// created with a token (Settings, "Connect GitHub").

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// gitHubFlowTTL is how long a started GitHub sign-in may take.
const gitHubFlowTTL = 10 * time.Minute

const gitHubCookieBase = "vx_github"

// gitHubCookieName carries the port like the session cookie does, so two
// servers on one host keep their sign-ins apart.
func gitHubCookieName(r *http.Request) string {
	return gitHubCookieBase + strings.TrimPrefix(cookieName(r), cookieBase)
}

// gitHubFlow is what the cookie holds between /ui/login/github and the
// callback.
type gitHubFlow struct {
	State    string    `json:"s"`
	Verifier string    `json:"v"`
	Next     string    `json:"n,omitempty"`
	Link     uuid.UUID `json:"l,omitempty"` // linking GitHub to this signed-in person
	Expires  int64     `json:"e"`
}

var (
	flowKeyOnce sync.Once
	flowKey     []byte
)

// gitHubFlowKey signs the flow cookie. The cookie names the person a link
// is for, so a browser must not be able to write one itself. With a server
// key (VX_SECRET_KEY) the signature holds across restarts and instances;
// without one a random key per process does.
func (u *UI) gitHubFlowKey() []byte {
	if k := u.svc.GitHub.SecretKey; len(k) > 0 {
		m := hmac.New(sha256.New, k)
		m.Write([]byte("vx github sign-in cookie"))
		return m.Sum(nil)
	}
	flowKeyOnce.Do(func() {
		flowKey = make([]byte, 32)
		_, _ = rand.Read(flowKey)
	})
	return flowKey
}

func (u *UI) sealFlow(f gitHubFlow) string {
	body, _ := json.Marshal(f)
	m := hmac.New(sha256.New, u.gitHubFlowKey())
	m.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (u *UI) openFlow(v string) (gitHubFlow, bool) {
	body64, sig64, ok := strings.Cut(v, ".")
	if !ok {
		return gitHubFlow{}, false
	}
	body, err1 := base64.RawURLEncoding.DecodeString(body64)
	sig, err2 := base64.RawURLEncoding.DecodeString(sig64)
	if err1 != nil || err2 != nil {
		return gitHubFlow{}, false
	}
	m := hmac.New(sha256.New, u.gitHubFlowKey())
	m.Write(body)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return gitHubFlow{}, false
	}
	var f gitHubFlow
	if json.Unmarshal(body, &f) != nil || f.State == "" || f.Verifier == "" {
		return gitHubFlow{}, false
	}
	return f, true
}

func (u *UI) setFlowCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	// Lax, not Strict: the cookie must come along when GitHub sends the
	// browser back, which is a top-level navigation from github.com.
	http.SetCookie(w, &http.Cookie{
		Name: gitHubCookieName(r), Value: value, Path: "/auth/github", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure(r), MaxAge: maxAge,
	})
}

func (u *UI) mountGitHub(mux *http.ServeMux) {
	// The pages share the layout and functions of the others: clone a
	// parsed page and swap in their content.
	for _, page := range []string{"github", "welcome", "signup"} {
		t, err := u.pages["login"].Clone()
		if err == nil {
			t, err = t.ParseFS(assets, "templates/"+page+".html")
		}
		if err != nil {
			panic("template " + page + ": " + err.Error())
		}
		u.pages[page] = t
	}
	mux.HandleFunc("GET /ui/login/github", u.gitHubStart)
	mux.HandleFunc("GET "+service.GitHubCallbackPath, u.gitHubCallback)
	mux.HandleFunc("GET "+gitHubSignupPath, u.gitHubSignupPage)
	mux.HandleFunc("POST "+gitHubSignupPath, u.gitHubSignupPost)
	mux.HandleFunc("POST /ui/settings/github", u.auth(u.gitHubLinkPost))
	mux.HandleFunc("GET /ui/welcome", u.auth(u.welcome))
	mux.HandleFunc("POST /ui/welcome", u.auth(u.welcomePost))
}

// gitHubProblem is a friendly page for a sign-in that did not work: what
// happened and what to do.
type gitHubProblem struct {
	Status  int
	Title   string
	Message string
	Login   string // the GitHub login, when known
	Retry   string // where "Try again" goes
}

func (u *UI) gitHubFail(w http.ResponseWriter, r *http.Request, p gitHubProblem) {
	if p.Retry == "" {
		p.Retry = "/ui/login"
	}
	u.renderStatus(w, r, p.Status, "github", map[string]any{"Title": p.Title, "Problem": p})
}

// gitHubStart begins a sign-in. It is a GET so the login page can link to
// it: the worst a forged link can do is sign someone in to their own
// account after they agree on GitHub.
func (u *UI) gitHubStart(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	u.gitHubRedirect(w, r, gitHubFlow{Next: next}, "/ui/login?next="+url.QueryEscape(safeNext(next)))
}

func (u *UI) gitHubRedirect(w http.ResponseWriter, r *http.Request, f gitHubFlow, retry string) {
	if !u.svc.GitHubSignInEnabled(r.Context()) {
		u.gitHubFail(w, r, gitHubProblem{Status: http.StatusServiceUnavailable, Title: "GitHub sign-in is not set up",
			Message: "This vx server has no GitHub App yet, so it cannot sign you in with GitHub. A superadmin sets it up on Admin > GitHub. Until then, sign in with a token or vx ui.",
			Retry:   "/ui/login"})
		return
	}
	authURL, state, verifier, err := u.svc.GitHubSignInStart(r.Context(), f.Next)
	if err != nil {
		u.gitHubFail(w, r, gitHubProblem{Status: errStatus(err), Title: "Could not start the sign-in", Message: err.Error(), Retry: retry})
		return
	}
	f.State, f.Verifier, f.Expires = state, verifier, time.Now().Add(gitHubFlowTTL).Unix()
	u.setFlowCookie(w, r, u.sealFlow(f), int(gitHubFlowTTL/time.Second))
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (u *UI) gitHubCallback(w http.ResponseWriter, r *http.Request) {
	// The code is in the URL: keep it out of other sites' logs.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	var flow gitHubFlow
	var have bool
	if c, err := r.Cookie(gitHubCookieName(r)); err == nil {
		flow, have = u.openFlow(c.Value)
	}
	// One use per cookie: a replayed callback finds none.
	u.setFlowCookie(w, r, "", -1)
	retry := "/ui/login/github"
	if have && flow.Link != uuid.Nil {
		retry = "/ui/settings"
	} else if have && flow.Next != "" {
		retry += "?next=" + url.QueryEscape(safeNext(flow.Next))
	}
	if e := q.Get("error"); e != "" {
		msg := "GitHub did not sign you in (" + e + "). Nothing changed in vx."
		if e == "access_denied" {
			msg = "You cancelled on GitHub, so vx did not get access to your GitHub account. Nothing changed. To sign in, start again and choose Authorize on GitHub."
		}
		u.gitHubFail(w, r, gitHubProblem{Status: http.StatusForbidden, Title: "Not signed in", Message: msg, Retry: retry})
		return
	}
	if !have || time.Now().Unix() > flow.Expires {
		u.gitHubFail(w, r, gitHubProblem{Status: http.StatusBadRequest, Title: "Sign-in expired",
			Message: "This sign-in was started in another browser, or more than 10 minutes ago, or was already used. Start again from this browser.", Retry: retry})
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(flow.State)) != 1 {
		u.gitHubFail(w, r, gitHubProblem{Status: http.StatusBadRequest, Title: "Sign-in did not match",
			Message: "GitHub came back with a different sign-in than the one this browser started, so vx stopped here to keep your account safe. Start again from this browser.", Retry: retry})
		return
	}
	if flow.Link != uuid.Nil {
		u.gitHubLinkFinish(w, r, flow, q.Get("code"))
		return
	}
	gu, err := u.svc.GitHubSignInUser(r.Context(), q.Get("code"), flow.Verifier)
	if err != nil {
		u.gitHubFailErr(w, r, err, retry)
		return
	}
	confirm, err := u.svc.GitHubSignupNeedsConfirm(r.Context(), gu)
	if err != nil {
		u.gitHubFailErr(w, r, err, retry)
		return
	}
	if confirm {
		// Ask before creating an account: the GitHub account this browser
		// is signed in to may not be the one the person meant.
		u.setSignupCookie(w, r, u.sealSignup(gitHubSignup{User: gu, Next: flow.Next, Expires: time.Now().Add(gitHubFlowTTL).Unix()}), int(gitHubFlowTTL/time.Second))
		http.Redirect(w, r, gitHubSignupPath, http.StatusSeeOther)
		return
	}
	u.gitHubSignInAs(w, r, gu, flow.Next, retry)
}

// gitHubSignInAs signs the browser in as GitHub user gu and moves on to
// next (the welcome page for a new account).
func (u *UI) gitHubSignInAs(w http.ResponseWriter, r *http.Request, gu service.GitHubUser, next, retry string) {
	p, token, isNew, err := u.svc.GitHubSignInAs(r.Context(), gu)
	if err != nil {
		u.gitHubFailErr(w, r, err, retry)
		return
	}
	setSession(w, r, token)
	dest := safeNext(next)
	if isNew && dest == "/ui" {
		dest = "/ui/welcome"
	}
	u.gitHubContinue(w, r, dest, p.DisplayName)
}

// ---------- confirming a new account ----------

// gitHubSignupPath asks "create a new vx account for GitHub LOGIN?" (GET)
// and creates it on Continue (POST). It is under /auth/github, where the
// sign-in cookies live.
const gitHubSignupPath = "/auth/github/signup"

// gitHubSignup is what the signup cookie holds between the callback and
// Continue: the GitHub user the code was for. It is signed like the flow
// cookie, so a browser cannot name another GitHub account.
type gitHubSignup struct {
	User    service.GitHubUser `json:"u"`
	Next    string             `json:"n,omitempty"`
	Expires int64              `json:"e"`
}

func gitHubSignupCookieName(r *http.Request) string { return gitHubCookieName(r) + "_signup" }

func (u *UI) sealSignup(sg gitHubSignup) string {
	body, _ := json.Marshal(sg)
	m := hmac.New(sha256.New, u.gitHubFlowKey())
	m.Write([]byte("signup\x00"))
	m.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (u *UI) openSignup(r *http.Request) (gitHubSignup, bool) {
	c, err := r.Cookie(gitHubSignupCookieName(r))
	if err != nil {
		return gitHubSignup{}, false
	}
	body64, sig64, ok := strings.Cut(c.Value, ".")
	if !ok {
		return gitHubSignup{}, false
	}
	body, err1 := base64.RawURLEncoding.DecodeString(body64)
	sig, err2 := base64.RawURLEncoding.DecodeString(sig64)
	if err1 != nil || err2 != nil {
		return gitHubSignup{}, false
	}
	m := hmac.New(sha256.New, u.gitHubFlowKey())
	m.Write([]byte("signup\x00"))
	m.Write(body)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return gitHubSignup{}, false
	}
	var sg gitHubSignup
	if json.Unmarshal(body, &sg) != nil || sg.User.ID == 0 || sg.User.Login == "" || time.Now().Unix() > sg.Expires {
		return gitHubSignup{}, false
	}
	return sg, true
}

func (u *UI) setSignupCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	// Lax like the flow cookie: it is set in the redirect chain that
	// started on github.com and must come along to the page it leads to.
	http.SetCookie(w, &http.Cookie{
		Name: gitHubSignupCookieName(r), Value: value, Path: "/auth/github", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure(r), MaxAge: maxAge,
	})
}

func (u *UI) signupExpired(w http.ResponseWriter, r *http.Request) {
	u.gitHubFail(w, r, gitHubProblem{Status: http.StatusBadRequest, Title: "Sign-up expired",
		Message: "This sign-up was started in another browser, more than 10 minutes ago, or was already finished. No account was created. Sign in with GitHub again.",
		Retry:   "/ui/login/github"})
}

// gitHubSignupPage asks before a sign-in with GitHub creates an account.
func (u *UI) gitHubSignupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	sg, ok := u.openSignup(r)
	if !ok {
		u.signupExpired(w, r)
		return
	}
	retry := "/ui/login/github"
	if sg.Next != "" {
		retry += "?next=" + url.QueryEscape(safeNext(sg.Next))
	}
	u.render(w, r, "signup", map[string]any{"Title": "Create a new account", "Login": sg.User.Login, "Name": sg.User.Name,
		"SwitchURL": u.svc.GitHubWebURL() + "/logout", "Retry": retry})
}

// gitHubSignupPost creates the account the signup page asked about.
func (u *UI) gitHubSignupPost(w http.ResponseWriter, r *http.Request) {
	if !u.checkOrigin(w, r) {
		return
	}
	sg, ok := u.openSignup(r)
	if !ok {
		u.signupExpired(w, r)
		return
	}
	// One use per cookie: Continue twice creates no second account.
	u.setSignupCookie(w, r, "", -1)
	retry := "/ui/login/github"
	if sg.Next != "" {
		retry += "?next=" + url.QueryEscape(safeNext(sg.Next))
	}
	u.gitHubSignInAs(w, r, sg.User, sg.Next, retry)
}

func (u *UI) gitHubFailErr(w http.ResponseWriter, r *http.Request, err error, retry string) {
	var refused *service.GitHubSignupRefused
	if errors.As(err, &refused) {
		p := gitHubProblem{Status: http.StatusForbidden, Login: refused.Login, Retry: "/ui/login"}
		if refused.Mode == service.SignupClosed {
			p.Title = "No new accounts"
			p.Message = "This vx server does not take new accounts right now. If you should have one, ask a superadmin to add you."
		} else {
			p.Title = "You are not invited yet"
			p.Message = "This vx server is invite-only, and GitHub user " + refused.Login + " has no invite. Ask a superadmin to invite " +
				strings.ToLower(refused.Login) + " (Admin > People > Invite by GitHub login), then sign in with GitHub again."
		}
		u.gitHubFail(w, r, p)
		return
	}
	title := "Could not sign you in"
	if errStatus(err) == http.StatusConflict {
		title = "Could not connect GitHub"
	}
	u.gitHubFail(w, r, gitHubProblem{Status: errStatus(err), Title: title, Message: err.Error(), Retry: retry})
}

// gitHubContinue answers the callback with a page that moves on by itself.
// Not a redirect: the session cookie is SameSite=Strict, and browsers do not
// send it on a redirect chain that started on github.com, so the next page
// would look signed out. A navigation from this page is same-site.
func (u *UI) gitHubContinue(w http.ResponseWriter, r *http.Request, dest, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := u.pages["github"].ExecuteTemplate(w, "continue", map[string]any{"Dest": dest, "Name": name}); err != nil {
		u.log.Error("render", "page", "github continue", "err", err)
	}
}

// gitHubLinkPost starts connecting GitHub to the signed-in person.
func (u *UI) gitHubLinkPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if !u.checkOrigin(w, r) {
		return
	}
	if actor.Kind != store.KindHuman {
		u.fail(w, &service.Error{Code: service.CodePermission, Msg: "agents cannot connect a GitHub account; sign in to the web UI as the person instead"})
		return
	}
	if l, ok, err := u.svc.GitHubLinkOf(r.Context(), actor); err == nil && ok {
		u.fail(w, &service.Error{Code: service.CodeExists, Msg: "you are already connected to GitHub user " + l.Login})
		return
	}
	u.gitHubRedirect(w, r, gitHubFlow{Next: "/ui/settings", Link: actor.ID}, "/ui/settings")
}

func (u *UI) gitHubLinkFinish(w http.ResponseWriter, r *http.Request, flow gitHubFlow, code string) {
	ctx := r.Context()
	p, err := u.svc.Store.PrincipalByID(ctx, flow.Link)
	if err != nil {
		u.gitHubFailErr(w, r, err, "/ui/settings")
		return
	}
	actor, err := u.svc.ResolveActor(ctx, p)
	if err != nil {
		u.gitHubFailErr(w, r, err, "/ui/settings")
		return
	}
	if _, err := u.svc.GitHubLinkFinish(ctx, actor, code, flow.Verifier); err != nil {
		u.gitHubFailErr(w, r, err, "/ui/settings")
		return
	}
	u.gitHubContinue(w, r, "/ui/settings?github=connected", p.DisplayName)
}

// ---------- welcome ----------

// publicURL is the address people reach this server at: the configured
// public URL, else the one this request came to.
func (u *UI) publicURL(r *http.Request) string {
	if p := strings.TrimRight(u.svc.GitHub.PublicURL, "/"); p != "" {
		return p
	}
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (u *UI) welcome(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	data := map[string]any{"Actor": actor, "Title": "Welcome", "Server": u.publicURL(r),
		"GitHub": u.svc.GitHubSignInEnabled(r.Context()), "Shown": u.svc.ShowWelcome(ctx, actor), "IsHuman": actor.Kind == store.KindHuman,
		"SiteAdmin": u.svc.IsSuperadmin(ctx, actor)}
	if l, ok, err := u.svc.GitHubLinkOf(ctx, actor); err == nil && ok {
		data["GitHubLogin"] = l.Login
	}
	if link, err := u.svc.GitHubAppInstallURL(ctx); err == nil {
		data["InstallURL"] = link
	}
	if inst, err := u.svc.MyGitHubInstallations(ctx, actor); err == nil {
		data["Installations"] = inst
	}
	u.render(w, r, "welcome", data)
}

func (u *UI) welcomePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if !u.checkOrigin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	show := r.PostForm.Get("show") == "yes"
	if err := u.svc.DismissWelcome(r.Context(), actor, !show); err != nil {
		u.failPage(w, err)
		return
	}
	if show {
		http.Redirect(w, r, "/ui/welcome", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui", http.StatusSeeOther)
}

// settingsGitHub adds the GitHub panel of the settings page.
func (u *UI) settingsGitHub(r *http.Request, actor service.Actor, data map[string]any) {
	data["GitHubEnabled"] = u.svc.GitHubSignInEnabled(r.Context())
	if l, ok, err := u.svc.GitHubLinkOf(r.Context(), actor); err == nil && ok {
		data["GitHubLogin"] = l.Login
	}
	data["GitHubConnected"] = r.URL.Query().Get("github") == "connected"
}

// ---------- invites (Admin > People) ----------

// peopleGitHub adds GitHub logins and invites to the people page.
func (u *UI) peopleGitHub(r *http.Request, actor service.Actor, rows []personRow, data map[string]any) {
	ctx := r.Context()
	ids := make([]uuid.UUID, len(rows))
	for i, p := range rows {
		ids[i] = p.ID
	}
	if links, err := u.svc.Store.GitHubLinksOf(ctx, ids); err == nil {
		for i := range rows {
			rows[i].GitHub = links[rows[i].ID].Login
		}
	}
	data["GitHubEnabled"] = u.svc.GitHubSignInEnabled(r.Context())
	data["Signup"] = string(u.svc.SignupModeOrDefault())
	if inv, err := u.svc.ListGitHubInvites(ctx, actor, false); err == nil {
		data["Invites"] = inv
	}
}

func (u *UI) invitePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if !u.svc.IsSuperadmin(r.Context(), actor) {
		http.NotFound(w, r)
		return
	}
	f := r.PostForm
	if err := u.svc.InviteGitHubUser(r.Context(), actor, f.Get("login"), f.Get("note")); err != nil {
		u.renderPeople(w, r, actor, failed(err), f)
		return
	}
	http.Redirect(w, r, "/ui/admin/people?ok=invited#invites", http.StatusSeeOther)
}

func (u *UI) inviteWithdrawPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if !u.svc.IsSuperadmin(r.Context(), actor) {
		http.NotFound(w, r)
		return
	}
	if err := u.svc.WithdrawGitHubInvite(r.Context(), actor, r.PostForm.Get("login")); err != nil {
		u.renderPeople(w, r, actor, failed(err), nil)
		return
	}
	http.Redirect(w, r, "/ui/admin/people?ok=uninvited#invites", http.StatusSeeOther)
}
