package web

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/service"
)

// taskItem is a task as the Tasks tab shows it.
type taskItem struct {
	service.Task
	Trust     string // relative to the viewer
	CreatedBy string
	For       string // the person an agent creator acts for
	Owner     string
	WaitsFor  []taskRef // tasks it depends on that are not done
}

type taskRef struct {
	ID    string
	Short string
	Title string
}

// tasksView is the Tasks tab: open tasks (blocked ones marked with what
// they wait for), taken ones with their owner, and done ones.
type tasksView struct {
	Open, Taken, Done []taskItem
	Active            int // open and taken: the tab's count
	Err               string
}

func (u *UI) tasks(r *http.Request, actor service.Actor, repoName, slug string) tasksView {
	var tv tasksView
	ts, err := u.svc.ListTasks(r.Context(), actor, service.ListTasksInput{Repo: repoName, Effort: slug})
	if err != nil {
		tv.Err = err.Error()
		return tv
	}
	byID := map[uuid.UUID]service.Task{}
	for _, t := range ts {
		byID[t.ID] = t
	}
	for _, t := range ts {
		it := taskItem{Task: t, Trust: service.TaskTrust(actor, t), CreatedBy: t.CreatedBy.Ref()}
		if t.CreatedParent != nil {
			it.For = t.CreatedParent.Ref()
		}
		if t.Owner != nil {
			it.Owner = t.Owner.Ref()
		}
		for _, d := range t.DependsOn {
			dep, ok := byID[d]
			if ok && dep.State == service.TaskDone {
				continue
			}
			ref := taskRef{ID: d.String(), Short: d.String()[:8]}
			if ok {
				ref.Title = dep.Title
			}
			it.WaitsFor = append(it.WaitsFor, ref)
		}
		switch t.State {
		case service.TaskOpen:
			tv.Open = append(tv.Open, it)
		case service.TaskTaken:
			tv.Taken = append(tv.Taken, it)
		default:
			tv.Done = append(tv.Done, it)
		}
	}
	tv.Active = len(tv.Open) + len(tv.Taken)
	return tv
}
