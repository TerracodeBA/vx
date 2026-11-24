package service

// Tasks: an effort's shared plan, implementing the task part of the
// contract in coordination.go. A task is open, taken by one owner, or done;
// it reads as blocked while a task it depends on is not done. Dependencies
// are fixed when a task is added.

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// Task actions for UpdateTask.
const (
	actTake    = "take"
	actRelease = "release"
	actDone    = "done"
	actReopen  = "reopen"
	actAssign  = "assign"
)

// maxTaskDeps caps the tasks one task can wait for.
const maxTaskDeps = 50

// AddTask adds a task to an effort's plan.
func (s *Service) AddTask(ctx context.Context, actor Actor, in AddTaskInput) (Task, error) {
	title, err := cleanTaskTitle(in.Title)
	if err != nil {
		return Task{}, err
	}
	body, err := cleanTaskBody(in.Body)
	if err != nil {
		return Task{}, err
	}
	v, err := s.boardEffort(ctx, in.Repo, in.Effort)
	if err != nil {
		return Task{}, err
	}
	var deps []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, d := range in.DependsOn {
		if strings.TrimSpace(d) == "" {
			continue
		}
		t, err := s.findTask(ctx, v.Effort.ID, d)
		if err != nil {
			return Task{}, err
		}
		if !seen[t.ID] {
			seen[t.ID] = true
			deps = append(deps, t.ID)
		}
	}
	if len(deps) > maxTaskDeps {
		return Task{}, errf(CodeInvalid, "the task waits for %d tasks; give at most %d, or split it", len(deps), maxTaskDeps)
	}
	row := store.Task{EffortID: v.Effort.ID, Title: title, Body: body, State: TaskOpen, CreatedBy: actor.ID, CreatedAt: s.Now()}
	after := map[string]any{"effort": v.Effort.Slug, "title": title, "depends_on": deps}
	if strings.TrimSpace(in.Owner) != "" {
		owner, err := s.resolvePrincipal(ctx, actor, in.Owner)
		if err != nil {
			return Task{}, err
		}
		row.State, row.OwnerID = TaskTaken, &owner.ID
		after["owner"] = owner.Ref()
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if row, err = tx.InsertTask(ctx, row); err != nil {
			return err
		}
		if err := tx.AddTaskDeps(ctx, row.ID, deps); err != nil {
			return err
		}
		if row, err = tx.TaskByID(ctx, row.ID); err != nil {
			return err
		}
		return tx.LogOp(ctx, &row.RepoID, &actor.ID, "task.add", row.ID.String(), nil, after)
	})
	if errors.Is(err, store.ErrCycle) {
		return Task{}, errf(CodeInvalid, "these dependencies would make tasks wait for each other; drop one of --after")
	}
	if err != nil {
		return Task{}, classify(err)
	}
	return s.newNoteCache().task(ctx, row)
}

// UpdateTask applies an action to a task and returns it, plus the tasks a
// done unblocked.
func (s *Service) UpdateTask(ctx context.Context, actor Actor, in UpdateTaskInput) (Task, []Task, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	switch action {
	case actTake, actRelease, actDone, actReopen:
	case actAssign:
		if strings.TrimSpace(in.Owner) == "" {
			return Task{}, nil, errf(CodeInvalid, "say who to assign the task to: an agent name, handle or ref")
		}
	default:
		return Task{}, nil, errf(CodeInvalid, "task action %q is not one of take, release, done, reopen, assign", in.Action)
	}
	found, err := s.findTask(ctx, uuid.Nil, in.ID)
	if err != nil {
		return Task{}, nil, err
	}
	c := s.newNoteCache()
	var assignee *store.Principal
	if action == actAssign {
		p, err := s.resolvePrincipal(ctx, actor, in.Owner)
		if err != nil {
			return Task{}, nil, err
		}
		assignee = &p
	}
	now := s.Now()
	var row store.Task
	var unblocked []store.Task
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		t, err := tx.TaskForUpdate(ctx, found.ID)
		if err != nil {
			return err
		}
		var owner, creator *store.Principal
		if t.OwnerID != nil {
			p, err := c.principal(ctx, *t.OwnerID)
			if err != nil {
				return err
			}
			owner = &p
		}
		p, err := c.principal(ctx, t.CreatedBy)
		if err != nil {
			return err
		}
		creator = &p
		state, newOwner, err := taskTransition(actor, t, owner, creator, action, assignee, in.Force)
		if err != nil {
			return err
		}
		if state == t.State && sameID(newOwner, t.OwnerID) {
			row = t // nothing to change: taking your own task, or done twice
			return nil
		}
		if err := tx.SetTaskState(ctx, t.ID, state, newOwner, now); err != nil {
			return err
		}
		if row, err = tx.TaskByID(ctx, t.ID); err != nil {
			return err
		}
		if state == TaskDone {
			deps, err := tx.TaskDependents(ctx, t.ID)
			if err != nil {
				return err
			}
			for _, d := range deps {
				if d.State != TaskDone && !d.Blocked {
					unblocked = append(unblocked, d)
				}
			}
		}
		after := map[string]any{"effort": t.EffortSlug, "action": action, "state": state, "owner": refOf(c, newOwner), "force": in.Force}
		return tx.LogOp(ctx, &t.RepoID, &actor.ID, "task.update", t.ID.String(),
			map[string]any{"state": t.State, "owner": refOf(c, t.OwnerID)}, after)
	})
	if err != nil {
		return Task{}, nil, classify(err)
	}
	out, err := c.task(ctx, row)
	if err != nil {
		return Task{}, nil, err
	}
	ub, err := c.tasks(ctx, unblocked)
	return out, ub, err
}

