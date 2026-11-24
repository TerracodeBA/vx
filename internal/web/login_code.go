package web

import (
	"net/http"
	"strings"
)

// Sign-in through a one-time code from `vx ui`. The link opens a page that
// names the account; one click signs in. Requiring the click (a POST) means a
// link someone else sends you cannot sign you in to their account silently.

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/ui") || strings.HasPrefix(next, "//") {
		return "/ui"
	}
	return next
}

func (u *UI) codePage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("c")
	next := safeNext(r.URL.Query().Get("next"))
	data := map[string]any{"Title": "Sign in", "Code": code, "Next": next}
	p, err := u.svc.PeekLoginCode(r.Context(), code)
	if err != nil {
		data["Error"] = err.Error()
	} else {
		data["Who"] = p.Ref()
	}
	w.Header().Set("Referrer-Policy", "no-referrer") // keep the code out of other sites' logs
	u.render(w, r, "code", data)
}

func (u *UI) codePost(w http.ResponseWriter, r *http.Request) {
	if !u.checkOrigin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	next := safeNext(r.PostForm.Get("next"))
	_, token, err := u.svc.RedeemLoginCode(r.Context(), r.PostForm.Get("code"))
	if err != nil {
		u.render(w, r, "code", map[string]any{"Title": "Sign in", "Next": next, "Error": err.Error()})
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, next, http.StatusSeeOther)
}
