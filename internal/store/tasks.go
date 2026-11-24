package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrCycle is returned when a task dependency would make tasks wait for
// each other.
var ErrCycle = errors.New("dependency cycle")

// Task is a stored task of an effort's plan. DependsOn and Blocked are read
// from task_deps: Blocked is set while some task it depends on is not done.
type Task struct {
	ID         uuid.UUID
	Seq        int64
	RepoID     uuid.UUID
	EffortID   uuid.UUID
	Title      string
	Body       string
	State      string // open, taken, done
	OwnerID    *uuid.UUID
	CreatedBy  uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DependsOn  []uuid.UUID // in the order the tasks were created
	Blocked    bool
	RepoName   string
	EffortSlug string
}

const taskCols = `t.id, t.seq, t.repo_id, t.effort_id, t.title, t.body, t.state, t.owner_id, t.created_by,
	t.created_at, t.updated_at,
	coalesce((select array_agg(d.depends_on_id order by dt.created_at, dt.seq) from task_deps d
		join tasks dt on dt.id = d.depends_on_id where d.task_id = t.id), '{}'),
	exists (select 1 from task_deps d join tasks dt on dt.id = d.depends_on_id where d.task_id = t.id and dt.state <> 'done'),
	r.name, e.slug`

const taskFrom = ` from tasks t join efforts e on e.id = t.effort_id join repos r on r.id = t.repo_id `

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Seq, &t.RepoID, &t.EffortID, &t.Title, &t.Body, &t.State, &t.OwnerID, &t.CreatedBy,
		&t.CreatedAt, &t.UpdatedAt, &t.DependsOn, &t.Blocked, &t.RepoName, &t.EffortSlug)
	if t.DependsOn == nil {
		t.DependsOn = []uuid.UUID{}
	}
	return t, err
}

func (q Q) queryTasks(ctx context.Context, sql string, args ...any) ([]Task, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// InsertTask stores a task. A zero ID gets a new one, an empty State is
// open and a zero CreatedAt means now. The repo is taken from the effort.
// Dependencies are added separately with AddTaskDeps.
func (q Q) InsertTask(ctx context.Context, t Task) (Task, error) {
	if t.ID == uuid.Nil {
		t.ID = NewID()
	}
	if t.State == "" {
		t.State = "open"
	}
	var created *time.Time
	if !t.CreatedAt.IsZero() {
		created = &t.CreatedAt
	}
	tag, err := q.db.Exec(ctx, `insert into tasks (id, repo_id, effort_id, title, body, state, owner_id, created_by, created_at, updated_at)
		select $1::uuid, e.repo_id, e.id, $3::text, $4::text, $5::text, $6::uuid, $7::uuid, coalesce($8::timestamptz, now()), coalesce($8::timestamptz, now())
		from efforts e where e.id = $2::uuid`,
		t.ID, t.EffortID, t.Title, t.Body, t.State, t.OwnerID, t.CreatedBy, created)
	if err != nil {
		return Task{}, err
	}
	if tag.RowsAffected() == 0 {
		return Task{}, fmt.Errorf("effort: %w", ErrNotFound)
	}
	return q.TaskByID(ctx, t.ID)
}

// AddTaskDeps makes task wait for each of deps. It returns ErrCycle, adding
// nothing, when one of deps already waits (directly or not) for task.
// Callers run it in a transaction with the task's other changes.
func (q Q) AddTaskDeps(ctx context.Context, task uuid.UUID, deps []uuid.UUID) error {
	if len(deps) == 0 {
		return nil
	}
	var cycle bool
	err := q.db.QueryRow(ctx, `with recursive waits(id) as (
			select unnest($2::uuid[])
			union
			select d.depends_on_id from task_deps d join waits w on d.task_id = w.id
		) select exists (select 1 from waits where id = $1)`, task, deps).Scan(&cycle)
	if err != nil {
		return err
	}
	if cycle {
		return fmt.Errorf("task %s: %w", task, ErrCycle)
	}
	_, err = q.db.Exec(ctx, `insert into task_deps (task_id, depends_on_id) select $1, unnest($2::uuid[]) on conflict do nothing`, task, deps)
	return err
}

// TaskByID loads a task.
func (q Q) TaskByID(ctx context.Context, id uuid.UUID) (Task, error) {
	t, err := scanTask(q.db.QueryRow(ctx, `select `+taskCols+taskFrom+`where t.id = $1`, id))
	return t, notFound(err, "task "+id.String())
}

// TaskForUpdate loads a task and locks its row until the transaction ends.
func (q Q) TaskForUpdate(ctx context.Context, id uuid.UUID) (Task, error) {
	t, err := scanTask(q.db.QueryRow(ctx, `select `+taskCols+taskFrom+`where t.id = $1 for update of t`, id))
	return t, notFound(err, "task "+id.String())
}

// TaskByPrefix finds the one task whose id starts with prefix, within one
// effort unless effort is uuid.Nil. It returns ErrNotFound or ErrAmbiguous
// otherwise.
func (q Q) TaskByPrefix(ctx context.Context, effort uuid.UUID, prefix string) (Task, error) {
	prefix = strings.ToLower(prefix)
	for _, c := range prefix {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c == '-') {
			return Task{}, fmt.Errorf("task %q: %w", prefix, ErrNotFound)
		}
	}
	var eff *uuid.UUID
	if effort != uuid.Nil {
		eff = &effort
	}
	tasks, err := q.queryTasks(ctx, `select `+taskCols+taskFrom+`where t.id::text like $1 || '%' and ($2::uuid is null or t.effort_id = $2) limit 2`, prefix, eff)
	if err != nil {
		return Task{}, err
	}
	switch len(tasks) {
	case 0:
		return Task{}, fmt.Errorf("task %s: %w", prefix, ErrNotFound)
	case 1:
		return tasks[0], nil
	}
	return Task{}, fmt.Errorf("task prefix %s: %w", prefix, ErrAmbiguous)
}

// TaskFilter selects an effort's tasks. Zero fields do not filter.
type TaskFilter struct {
	EffortID uuid.UUID
	States   []string
	Owners   []uuid.UUID
}

// ListTasks lists an effort's tasks: not done first, then by creation.
func (q Q) ListTasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	return q.queryTasks(ctx, `select `+taskCols+taskFrom+`where t.effort_id = $1
		and (coalesce(cardinality($2::text[]), 0) = 0 or t.state = any($2))
		and (coalesce(cardinality($3::uuid[]), 0) = 0 or t.owner_id = any($3))
		order by t.state = 'done', t.created_at, t.seq`, f.EffortID, f.States, f.Owners)
}

// TaskDependents lists the tasks that wait for task, by creation.
func (q Q) TaskDependents(ctx context.Context, task uuid.UUID) ([]Task, error) {
	return q.queryTasks(ctx, `select `+taskCols+taskFrom+`join task_deps d on d.task_id = t.id where d.depends_on_id = $1
		order by t.created_at, t.seq`, task)
}

// SetTaskState changes a task's state and owner.
func (q Q) SetTaskState(ctx context.Context, id uuid.UUID, state string, owner *uuid.UUID, now time.Time) error {
	tag, err := q.db.Exec(ctx, `update tasks set state = $2, owner_id = $3, updated_at = $4 where id = $1`, id, state, owner, now)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	return err
}
