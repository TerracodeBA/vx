package web

// Single sign-on through an organization's identity provider (OIDC).
// /ui/login/sso finds the organization (by work email or handle) and sends
// the browser to its provider with state, nonce and a PKCE challenge; they
// and the verifier live in a short-lived signed cookie bound to this
// browser. The provider sends the browser back to /auth/oidc/callback,
// which checks the state against the cookie and lets the service finish.
//
// Signed in already (with GitHub or a token), the same round trip connects
// the identity to that person and marks their current session; a GET then
// asks first, so a link from another site cannot start it.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

const oidcCookieBase = "vx_oidc"

func oidcCookieName(r *http.Request) string {
	return oidcCookieBase + strings.TrimPrefix(cookieName(r), cookieBase)
}

// oidcFlow is what the cookie holds between start and callback.
type oidcFlow struct {
	Org      string    `json:"o"`
	State    string    `json:"s"`
	Nonce    string    `json:"n"`
	Verifier string    `json:"v"`
	Next     string    `json:"x,omitempty"`
	Person   uuid.UUID `json:"p,omitempty"` // signed-in person: connect and mark their session
	Token    uuid.UUID `json:"t,omitempty"` // that session's token
	Expires  int64     `json:"e"`
}

func (u *UI) oidcKey() []byte {
	m := hmac.New(sha256.New, u.gitHubFlowKey())
	m.Write([]byte("vx oidc sign-in cookie"))
	return m.Sum(nil)
}

func (u *UI) sealOIDC(f oidcFlow) string {
	body, _ := json.Marshal(f)
	m := hmac.New(sha256.New, u.oidcKey())
	m.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (u *UI) openOIDC(v string) (oidcFlow, bool) {
	body64, sig64, ok := strings.Cut(v, ".")
	if !ok {
		return oidcFlow{}, false
	}
	body, err1 := base64.RawURLEncoding.DecodeString(body64)
	sig, err2 := base64.RawURLEncoding.DecodeString(sig64)
	if err1 != nil || err2 != nil {
		return oidcFlow{}, false
	}
	m := hmac.New(sha256.New, u.oidcKey())
	m.Write(body)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return oidcFlow{}, false
	}
	var f oidcFlow
	if json.Unmarshal(body, &f) != nil || f.State == "" || f.Nonce == "" || f.Verifier == "" || f.Org == "" {
		return oidcFlow{}, false
	}
	return f, true
}

func (u *UI) setOIDCCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	// Lax: the cookie must come along when the provider sends the browser
	// back, a top-level navigation from another site.
	http.SetCookie(w, &http.Cookie{
		Name: oidcCookieName(r), Value: value, Path: "/auth/oidc", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secure(r), MaxAge: maxAge,
	})
}

func (u *UI) mountSSO(mux *http.ServeMux) {
	t, err := u.pages["login"].Clone()
	if err == nil {
		t, err = t.ParseFS(assets, "templates/sso.html")
	}
	if err != nil {
		panic("template sso: " + err.Error())
	}
	u.pages["sso"] = t
	mux.HandleFunc("GET /ui/login/sso", u.ssoStartGet)
	mux.HandleFunc("POST /ui/login/sso", u.auth(u.ssoStartPost))
	mux.HandleFunc("GET "+service.OIDCCallbackPath, u.ssoCallback)
	mux.HandleFunc("POST /ui/orgs/{org}/sso", u.auth(u.admin(u.orgSSOPost)))
	mux.HandleFunc("POST /ui/orgs/{org}/sso/remove", u.auth(u.admin(u.orgSSORemovePost)))
	mux.HandleFunc("POST /ui/admin/people/merge", u.auth(u.admin(u.mergePost)))
}

func (u *UI) ssoFail(w http.ResponseWriter, r *http.Request, status int, title, msg, retry string) {
	if retry == "" {
		retry = "/ui/login"
	}
	u.renderStatus(w, r, status, "sso", map[string]any{"Title": title, "Problem": gitHubProblem{Title: title, Message: msg, Retry: retry}})
}

