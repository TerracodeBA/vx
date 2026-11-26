package web

import (
	"net/http"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// repoListRow is one repository on the Repositories page.
type repoListRow struct {
	service.RepoView
	OpenEfforts int
	Working     int       // open workspaces in its open efforts
	Activity    time.Time // latest effort update; zero when none
}

// repos lists every repository the caller can read. The sidebar has a
// single Repositories entry that opens this page instead of listing every
// repository and its efforts on each request.
func (u *UI) repos(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	repos, err := u.svc.ListRepos(ctx, actor)
	if err != nil {
		u.fail(w, err)
		return
	}
	rows := make([]repoListRow, 0, len(repos))
	working, efforts, github := 0, 0, 0
	for _, repo := range repos {
		row := repoListRow{RepoView: repo}
		if repo.GitHubBacked() {
			github++
		}
		if _, effs, err := u.svc.ListEfforts(ctx, actor, repo.Name, ""); err == nil {
			for _, e := range effs {
				if e.UpdatedAt.After(row.Activity) {
					row.Activity = e.UpdatedAt
				}
				if e.State != store.EffortOpen {
					continue
				}
				row.OpenEfforts++
				if ws, err := u.svc.Store.ListWorkspaces(ctx, e.ID, store.WorkspaceOpen); err == nil {
					row.Working += len(ws)
				}
			}
		}
		working += row.Working
		efforts += row.OpenEfforts
		rows = append(rows, row)
	}
	data := map[string]any{
		"Actor": actor, "Title": "Repositories", "Rows": rows,
		"OpenEfforts": efforts, "Working": working, "GitHub": github,
	}
	if len(repos) == 0 {
		data["Access"] = u.accessHelp(r, actor)
	}
	u.render(w, r, "repos", data)
}

// accessHelp explains an account that reads no repositories (#44): which
// account is signed in, why it sees nothing and what to do. Callers show
// it only when the list of readable repositories came back empty. nil when
// vx cannot say, and the page falls back to its plain empty state.
func (u *UI) accessHelp(r *http.Request, actor service.Actor) *service.AccessHelp {
	h, err := u.svc.AccessHelp(r.Context(), actor)
	if err != nil {
		u.log.Warn("access help", "actor", actor.Ref(), "err", err)
		return nil
	}
	return &h
}
