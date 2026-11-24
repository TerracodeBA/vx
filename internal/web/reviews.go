package web

import (
	"net/http"
	"net/url"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// ---------- reviews (landing policies) ----------

// reviewURL is the change page of a review.
func reviewURL(r service.Review) string {
	return "/ui/change?repo=" + url.QueryEscape(r.RepoName) + "&review=" + url.QueryEscape(r.ID.String())
}

// reviews lists the changes waiting for a person in the repositories the
// viewer can read, and the latest decided ones.
func (u *UI) reviews(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	pending, err := u.svc.ListReviews(ctx, actor, service.ReviewQuery{})
	if err != nil {
		u.fail(w, err)
		return
	}
	decided, err := u.svc.ListReviews(ctx, actor, service.ReviewQuery{States: []store.ReviewState{store.ReviewApproved, store.ReviewRejected}, Limit: 20})
	if err != nil {
		u.fail(w, err)
		return
	}
	mine := 0
	for _, rv := range pending {
		if rv.CanDecide {
			mine++
		}
	}
	u.render(w, r, "reviews", map[string]any{
		"Actor": actor, "Pending": pending, "Decided": decided, "Mine": mine, "Title": "Reviews", "Section": "reviews",
	})
}

// reviewChange renders a pending (or decided) review on the change page:
// the snapshot's files against its base, with Approve and Reject.
func (u *UI) reviewChange(w http.ResponseWriter, r *http.Request, actor service.Actor, id string) {
	ctx := r.Context()
	rv, err := u.svc.GetReview(ctx, actor, id)
	if err != nil {
		u.fail(w, err)
		return
	}
	repo, _, err := u.svc.GetRepo(ctx, actor, rv.RepoName)
	if err != nil {
		u.fail(w, err)
		return
	}
	eff, err := u.svc.GetEffort(ctx, actor, rv.RepoName, rv.EffortSlug)
	if err != nil {
		u.fail(w, err)
		return
	}
	read, err := u.svc.BlobReader(ctx, actor, rv.RepoID)
	if err != nil {
		u.fail(w, err)
		return
	}
	var diffs []fileDiff
	var added, deleted int
	for _, f := range rv.Files {
		fd, err := u.fileDiff(ctx, read, f)
		if err != nil {
			u.fail(w, err)
			return
		}
		added += f.Added
		deleted += f.Deleted
		diffs = append(diffs, fd)
	}
	onBehalf := ""
	if rv.AuthorOwner != nil {
		onBehalf = rv.AuthorOwner.Ref()
	}
	// The change as it would land: the author's snapshot, its evidence.
	c := store.ChangesetRow{Changeset: &object.Changeset{
		ChangeID: rv.ChangeID, Tree: rv.Tree, Intent: rv.Intent, Evidence: rv.Evidence, DependsOn: rv.DependsOn,
		Author:    object.Author{Principal: rv.Author.Ref(), OnBehalfOf: onBehalf, Model: rv.Model, Session: rv.Session},
		CreatedAt: rv.CreatedAt, Parents: []hash.Hash{rv.Base},
	}}
	u.render(w, r, "change", map[string]any{
		"Actor": actor, "Repo": repo.Repo, "C": c, "Effort": &eff.Effort, "Diffs": diffs,
		"Added": added, "Deleted": deleted, "Title": c.Title(), "Review": rv,
	})
}

func (u *UI) reviewPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
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
	var d service.ReviewDecision
	var err error
	switch r.PostForm.Get("decision") {
	case "approve":
		d, err = u.svc.ApproveReview(r.Context(), actor, id)
	case "reject":
		d, err = u.svc.RejectReview(r.Context(), actor, id, r.PostForm.Get("reason"))
	default:
		err = &service.Error{Code: service.CodeInvalid, Msg: "decide approve or reject"}
	}
	if err != nil {
		u.fail(w, err)
		return
	}
	http.Redirect(w, r, reviewURL(d.Review)+"&decided=1", http.StatusSeeOther)
}
