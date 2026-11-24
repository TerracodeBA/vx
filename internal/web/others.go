package web

import (
	"net/http"

	"github.com/TerracodeBA/vx/internal/service"
)

// otherEffortsShown and its friends cap the effort page's "Other efforts
// on these files" panel.
const (
	otherEffortsShown = 10
	otherFilesShown   = 8
	otherNotesShown   = 3
)

// otherEffortRow is another open effort of the repository that changed
// files this effort changed.
type otherEffortRow struct {
	Slug, Name, Owner string
	Files             []string
	MoreFiles         int
	Working           []service.ContextWorkspace
	Notes             []otherNoteRow
}

// otherNoteRow is an interface or decision note of another effort, with
// its trust label for the viewer.
type otherNoteRow struct {
	service.Note
	Trust string
}

// otherEfforts lists the other open efforts that changed the files this
// effort changed, from Context without targets. They are extra
// information: when they cannot be read the panel is left out.
func (u *UI) otherEfforts(r *http.Request, actor service.Actor, repo, slug string) []otherEffortRow {
	c, err := u.svc.Context(r.Context(), actor, repo, slug, nil)
	if err != nil {
		return nil
	}
	var out []otherEffortRow
	for i, o := range c.OtherEfforts {
		if i == otherEffortsShown {
			break
		}
		row := otherEffortRow{Slug: o.Effort.Slug, Name: o.Effort.Name, Owner: o.Owner.Ref(), Files: o.ChangedPaths, Working: o.Working}
		if len(row.Files) > otherFilesShown {
			row.Files, row.MoreFiles = row.Files[:otherFilesShown], len(row.Files)-otherFilesShown
		}
		for j, n := range o.Notes {
			if j == otherNotesShown {
				break
			}
			row.Notes = append(row.Notes, otherNoteRow{Note: n, Trust: service.TrustOf(actor, n)})
		}
		out = append(out, row)
	}
	return out
}