// sessionActor is the signed-in actor of r, if there is one.
func (u *UI) sessionActor(r *http.Request) (service.Actor, bool) {
	c, err := r.Cookie(cookieName(r))
	if err != nil {
		return service.Actor{}, false
	}
	p, tokenID, err := u.svc.Store.AuthenticateToken(r.Context(), c.Value)
	if err != nil {
		return service.Actor{}, false
	}
	a, err := u.svc.ResolveActor(r.Context(), p)
	if err != nil {
		return service.Actor{}, false
	}
	a.TokenID = tokenID
	return a, true
}

// ssoStartGet starts a sign-in for ?org= or ?who= (a work email or an
// organization handle). Signed-in people are asked first.
func (u *UI) ssoStartGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	next := safeNext(q.Get("next"))
	org := q.Get("org")
	if org == "" {
		var err error
		if org, err = u.svc.SSOOrgFor(r.Context(), q.Get("who")); err != nil {
			u.ssoFail(w, r, errStatus(err), "Single sign-on", err.Error(), "/ui/login")
			return
		}
	}
	if a, ok := u.sessionActor(r); ok && a.Kind == store.KindHuman {
		u.render(w, r, "sso", map[string]any{"Title": "Single sign-on", "Confirm": map[string]string{"Org": org}, "Next": next, "Who": a.Ref()})
		return
	}
	u.ssoRedirect(w, r, oidcFlow{Org: org, Next: next})
}

// ssoStartPost starts a sign-in for the signed-in person: it connects the
// identity to them and marks their session.
func (u *UI) ssoStartPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	if actor.Kind != store.KindHuman {
		u.fail(w, &service.Error{Code: service.CodePermission, Msg: "agents do not sign in with single sign-on; sign in to the web UI as the person instead"})
		return
	}
	u.ssoRedirect(w, r, oidcFlow{Org: r.PostForm.Get("org"), Next: safeNext(r.PostForm.Get("next")), Person: actor.ID, Token: actor.TokenID})
}

func (u *UI) ssoRedirect(w http.ResponseWriter, r *http.Request, f oidcFlow) {
	st, err := u.svc.SSOSignInStart(r.Context(), f.Org)
	if err != nil {
		u.ssoFail(w, r, errStatus(err), "Could not start single sign-on", err.Error(), "/ui/login")
		return
	}
	f.Org, f.State, f.Nonce, f.Verifier, f.Expires = st.Org, st.State, st.Nonce, st.Verifier, time.Now().Add(gitHubFlowTTL).Unix()
	u.setOIDCCookie(w, r, u.sealOIDC(f), int(gitHubFlowTTL/time.Second))
	http.Redirect(w, r, st.URL, http.StatusFound)
}

func (u *UI) ssoCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	var flow oidcFlow
	var have bool
	if c, err := r.Cookie(oidcCookieName(r)); err == nil {
		flow, have = u.openOIDC(c.Value)
	}
	u.setOIDCCookie(w, r, "", -1) // one use per cookie
	retry := "/ui/login"
	if have {
		retry = "/ui/login/sso?org=" + url.QueryEscape(flow.Org) + "&next=" + url.QueryEscape(safeNext(flow.Next))
	}
	if e := q.Get("error"); e != "" {
		msg := "The identity provider did not sign you in (" + e + "). Nothing changed in vx."
		if d := q.Get("error_description"); d != "" {
			msg = "The identity provider did not sign you in: " + d + " (" + e + "). Nothing changed in vx."
		}
		if e == "access_denied" {
			msg = "You cancelled at the identity provider, or it does not let you use vx. Nothing changed. Ask your administrator to assign vx's app to you if you should have access."
		}
		u.ssoFail(w, r, http.StatusForbidden, "Not signed in", msg, retry)
		return
	}
	if !have || time.Now().Unix() > flow.Expires {
		u.ssoFail(w, r, http.StatusBadRequest, "Sign-in expired",
			"This sign-in was started in another browser, or more than 10 minutes ago, or was already used. Start again from this browser.", retry)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(flow.State)) != 1 {
		u.ssoFail(w, r, http.StatusBadRequest, "Sign-in did not match",
			"The identity provider came back with a different sign-in than the one this browser started, so vx stopped here to keep your account safe. Start again from this browser.", retry)
		return
	}
	res, err := u.svc.SSOSignInFinish(r.Context(), service.SSOFlow{Org: flow.Org, State: flow.State, Nonce: flow.Nonce, Verifier: flow.Verifier,
		LinkPerson: flow.Person, LinkToken: flow.Token}, q.Get("code"))
	var exists *service.AccountExistsError
	switch {
	case errors.As(err, &exists):
		next := "/ui/login/sso?org=" + url.QueryEscape(exists.Org) + "&next=" + url.QueryEscape(safeNext(flow.Next))
		u.renderStatus(w, r, http.StatusConflict, "sso", map[string]any{"Title": "This looks like you", "Exists": exists,
			"SignInLink": "/ui/login?next=" + url.QueryEscape(next)})
		return
	case err != nil:
		u.ssoFail(w, r, errStatus(err), "Could not sign you in", err.Error(), retry)
		return
	}
	if res.Token != "" {
		setSession(w, r, res.Token)
	}
	dest := safeNext(flow.Next)
	if res.IsNew && dest == "/ui" {
		dest = "/ui/welcome"
	}
	u.gitHubContinue(w, r, dest, res.Person.DisplayName)
}

