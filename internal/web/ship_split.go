package web

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/TerracodeBA/vx/internal/service"
)

// ---------- prepare pull requests (a split) ----------
//
// The ship page with ?split=independent|stacked shows vx's grouping of the
// effort's changes into parts, one pull request each. A person regroups
// with a plain form: every change has a part number to pick, and each part
// has buttons to merge it into the part before or to split it into one
// part per change. Every button posts the whole grouping, the page answers
// with the plan for it, and "Open pull requests" ships it. No JavaScript.

// splitPart is one part as the form shows it.
type splitPart struct {
	service.ShipGroup
	Before int // the part number above this one
}

// shipSplit renders the Prepare pull requests page for a grouping (nil:
// vx's suggestion).
func (u *UI) shipSplit(w http.ResponseWriter, r *http.Request, actor service.Actor, repoName, slug, target string, mode service.SplitMode, groups [][]string, extra map[string]any) {
	ctx := r.Context()
	v, err := u.svc.GetEffort(ctx, actor, repoName, slug)
	if err != nil {
		u.fail(w, err)
		return
	}
	data := map[string]any{"Actor": actor, "Repo": v.Repo, "Effort": v.Effort, "Title": "Prepare pull requests for " + v.Effort.Name,
		"Split": string(mode), "Target": target}
	for k, val := range extra {
		data[k] = val
	}
	plan, err := u.svc.PlanSplit(ctx, actor, service.SplitInput{Repo: repoName, Effort: slug, Target: target, Mode: mode, Groups: groups})
	if err != nil {
		data["PlanError"] = err.Error()
	} else {
		data["Plan"] = plan
		var parts []splitPart
		for _, g := range plan.Groups {
			parts = append(parts, splitPart{ShipGroup: g, Before: g.Part - 1})
		}
		data["Parts"] = parts
		data["NewPart"] = len(plan.Groups) + 1
	}
	data["Shipments"], _ = u.svc.ListShipments(ctx, actor, repoName, slug)
	data["Shipped"] = r.URL.Query().Get("shipped") != ""
	u.render(w, r, "ship", data)
}

// shipSplitPost handles every button of the Prepare pull requests form.
func (u *UI) shipSplitPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	f := r.PostForm
	repoName, slug, target := f.Get("repo"), f.Get("slug"), f.Get("to")
	mode, err := service.ParseSplitMode(f.Get("split"))
	if err != nil {
		u.fail(w, err)
		return
	}
	if mode == service.SplitNone {
		http.Redirect(w, r, "/ui/ship?repo="+url.QueryEscape(repoName)+"&slug="+url.QueryEscape(slug), http.StatusSeeOther)
		return
	}
	groups := formGroups(f)
	op, arg, _ := strings.Cut(f.Get("op"), ":")
	k, _ := strconv.Atoi(arg)
	switch op {
	case "merge": // part k into part k-1
		if k >= 2 && k <= len(groups) {
			groups[k-2] = append(groups[k-2], groups[k-1]...)
			groups = append(groups[:k-1], groups[k:]...)
		}
	case "split": // part k into one part per change
		if k >= 1 && k <= len(groups) && len(groups[k-1]) > 1 {
			var each [][]string
			for _, id := range groups[k-1] {
				each = append(each, []string{id})
			}
			rest := append(each, groups[k:]...)
			groups = append(groups[:k-1:k-1], rest...)
		}
	case "up": // part k above part k-1 (stacked order)
		if k >= 2 && k <= len(groups) {
			groups[k-2], groups[k-1] = groups[k-1], groups[k-2]
		}
	case "open":
		_, err := u.svc.OpenShipment(r.Context(), actor, service.ShipInput{
			Repo: repoName, Effort: slug, Target: target, Draft: f.Get("draft") == "on",
			Split: mode, Groups: groups,
		})
		if err != nil {
			u.shipSplit(w, r, actor, repoName, slug, target, mode, groups, map[string]any{"OpenError": err.Error()})
			return
		}
		http.Redirect(w, r, "/ui/ship?repo="+url.QueryEscape(repoName)+"&slug="+url.QueryEscape(slug)+"&split="+string(mode)+
			"&to="+url.QueryEscape(target)+"&shipped=1", http.StatusSeeOther)
		return
	}
	u.shipSplit(w, r, actor, repoName, slug, target, mode, groups, map[string]any{"Checked": true})
}

// formGroups reads the grouping from the form: "change" lists the change
// ids in the order shown, "part.<id>" the part each is in. Parts keep
// their numbers' order; numbers with no change are dropped.
func formGroups(f url.Values) [][]string {
	byPart := map[int][]string{}
	for _, id := range f["change"] {
		n, err := strconv.Atoi(f.Get("part." + id))
		if err != nil || n < 1 {
			n = 1
		}
		byPart[n] = append(byPart[n], id)
	}
	var nums []int
	for n := range byPart {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var out [][]string
	for _, n := range nums {
		out = append(out, byPart[n])
	}
	return out
}
