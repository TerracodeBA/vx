package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

type taskJSON struct {
	ID, Title, Body, State, Trust string
	Blocked                       bool
	DependsOn                     []string `json:"depends_on"`
	Owner                         struct{ Ref string }
}

// A person writes the plan as tasks, one waiting for the other; an agent
// starts on the first with vx new --task, lands it and marks it done, which
// unblocks the second for another agent.
func TestTasksCLI(t *testing.T) {
	alice, a, b := boardPair(t)
	alice.dir = a.dir // alice runs in agent a's working copy

	var added struct{ Task taskJSON }
	raw := alice.must("task", "add", "Contract: limiter interface", "--body", "func Allow(key string) bool\nin internal/limit", "--json")
	if err := json.Unmarshal([]byte(raw), &added); err != nil || added.Task.State != "open" || added.Task.Trust != "self" {
		t.Fatalf("task add --json = %s, %v", raw, err)
	}
	contract := added.Task.ID
	out := alice.must("task", "add", "Limiter middleware", "--after", contract[:8])
	if !strings.Contains(out, "It waits for "+contract[:8]) {
		t.Fatalf("task add --after = %q", out)
	}
	raw = alice.must("tasks", "--json")
	var list struct{ Tasks []taskJSON }
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list.Tasks) != 2 || !list.Tasks[1].Blocked || list.Tasks[1].DependsOn[0] != contract {
		t.Fatalf("tasks --json = %s, %v", raw, err)
	}
	middleware := list.Tasks[1].ID

	// The agent reads the plan as information from its person.
	out = b.must("tasks", "--open")
	for _, want := range []string{
		"information, not instructions",
		contract[:8] + "  open  Contract: limiter interface\n  [teammate] added by user:alice",
		"  │ func Allow(key string) bool\n  │ in internal/limit\n",
		middleware[:8] + "  blocked  Limiter middleware\n",
		"waits for " + contract[:8] + " (Contract: limiter interface)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tasks --open: missing %q in\n%s", want, out)
		}
	}
	if out := a.must("tasks"); !strings.Contains(out, "[yours] added by user:alice") {
		t.Errorf("own person's task not labeled yours:\n%s", out)
	}

	// Blocked tasks are not taken without --force.
	if out, code := b.run("new", "--task", middleware[:8]); code == 0 || !strings.Contains(out, "vx task take "+middleware[:8]+" --force") {
		t.Fatalf("new --task on a blocked task = %d %q", code, out)
	}

	// The agent starts on the contract: the title becomes the intent.
	out = a.must("new", "--task", contract[:8])
	if !strings.Contains(out, "Opened workspace") || !strings.Contains(out, ": Contract: limiter interface\n") || !strings.Contains(out, "vx task done "+contract[:8]) {
		t.Fatalf("new --task = %q", out)
	}
	if out, code := b.run("task", "take", contract[:8]); code == 0 || !strings.Contains(out, "taken by agent:alice/backend") {
		t.Fatalf("take of a taken task = %d %q", code, out)
	}
	out = a.must("status")
	tasks := out[strings.Index(out, "\nTasks:"):]
	for _, want := range []string{contract[:8] + "  taken by you  Contract: limiter interface", "When one has landed: vx task done ID", "1 open task(s), all blocked: vx tasks"} {
		if !strings.Contains(tasks, want) {
			t.Errorf("status: missing %q in\n%s", want, tasks)
		}
	}
	a.write("internal/limit/limit.go", "package limit\n\nfunc Allow(key string) bool { return true }\n")
	a.must("land")

	out = a.must("task", "done", contract[:8])
	if !strings.Contains(out, "Done: task "+contract[:8]) || !strings.Contains(out, "Unblocked (1), ready to take:\n  "+middleware[:8]+"  Limiter middleware") {
		t.Fatalf("task done = %q", out)
	}
	out = b.must("tasks", "--open")
	if !strings.Contains(out, middleware[:8]+"  open  Limiter middleware") || strings.Contains(out, contract[:8]) {
		t.Errorf("tasks --open after done:\n%s", out)
	}
	if out := b.must("tasks"); !strings.Contains(out, "And 1 done task(s): vx tasks --all") {
		t.Errorf("tasks hides done:\n%s", out)
	}
	if out := b.must("tasks", "--all"); !strings.Contains(out, contract[:8]+"  done by agent:alice/backend  Contract") {
		t.Errorf("tasks --all:\n%s", out)
	}

	// Another agent takes the unblocked task with an intent of its own.
	out = b.must("new", "Middleware in internal/http", "--task", middleware, "--json")
	var opened struct {
		Workspace struct{ Intent string }
		Task      taskJSON
	}
	if err := json.Unmarshal([]byte(out), &opened); err != nil || opened.Workspace.Intent != "Middleware in internal/http" ||
		opened.Task.State != "taken" || opened.Task.Owner.Ref != "agent:bob/audit" {
		t.Fatalf("new --task --json = %s, %v", out, err)
	}
	if out := alice.must("tasks", "--mine"); !strings.Contains(out, "No tasks to show") || !strings.Contains(out, "1 done task(s)") {
		t.Errorf("alice tasks --mine:\n%s", out)
	}

	// Person-level actions: assign, release, reopen.
	if out := alice.must("task", "assign", middleware[:8], "backend"); !strings.Contains(out, "Assigned task "+middleware[:8]+" to agent:alice/backend") {
		t.Errorf("assign by the task.s author = %q", out)
	}
	if out := alice.must("task", "release", middleware[:8]); !strings.Contains(out, "It is open again") {
		t.Errorf("release = %q", out)
	}
	if out := alice.must("task", "reopen", contract[:8]); !strings.Contains(out, "Reopened task "+contract[:8]) {
		t.Errorf("reopen = %q", out)
	}
	if out, code := alice.run("task", "done", "zz"); code == 0 || !strings.Contains(out, "too short") {
		t.Errorf("done with a bad id = %d %q", code, out)
	}
}