// ListTasks lists an effort's tasks: not done first, then by creation.
func (s *Service) ListTasks(ctx context.Context, actor Actor, in ListTasksInput) ([]Task, error) {
	f := store.TaskFilter{}
	for _, st := range in.States {
		st = strings.ToLower(strings.TrimSpace(st))
		switch st {
		case TaskOpen, TaskTaken, TaskDone:
			f.States = append(f.States, st)
		default:
			return nil, errf(CodeInvalid, "task state %q is not one of open, taken, done", st)
		}
	}
	v, err := s.boardEffort(ctx, in.Repo, in.Effort)
	if err != nil {
		return nil, err
	}
	f.EffortID = v.Effort.ID
	if in.Mine {
		f.Owners = []uuid.UUID{actor.ID}
		if actor.Kind == store.KindHuman {
			agents, err := s.Store.ListAgents(ctx, actor.ID)
			if err != nil {
				return nil, err
			}
			for _, a := range agents {
				f.Owners = append(f.Owners, a.ID)
			}
		}
	}
	rows, err := s.Store.ListTasks(ctx, f)
	if err != nil {
		return nil, err
	}
	return s.newNoteCache().tasks(ctx, rows)
}

// taskTransition checks that actor may apply action to t and returns the
// task's new state and owner.
func taskTransition(actor Actor, t store.Task, owner, creator *store.Principal, action string, assignee *store.Principal, force bool) (string, *uuid.UUID, error) {
	id := shortID(t.ID)
	mayFinish := actsFor(actor, creator) || (owner != nil && actsFor(actor, owner))
	switch action {
	case actTake:
		switch {
		case t.State == TaskDone:
			return "", nil, errf(CodeFailedPrecondition, "task %s is done; reopen it first: vx task reopen %s", id, id)
		case owner != nil && owner.ID == actor.ID:
			return TaskTaken, &actor.ID, nil
		case owner != nil && !actsFor(actor, owner):
			return "", nil, errf(CodeFailedPrecondition, "task %s is taken by %s; pick another with `vx tasks --open`, or ask them to release it", id, owner.Ref())
		case t.Blocked && !force:
			return "", nil, errf(CodeFailedPrecondition, "task %s waits for tasks that are not done (`vx tasks` shows which); take another, or take it anyway with --force", id)
		}
		return TaskTaken, &actor.ID, nil
	case actRelease:
		switch {
		case t.State != TaskTaken:
			return "", nil, errf(CodeFailedPrecondition, "task %s is %s, not taken; there is nothing to release", id, t.State)
		case !mayFinish:
			return "", nil, errf(CodePermission, "task %s is taken by %s; only they, their person or the task's author %s can release it", id, owner.Ref(), creator.Ref())
		}
		return TaskOpen, nil, nil
	case actDone:
		switch {
		case t.State == TaskDone:
			return TaskDone, t.OwnerID, nil
		case !mayFinish:
			who := "nobody has taken it"
			if owner != nil {
				who = "it is taken by " + owner.Ref()
			}
			return "", nil, errf(CodePermission, "task %s is not yours to finish: %s. Take it first (vx task take %s), or ask its author %s", id, who, id, creator.Ref())
		case t.Blocked && !force:
			return "", nil, errf(CodeFailedPrecondition, "task %s waits for tasks that are not done (`vx tasks` shows which); finish those first, or mark it done anyway with --force", id)
		}
		return TaskDone, t.OwnerID, nil
	case actReopen:
		switch {
		case t.State != TaskDone:
			return "", nil, errf(CodeFailedPrecondition, "task %s is %s, not done; only done tasks reopen", id, t.State)
		case !mayFinish:
			return "", nil, errf(CodePermission, "only the task's author %s, its last owner or their person can reopen task %s", creator.Ref(), id)
		}
		return TaskOpen, nil, nil
	case actAssign:
		switch {
		case t.State == TaskDone:
			return "", nil, errf(CodeFailedPrecondition, "task %s is done; reopen it before assigning it", id)
		case owner != nil && owner.ID != assignee.ID && !mayFinish:
			return "", nil, errf(CodePermission, "task %s is taken by %s; only they, their person or the task's author %s can hand it to someone else", id, owner.Ref(), creator.Ref())
		}
		return TaskTaken, &assignee.ID, nil
	}
	return "", nil, errf(CodeInvalid, "task action %q is not one of take, release, done, reopen, assign", action)
}

