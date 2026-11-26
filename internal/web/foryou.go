package web

import (
	"net/http"
	"net/url"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// ---------- For you (v0.5, #37) ----------

// forYouRow is one item of the For you page, with what the template needs.
type forYouRow struct {
	service.ForYouItem
	Label     string // what kind of thing it is, for people
	Tone      string // pill class: bad, warn, ok or ""
	Anchor    string // the item's id on the page
	RepoName  string
	Effort    string
	Link      string // where to see it in full
	CanAnswer bool
}

var forYouLabels = map[string][2]string{
	service.ForYouQuestion: {"Question", "warn"},
	service.ForYouReview:   {"Review", "warn"},
	service.ForYouCI:       {"CI failed", "bad"},
	service.ForYouComment:  {"PR review", ""},
	service.ForYouOverlap:  {"Overlap", "warn"},
	service.ForYouRebase:   {"Needs rebase", "bad"},
	// An agreement item is "bad" when it blocks work, "warn" when announced.
	service.ForYouAgreement: {"Agreement", "warn"},
}

func forYouRows(f service.ForYou, actor service.Actor) []forYouRow {
	rows := make([]forYouRow, 0, len(f.Items))
	for _, it := range f.Items {
		r := forYouRow{ForYouItem: it}
		l := forYouLabels[it.Kind]
		r.Label, r.Tone = l[0], l[1]
		switch {
		case it.Agreement != nil:
			r.Anchor = "a-" + it.Agreement.Org + "-" + it.Agreement.Version.ID
			r.Link = "/ui/orgs/" + url.PathEscape(it.Agreement.Org) + "#agreement"
			if it.Agreement.Blocking {
				r.Tone = "bad"
			}
		case it.Review != nil:
			r.Anchor = "r-" + it.Review.ID.String()
			r.RepoName, r.Effort = it.Review.RepoName, it.Review.EffortSlug
			r.Link = reviewURL(*it.Review)
		case it.Note != nil:
			n := it.Note
			r.Anchor = "n-" + n.ID.String()
			r.RepoName, r.Effort = n.RepoName, n.EffortSlug
			r.Link = "/ui/effort?repo=" + url.QueryEscape(n.RepoName) + "&slug=" + url.QueryEscape(n.EffortSlug) + "&view=board#n-" + n.ID.String()
			if it.Kind == service.ForYouQuestion && n.State == service.NoteEscalated {
				r.Label, r.Tone = "Escalated question", "bad"
			}
			r.CanAnswer = it.Kind == service.ForYouQuestion && actor.Kind == store.KindHuman
		}
		rows = append(rows, r)
	}
	return rows
}

// forYou lists what waits for the viewer, newest first, with answer and
// approve forms in place.
func (u *UI) forYou(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	f, err := u.svc.ForYou(r.Context(), actor)
	if err != nil {
		u.fail(w, err)
		return
	}
	flash := ""
	switch param(r, "done") {
	case "answered":
		flash = "Answer posted. The agent that asked gets it with vx inbox, vx wait or its next sync."
	case "approved":
		flash = "Approved. The change landed in its effort."
	case "rejected":
		flash = "Rejected. The author's agent sees why in vx inbox."
	}
	rows := forYouRows(f, actor)
	data := map[string]any{
		"Actor": actor, "Title": "For you", "Section": "foryou", "Rows": rows, "Counts": f.Counts,
		"Flash": flash, "IsHuman": actor.Kind == store.KindHuman,
		"OnEfforts":   f.Counts[service.ForYouOverlap] + f.Counts[service.ForYouRebase],
		"HumanChecks": u.humanChecks(r, actor),
	}
	if len(rows) == 0 {
		if ids, _, err := u.svc.ReadableRepoIDs(r.Context(), actor); err == nil && len(ids) == 0 {
			data["Access"] = u.accessHelp(r, actor)
		}
	}
	u.render(w, r, "foryou", data)
}

// forYouAnswerPost answers a question from the For you page.
func (u *UI) forYouAnswerPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	if actor.Kind != store.KindHuman {
		u.fail(w, &service.Error{Code: service.CodePermission, Msg: "only people answer questions in the web UI; agents answer with vx answer"})
		return
	}
	if _, _, err := u.svc.AnswerNote(r.Context(), actor, r.PostForm.Get("note"), r.PostForm.Get("body")); err != nil {
		u.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/for-you?done=answered", http.StatusSeeOther)
}

// forYouReviewPost approves or rejects a review from the For you page.
func (u *UI) forYouReviewPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	if actor.Kind != store.KindHuman {
		u.fail(w, &service.Error{Code: service.CodePermission, Msg: "only people approve or reject reviews"})
		return
	}
	id := r.PostForm.Get("review")
	var err error
	done := ""
	switch r.PostForm.Get("decision") {
	case "approve":
		_, err = u.svc.ApproveReview(r.Context(), actor, id)
		done = "approved"
	case "reject":
		_, err = u.svc.RejectReview(r.Context(), actor, id, r.PostForm.Get("reason"))
		done = "rejected"
	default:
		err = &service.Error{Code: service.CodeInvalid, Msg: "decide approve or reject"}
	}
	if err != nil {
		u.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/for-you?done="+done, http.StatusSeeOther)
}
