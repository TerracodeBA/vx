package web

// Deleting organizations (#40): the Delete organization section of an
// organization's page (owners), the page a deleted organization shows its
// owners (restore, deletion confirmation), and the confirmation download.

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
)

func (u *UI) mountOrgDelete(mux *http.ServeMux) {
	mux.HandleFunc("POST /ui/orgs/{org}/delete", u.auth(u.admin(u.orgDeletePost)))
	mux.HandleFunc("POST /ui/orgs/{org}/restore", u.auth(u.admin(u.orgRestorePost)))
	mux.HandleFunc("GET /ui/orgs/{org}/deletion-record", u.auth(u.orgDeletionRecord))
}

func longDate(t time.Time) string { return t.UTC().Format("2 January 2006, 15:04 UTC") }

// graceDays is the grace period in whole days, for the delete section.
func (u *UI) graceDays() int {
	g := u.svc.OrgDeletion.Grace
	switch {
	case g == 0:
		g = service.DefaultOrgDeleteGrace
	case g < 0:
		return 0
	}
	return int(g.Hours() / 24)
}

func (u *UI) orgDeletePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	_, err := u.svc.DeleteOrg(r.Context(), actor, r.PathValue("org"), r.PostForm.Get("confirm"))
	if err != nil {
		u.orgDone(w, r, actor, err, "", "")
		return
	}
	http.Redirect(w, r, "/ui/orgs/"+url.PathEscape(r.PathValue("org"))+"?ok=deleted", http.StatusSeeOther)
}

func (u *UI) orgRestorePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if _, err := u.svc.RestoreOrg(r.Context(), actor, r.PathValue("org")); err != nil {
		if !u.renderDeletedOrg(w, r, actor, failed(err)) {
			u.failPage(w, err)
		}
		return
	}
	http.Redirect(w, r, "/ui/orgs/"+url.PathEscape(r.PathValue("org"))+"?ok=restored", http.StatusSeeOther)
}

// renderDeletedOrg shows a deleted organization to its owners. It returns
// false when the caller is not one (the page is then a 404 like for any
// organization that does not exist).
func (u *UI) renderDeletedOrg(w http.ResponseWriter, r *http.Request, actor service.Actor, f flash) bool {
	d, err := u.svc.OrgDeletionRecord(r.Context(), actor, r.PathValue("org"))
	if err != nil {
		return false
	}
	restorable := d.PurgeStarted == nil && time.Now().Before(d.PurgeAfter)
	data := map[string]any{
		"Actor": actor, "Title": d.Org + " (deleted)", "Section": "orgs", "Flash": f,
		"D": d, "DeletedOn": longDate(d.DeletedAt), "PurgeOn": longDate(d.PurgeAfter),
		"Restorable": restorable, "Purging": d.PurgeStarted != nil,
	}
	u.renderStatus(w, r, f.Status, "org_deleted", data)
	return true
}

func (u *UI) orgDeletionRecord(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	d, err := u.svc.OrgDeletionRecord(r.Context(), actor, r.PathValue("org"))
	if err != nil {
		u.failPage(w, err)
		return
	}
	name := "vx-deletion-" + strings.ReplaceAll(d.Org, `"`, "") + ".txt"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(d.Confirmation(u.svc.GitHub.PublicURL)))
}

// deletedOrgRows are the caller's deleted organizations for the
// organizations page.
type deletedOrgRow struct {
	service.OrgDeletion
	PurgeOn string
	Purging bool
}

func (u *UI) deletedOrgRows(r *http.Request, actor service.Actor) []deletedOrgRow {
	ds, err := u.svc.DeletedOrgs(r.Context(), actor)
	if err != nil {
		return nil
	}
	out := make([]deletedOrgRow, 0, len(ds))
	for _, d := range ds {
		out = append(out, deletedOrgRow{OrgDeletion: d, PurgeOn: longDate(d.PurgeAfter), Purging: d.PurgeStarted != nil})
	}
	return out
}