// actsFor reports whether actor is p or p's person.
func actsFor(actor Actor, p *store.Principal) bool {
	if p == nil {
		return false
	}
	return p.ID == actor.ID || (actor.Kind == store.KindHuman && p.ParentID != nil && *p.ParentID == actor.ID)
}

func sameID(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func refOf(c *noteCache, id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	if p, ok := c.principals[*id]; ok {
		return p.Ref()
	}
	return id.String()
}

// cleanTaskTitle strips control characters and folds the title onto one
// line.
func cleanTaskTitle(s string) (string, error) {
	out := strings.Join(strings.Fields(stripControls(s)), " ")
	switch n := utf8.RuneCountInString(out); {
	case n == 0:
		return "", errf(CodeInvalid, "the task has no title; say in one line what is to be done")
	case n > MaxTaskTitle:
		return "", errf(CodeInvalid, "the task title is %d characters; keep it to %d and put details in the body", n, MaxTaskTitle)
	}
	return out, nil
}

func cleanTaskBody(s string) (string, error) {
	out := strings.TrimSpace(stripControls(s))
	if n := utf8.RuneCountInString(out); n > MaxTaskBody {
		return "", errf(CodeInvalid, "the task body is %d characters; keep it to %d and point to files or notes for details", n, MaxTaskBody)
	}
	return out, nil
}

// findTask resolves a full task id or a unique prefix of at least
// MinNoteIDPrefix characters, within one effort unless effort is uuid.Nil.
func (s *Service) findTask(ctx context.Context, effort uuid.UUID, id string) (store.Task, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if full, err := uuid.Parse(id); err == nil {
		t, err := s.Store.TaskByID(ctx, full)
		if err == nil && effort != uuid.Nil && t.EffortID != effort {
			return store.Task{}, errf(CodeInvalid, "task %s is in effort %s, not this one; tasks only wait for tasks of their own effort", shortID(full), t.EffortSlug)
		}
		if errors.Is(err, store.ErrNotFound) {
			return store.Task{}, errf(CodeNotFound, "no task %s; `vx tasks` lists the effort's tasks", id)
		}
		return t, err
	}
	if len(id) < MinNoteIDPrefix {
		return store.Task{}, errf(CodeInvalid, "task id %q is too short: give at least %d characters", id, MinNoteIDPrefix)
	}
	t, err := s.Store.TaskByPrefix(ctx, effort, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Task{}, errf(CodeNotFound, "no task in this effort has an id starting with %s; `vx tasks --all` lists them", id)
	case errors.Is(err, store.ErrAmbiguous):
		return store.Task{}, errf(CodeInvalid, "several tasks have ids starting with %s; give more characters", id)
	}
	return t, err
}

func (c *noteCache) task(ctx context.Context, r store.Task) (Task, error) {
	t := Task{
		ID: r.ID, RepoName: r.RepoName, EffortSlug: r.EffortSlug, EffortID: r.EffortID, Title: r.Title, Body: r.Body,
		State: r.State, DependsOn: r.DependsOn, Blocked: r.Blocked, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if t.DependsOn == nil {
		t.DependsOn = []uuid.UUID{}
	}
	var err error
	if t.CreatedBy, err = c.principal(ctx, r.CreatedBy); err != nil {
		return Task{}, err
	}
	if t.CreatedBy.ParentID != nil {
		p, err := c.principal(ctx, *t.CreatedBy.ParentID)
		if err != nil {
			return Task{}, err
		}
		t.CreatedParent = &p
	}
	if r.OwnerID != nil {
		p, err := c.principal(ctx, *r.OwnerID)
		if err != nil {
			return Task{}, err
		}
		t.Owner = &p
	}
	return t, nil
}

func (c *noteCache) tasks(ctx context.Context, rows []store.Task) ([]Task, error) {
	out := make([]Task, 0, len(rows))
	for _, r := range rows {
		t, err := c.task(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}
