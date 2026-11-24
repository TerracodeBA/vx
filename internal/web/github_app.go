package web

// The GitHub App: Admin > GitHub (superadmins create the App through
// GitHub's manifest flow), the pages GitHub sends the browser back to, and
// the webhook endpoint. The manifest form is a plain form that posts to
// GitHub; no script is involved, so the strict CSP holds.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// maxWebhookBody is GitHub's limit on webhook payloads.
const maxWebhookBody = 25 << 20

func (u *UI) mountGitHubApp(mux *http.ServeMux) error {
	for page, file := range map[string]string{"github_admin": "github_admin.html", "github_return": "github_return.html"} {
		t, err := u.pages["login"].Clone()
		if err == nil {
			t, err = t.ParseFS(assets, "templates/"+file)
		}
		if err != nil {
			return fmt.Errorf("template %s: %w", page, err)
		}
		u.pages[page] = t
	}
	mux.HandleFunc("GET /ui/admin/github", u.auth(u.githubAdminPage))
	mux.HandleFunc("GET "+service.GitHubManifestReturnURL, u.githubCreatedPage)
	mux.HandleFunc("POST /ui/admin/github/finish", u.auth(u.admin(u.githubFinishPost)))
	mux.HandleFunc("GET "+service.GitHubSetupURLPath, u.githubInstalledPage)
	mux.HandleFunc("POST "+service.GitHubWebhookPath, u.githubWebhook)
	u.mountHostHooks(mux)
	return nil
}

func (u *UI) githubAdminPage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	u.renderGitHubAdmin(w, r, actor, flash{})
}

func (u *UI) renderGitHubAdmin(w http.ResponseWriter, r *http.Request, actor service.Actor, f flash) {
	ctx := r.Context()
	if !u.svc.IsSuperadmin(ctx, actor) {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("created") != "" && f.OK == "" && f.Err == "" {
		f.OK = "The GitHub App is set up. Install it on the organizations whose repositories vx should work on."
	}
	data := map[string]any{"Actor": actor, "Title": "GitHub", "Section": "admin", "Flash": f,
		"IsHuman": actor.Kind == store.KindHuman, "WebURL": u.svc.GitHubWebURL(),
		"SuperadminReview": u.svc.SuperadminReviewPending(ctx, actor)}
	app, err := u.svc.GitHubApp(ctx)
	switch {
	case err == nil:
		data["App"] = app
		data["Gaps"], _ = u.svc.GitHubAppGaps(ctx, actor)
	case !errors.Is(err, service.ErrGitHubNotConfigured):
		data["LoadErr"] = err.Error()
	}
	org := strings.TrimSpace(r.URL.Query().Get("org"))
	data["Org"] = org
	if actor.Kind == store.KindHuman {
		manifest, state, err := u.svc.GitHubSetupManifest(ctx, actor)
		if err != nil {
			data["SetupErr"] = err.Error()
		} else {
			data["Manifest"] = string(manifest)
			data["FormURL"] = u.svc.GitHubManifestFormURL(org, state)
		}
	}
	if f.Status != 0 {
		w.WriteHeader(f.Status)
	}
	u.render(w, r, "github_admin", data)
}

// githubCreatedPage is where GitHub sends the browser after creating the
// App. The session cookie is SameSite=Strict, so it does not come along on
// this cross-site navigation: the page asks to finish with one click, a
// same-site POST that carries the session.
func (u *UI) githubCreatedPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u.render(w, r, "github_return", map[string]any{"Title": "GitHub App created", "Created": true,
		"Code": q.Get("code"), "State": q.Get("state")})
}

func (u *UI) githubFinishPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	err := u.svc.CompleteGitHubSetup(r.Context(), actor, r.PostForm.Get("code"), r.PostForm.Get("state"))
	if err != nil {
		u.renderGitHubAdmin(w, r, actor, flash{Status: errStatus(err), Err: err.Error()})
		return
	}
	http.Redirect(w, r, "/ui/admin/github?created=1", http.StatusSeeOther)
}

// githubInstalledPage is the App's setup URL: GitHub sends the browser
// here after an installation or a change to it. Webhooks bring the
// installation in too; syncing here makes it show up at once.
func (u *UI) githubInstalledPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Title": "GitHub App installed", "Installed": true}
	// Only installations vx does not know yet: the page needs no session,
	// so it must not let anyone make vx ask GitHub again and again.
	if id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64); err == nil && id > 0 && !u.knownInstallation(r, id) {
		if err := u.svc.SyncInstallation(r.Context(), id); err != nil {
			u.log.Warn("github installation sync from setup page", "installation", id, "err", err)
			data["SyncErr"] = "vx could not read this installation from GitHub yet; it will show up when GitHub's webhook arrives."
		}
	}
	u.render(w, r, "github_return", data)
}

// githubWebhook receives GitHub's webhook deliveries. The service checks
// the signature; nothing here trusts the body before that.
func (u *UI) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "payload too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	err = u.svc.HandleGitHubWebhook(r.Context(), event, delivery, r.Header.Get("X-Hub-Signature-256"), body)
	status, msg := http.StatusOK, "ok"
	var se *service.Error
	switch {
	case err == nil:
	case errors.Is(err, service.ErrGitHubNotConfigured):
		status, msg = http.StatusServiceUnavailable, err.Error()
	case errors.As(service.Classify(err), &se):
		switch se.Code {
		case service.CodeExists: // a replay of a delivery already applied
			status, msg = http.StatusOK, se.Msg
		case service.CodePermission:
			status, msg = http.StatusUnauthorized, se.Msg
		case service.CodeInvalid, service.CodeFailedPrecondition:
			status, msg = http.StatusBadRequest, se.Msg
		default:
			status, msg = http.StatusInternalServerError, se.Msg
		}
	default:
		status, msg = http.StatusInternalServerError, "could not apply the delivery"
	}
	if status >= 500 || status == http.StatusUnauthorized {
		u.log.Warn("github webhook", "event", event, "delivery", delivery, "status", status, "err", err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintln(w, msg)
}

func (u *UI) knownInstallation(r *http.Request, id int64) bool {
	_, err := u.svc.Store.GitHubInstallationByID(r.Context(), id)
	return err == nil
}
