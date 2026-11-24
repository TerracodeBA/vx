package service_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func (b *boardEnv) addTask(actor service.Actor, title string, after ...string) service.Task {
	b.t.Helper()
	t, err := b.svc.AddTask(b.ctx, actor, service.AddTaskInput{Repo: "shop-api", Effort: b.effort.Slug, Title: title, DependsOn: after})
	if err != nil {
		b.t.Fatal(err)
	}
	return t
}

func (b *boardEnv) update(actor service.Actor, id uuid.UUID, action string, force bool) (service.Task, []service.Task, error) {
	return b.svc.UpdateTask(b.ctx, actor, service.UpdateTaskInput{ID: id.String(), Action: action, Force: force})
}

func (b *boardEnv) mustUpdate(actor service.Actor, id uuid.UUID, action string) (service.Task, []service.Task) {
	b.t.Helper()
	t, ub, err := b.update(actor, id, action, false)
	if err != nil {
		b.t.Fatalf("%s %s: %v", action, id, err)
	}
	return t, ub
}

func titles(ts []service.Task) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return strings.Join(out, ",")
}

func TestTaskAddValidates(t *testing.T) {
	b := newBoardEnv(t)
	task, err := b.svc.AddTask(b.ctx, b.agent, service.AddTaskInput{
		Repo: "shop-api", Effort: b.effort.Slug, Title: "  Add the\x00 limiter\r\n middleware‮ ", Body: "  Use a token\x1b[31m bucket.\n\tPer IP.⁦  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "Add the limiter middleware" || task.Body != "Use a token[31m bucket.\n\tPer IP." {
		t.Fatalf("title %q body %q", task.Title, task.Body)
	}
	if task.State != service.TaskOpen || task.Owner != nil || task.Blocked || len(task.DependsOn) != 0 ||
		task.CreatedBy.ID != b.agent.ID || task.CreatedParent == nil || task.CreatedParent.ID != b.alice.ID ||
		task.RepoName != "shop-api" || task.EffortSlug != b.effort.Slug || !task.CreatedAt.Equal(b.now) {
		t.Fatalf("task = %+v", task)
	}
	if service.TaskTrust(b.alice, task) != service.TrustSamePerson || service.TaskTrust(b.bob, task) != service.TrustTeammate {
		t.Fatal("trust labels")
	}
	b.addTask(b.alice, strings.Repeat("é", service.MaxTaskTitle))

	add := func(in service.AddTaskInput) error {
		in.Repo, in.Effort = "shop-api", b.effort.Slug
		_, err := b.svc.AddTask(b.ctx, b.agent, in)
		return err
	}
	wantCode(t, add(service.AddTaskInput{Title: " \n\x00 "}), service.CodeInvalid, "no title")
	wantCode(t, add(service.AddTaskInput{Title: strings.Repeat("x", service.MaxTaskTitle+1)}), service.CodeInvalid, "201 characters")
	wantCode(t, add(service.AddTaskInput{Title: "x", Body: strings.Repeat("x", service.MaxTaskBody+1)}), service.CodeInvalid, "4001 characters")
	wantCode(t, add(service.AddTaskInput{Title: "x", DependsOn: []string{"abc"}}), service.CodeInvalid, "too short")
	wantCode(t, add(service.AddTaskInput{Title: "x", DependsOn: []string{"00000000"}}), service.CodeNotFound, "no task")
	wantCode(t, add(service.AddTaskInput{Title: "x", Owner: "nobody"}), service.CodeNotFound, "no agent")
	_, err = b.svc.AddTask(b.ctx, b.agent, service.AddTaskInput{Repo: "shop-api", Effort: "nope", Title: "x"})
	wantCode(t, err, service.CodeNotFound, "no effort nope")

	// Assigned at once.
	as, err := b.svc.AddTask(b.ctx, b.alice, service.AddTaskInput{Repo: "shop-api", Effort: b.effort.Slug, Title: "Review", Owner: "tests"})
	if err != nil || as.State != service.TaskTaken || as.Owner == nil || as.Owner.ID != b.tests.ID {
		t.Fatalf("assigned = %+v, %v", as, err)
	}
	ops, err := b.svc.Store.ListOps(b.ctx, b.repo.ID, 10)
	if err != nil || ops[0].Op != "task.add" {
		t.Fatalf("ops = %+v, %v", ops, err)
	}
}

func TestTaskDependencies(t *testing.T) {
	b := newBoardEnv(t)
	contract := b.addTask(b.alice, "Contract")
	other := b.addTask(b.alice, "Other")
	impl := b.addTask(b.alice, "Implementation", contract.ID.String()[:8], other.ID.String(), contract.ID.String())
	if !impl.Blocked || len(impl.DependsOn) != 2 || impl.DependsOn[0] != contract.ID || impl.DependsOn[1] != other.ID {
		t.Fatalf("impl = %+v", impl)
	}

	// Dependencies stay within the effort.
	eff2, err := b.svc.CreateEffort(b.ctx, b.alice, service.CreateEffortInput{Repo: "shop-api", Name: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.svc.AddTask(b.ctx, b.alice, service.AddTaskInput{Repo: "shop-api", Effort: eff2.Slug, Title: "x", DependsOn: []string{contract.ID.String()}})
	wantCode(t, err, service.CodeInvalid, "tasks only wait for tasks of their own effort")
	_, err = b.svc.AddTask(b.ctx, b.alice, service.AddTaskInput{Repo: "shop-api", Effort: eff2.Slug, Title: "x", DependsOn: []string{contract.ID.String()[:8]}})
	wantCode(t, err, service.CodeNotFound, "no task in this effort")

	// The store refuses cycles and self-dependencies.
	err = b.svc.Store.InTx(b.ctx, func(tx *store.Tx) error { return tx.AddTaskDeps(b.ctx, contract.ID, []uuid.UUID{impl.ID}) })
	if !strings.Contains(err.Error(), store.ErrCycle.Error()) {
		t.Fatalf("cycle err = %v", err)
	}
	err = b.svc.Store.InTx(b.ctx, func(tx *store.Tx) error { return tx.AddTaskDeps(b.ctx, contract.ID, []uuid.UUID{contract.ID}) })
	if err == nil || !strings.Contains(err.Error(), store.ErrCycle.Error()) {
		t.Fatalf("self err = %v", err)
	}

	// Blocked until every dependency is done; done reports what it unblocked.
	if _, _, err := b.update(b.agent, impl.ID, "take", false); err == nil {
		t.Fatal("took a blocked task")
	} else {
		wantCode(t, err, service.CodeFailedPrecondition, "--force")
	}
	b.mustUpdate(b.agent, contract.ID, "take")
	if _, ub := b.mustUpdate(b.agent, contract.ID, "done"); len(ub) != 0 {
		t.Fatalf("unblocked with other open = %v", titles(ub))
	}
	_, ub := b.mustUpdate(b.alice, other.ID, "done")
	if titles(ub) != "Implementation" || ub[0].Blocked {
		t.Fatalf("unblocked = %+v", ub)
	}
	// Done twice changes nothing and unblocks nothing again.
	if again, ub := b.mustUpdate(b.alice, other.ID, "done"); again.State != service.TaskDone || len(ub) != 0 {
		t.Fatalf("done again = %+v %v", again, ub)
	}
	list, err := b.svc.ListTasks(b.ctx, b.bob, service.ListTasksInput{Repo: "shop-api", Effort: b.effort.Slug})
	if err != nil || titles(list) != "Implementation,Contract,Other" || list[0].Blocked {
		t.Fatalf("list = %v, %v", titles(list), err)
	}
	// Reopening a dependency blocks again.
	b.mustUpdate(b.alice, other.ID, "reopen")
	list, _ = b.svc.ListTasks(b.ctx, b.bob, service.ListTasksInput{Repo: "shop-api", Effort: b.effort.Slug, States: []string{"open"}})
	if titles(list) != "Other,Implementation" || !list[1].Blocked || list[0].Owner != nil {
		t.Fatalf("open after reopen = %v", titles(list))
	}
	// Force takes and finishes a blocked task.
	if tk, _, err := b.update(b.bobBE, impl.ID, "take", true); err != nil || tk.Owner.ID != b.bobBE.ID {
		t.Fatalf("force take = %+v, %v", tk, err)
	}
	_, _, err = b.update(b.bobBE, impl.ID, "done", false)
	wantCode(t, err, service.CodeFailedPrecondition, "--force")
	if d, _, err := b.update(b.bobBE, impl.ID, "done", true); err != nil || d.State != service.TaskDone || d.Owner.ID != b.bobBE.ID {
		t.Fatalf("force done = %+v, %v", d, err)
	}
}

func TestTaskActionsAndPermissions(t *testing.T) {
	b := newBoardEnv(t)
	task := b.addTask(b.bob, "Write the limiter") // by bob, a teammate of alice's agents

	tk, _ := b.mustUpdate(b.agent, task.ID, "take")
	if tk.State != service.TaskTaken || tk.Owner.ID != b.agent.ID {
		t.Fatalf("taken = %+v", tk)
	}
	// Taking your own task again is fine; others cannot take it.
	b.mustUpdate(b.agent, task.ID, "take")
	_, _, err := b.update(b.bobBE, task.ID, "take", true)
	wantCode(t, err, service.CodeFailedPrecondition, "taken by agent:alice/claude")
	_, _, err = b.update(b.tests, task.ID, "take", false)
	wantCode(t, err, service.CodeFailedPrecondition, "taken by agent:alice/claude")
	// A teammate who neither owns nor wrote it cannot release or finish it.
	_, _, err = b.update(b.bobBE, task.ID, "release", false)
	wantCode(t, err, service.CodePermission, "only they")
	_, _, err = b.update(b.bobBE, task.ID, "done", false)
	wantCode(t, err, service.CodePermission, "not yours to finish")
	// The owner's person takes over from their agent.
	tk, _ = b.mustUpdate(b.alice, task.ID, "take")
	if tk.Owner.ID != b.alice.ID {
		t.Fatalf("take over = %+v", tk.Owner)
	}
	// The creator releases it.
	if r, _ := b.mustUpdate(b.bob, task.ID, "release"); r.State != service.TaskOpen || r.Owner != nil {
		t.Fatalf("released = %+v", r)
	}
	_, _, err = b.update(b.bob, task.ID, "release", false)
	wantCode(t, err, service.CodeFailedPrecondition, "not taken")
	_, _, err = b.update(b.agent, task.ID, "reopen", false)
	wantCode(t, err, service.CodeFailedPrecondition, "not done")
	// Nobody owns it: only the creator (or their person) may mark it done.
	_, _, err = b.update(b.agent, task.ID, "done", false)
	wantCode(t, err, service.CodePermission, "nobody has taken it")

	// Assign sets the owner and the state.
	as, _, err := b.svc.UpdateTask(b.ctx, b.bob, service.UpdateTaskInput{ID: task.ID.String()[:8], Action: "assign", Owner: "agent:alice/tests"})
	if err != nil || as.State != service.TaskTaken || as.Owner.ID != b.tests.ID {
		t.Fatalf("assign = %+v, %v", as, err)
	}
	_, _, err = b.svc.UpdateTask(b.ctx, b.bobBE, service.UpdateTaskInput{ID: task.ID.String(), Action: "assign", Owner: "backend"})
	wantCode(t, err, service.CodePermission, "hand it to someone else")
	_, _, err = b.svc.UpdateTask(b.ctx, b.bob, service.UpdateTaskInput{ID: task.ID.String(), Action: "assign"})
	wantCode(t, err, service.CodeInvalid, "say who")
	// A person whose agent owns it marks it done.
	if d, _ := b.mustUpdate(b.alice, task.ID, "done"); d.State != service.TaskDone || d.Owner.ID != b.tests.ID {
		t.Fatalf("done = %+v", d)
	}
	_, _, err = b.update(b.agent, task.ID, "take", false)
	wantCode(t, err, service.CodeFailedPrecondition, "reopen it first")
	_, _, err = b.update(b.bobBE, task.ID, "reopen", false)
	wantCode(t, err, service.CodePermission, "reopen")
	if r, _ := b.mustUpdate(b.tests, task.ID, "reopen"); r.State != service.TaskOpen || r.Owner != nil {
		t.Fatalf("reopened = %+v", r)
	}
	// The creator finishes an open task.
	b.mustUpdate(b.bob, task.ID, "done")

	_, _, err = b.update(b.bob, task.ID, "start", false)
	wantCode(t, err, service.CodeInvalid, "not one of take")
	_, _, err = b.svc.UpdateTask(b.ctx, b.bob, service.UpdateTaskInput{ID: "1234", Action: "take"})
	wantCode(t, err, service.CodeInvalid, "too short")

	ops, err := b.svc.Store.ListOps(b.ctx, b.repo.ID, 1)
	if err != nil || ops[0].Op != "task.update" {
		t.Fatalf("ops = %+v, %v", ops, err)
	}
}

func TestTaskListMine(t *testing.T) {
	b := newBoardEnv(t)
	t1 := b.addTask(b.alice, "One")
	t2 := b.addTask(b.alice, "Two")
	t3 := b.addTask(b.alice, "Three")
	b.addTask(b.alice, "Four")
	b.mustUpdate(b.agent, t1.ID, "take")
	b.mustUpdate(b.alice, t2.ID, "take")
	b.mustUpdate(b.bobBE, t3.ID, "take")
	b.mustUpdate(b.agent, t1.ID, "done")

	list := func(actor service.Actor, in service.ListTasksInput) string {
		t.Helper()
		in.Repo, in.Effort = "shop-api", b.effort.Slug
		ts, err := b.svc.ListTasks(b.ctx, actor, in)
		if err != nil {
			t.Fatal(err)
		}
		return titles(ts)
	}
	if got := list(b.agent, service.ListTasksInput{Mine: true}); got != "One" {
		t.Errorf("agent mine = %s", got)
	}
	if got := list(b.alice, service.ListTasksInput{Mine: true}); got != "Two,One" {
		t.Errorf("alice mine = %s", got)
	}
	if got := list(b.alice, service.ListTasksInput{Mine: true, States: []string{"taken"}}); got != "Two" {
		t.Errorf("alice mine taken = %s", got)
	}
	if got := list(b.bob, service.ListTasksInput{Mine: true}); got != "Three" {
		t.Errorf("bob mine = %s", got)
	}
	if got := list(b.tests, service.ListTasksInput{}); got != "Two,Three,Four,One" {
		t.Errorf("all = %s", got)
	}
	_, err := b.svc.ListTasks(b.ctx, b.alice, service.ListTasksInput{Repo: "shop-api", Effort: b.effort.Slug, States: []string{"blocked"}})
	wantCode(t, err, service.CodeInvalid, "not one of open, taken, done")
}