// ssoRequired is the page for a member who must sign in through their
// organization's provider first.
func (u *UI) ssoRequired(w http.ResponseWriter, r *http.Request, e *service.SSORequiredError) {
	data := map[string]any{"Title": e.Org + " requires single sign-on", "Required": e, "Next": safeNext(r.URL.RequestURI()), "Server": u.publicURL(r)}
	if a, ok := actorOf(r); ok {
		data["Who"] = a.Ref()
	}
	u.renderStatus(w, r, http.StatusForbidden, "sso", data)
}

// reqWriter carries the request along with the response, so error pages
// rendered from u.fail know where the person was going.
type reqWriter struct {
	http.ResponseWriter
	r *http.Request
}

func (w *reqWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ssoRequiredFrom renders the single sign-on page when err asks for it and
// the request is known; it reports whether it did.
func (u *UI) ssoRequiredFrom(w http.ResponseWriter, err error) bool {
	var e *service.SSORequiredError
	rw, ok := w.(*reqWriter)
	if !ok || !errors.As(err, &e) {
		return false
	}
	u.ssoRequired(rw.ResponseWriter, rw.r, e)
	return true
}

// ---------- organization settings ----------

// orgSSO adds the single sign-on panel of an organization page.
func (u *UI) orgSSO(r *http.Request, actor service.Actor, myRole service.OrgRole, data map[string]any) {
	cfg, ok, err := u.svc.OrgSSO(r.Context(), actor, r.PathValue("org"))
	if err == nil && ok {
		data["SSO"] = cfg
		data["SSODomains"] = strings.Join(cfg.Domains, ", ")
	}
	data["SSOOwner"] = myRole == service.OrgOwner && actor.Kind == store.KindHuman
	if cb, err := u.svc.SSOCallbackURL(); err == nil {
		data["SSOCallback"] = cb
	}
	data["SSOKey"] = len(u.svc.GitHub.SecretKey) > 0
}

func (u *UI) orgSSOPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	f := r.PostForm
	_, err := u.svc.SetOrgSSO(r.Context(), actor, r.PathValue("org"), service.SSOConfigInput{
		Issuer: f.Get("issuer"), ClientID: f.Get("client_id"), ClientSecret: f.Get("client_secret"),
		Domains: []string{f.Get("domains")}, Require: f.Get("require") == "yes",
	})
	u.orgDone(w, r, actor, err, "sso", "sso")
}

func (u *UI) orgSSORemovePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	err := u.svc.RemoveOrgSSO(r.Context(), actor, r.PathValue("org"))
	u.orgDone(w, r, actor, err, "sso-removed", "sso")
}

// ---------- merging accounts (Admin > People) ----------

func (u *UI) mergePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if !u.svc.IsSuperadmin(r.Context(), actor) {
		http.NotFound(w, r)
		return
	}
	f := r.PostForm
	if _, err := u.svc.MergePeople(r.Context(), actor, handleOf(f.Get("keep")), handleOf(f.Get("merge"))); err != nil {
		u.renderPeople(w, r, actor, failed(err), f)
		return
	}
	http.Redirect(w, r, "/ui/admin/people?ok=merged#merge", http.StatusSeeOther)
}
