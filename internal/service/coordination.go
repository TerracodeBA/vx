package service

// Coordination: claims and tasks. This file is the contract the claims and
// tasks implementations, CLI, web UI and MCP tools build on; the functions
// return "not implemented yet" until their storage lands. Signatures are
// fixed. Everyone signed in may read and write every effort's claims and
// tasks, as for the board.

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// Claim limits.
const (
	DefaultClaimTTL = 30 * time.Minute
	MaxClaimTTL     = 4 * time.Hour
	MaxClaimReason  = 300
)

// Claim says a principal is changing some paths or symbols now.
type Claim struct {
	ID          uuid.UUID
	RepoName    string
	EffortSlug  string
	EffortID    uuid.UUID
	Targets     []string
	Owner       store.Principal
	OwnerParent *store.Principal // the person an agent owner acts for
	WorkspaceID *uuid.UUID
	Reason      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ReleasedAt  *time.Time
}

// ClaimInput is a new claim.
type ClaimInput struct {
	Repo, Effort string
	Targets      []string
	Reason       string
	TTL          time.Duration // 0: DefaultClaimTTL
	WorkspaceID  string        // optional
}

// Task states.
const (
	TaskOpen  = "open"
	TaskTaken = "taken"
	TaskDone  = "done"
)

// Task limits.
const (
	MaxTaskTitle = 200
	MaxTaskBody  = 4000
)

// Task is one item of an effort's shared plan.
type Task struct {
	ID            uuid.UUID
	RepoName      string
	EffortSlug    string
	EffortID      uuid.UUID
	Title, Body   string
	State         string
	Owner         *store.Principal
	DependsOn     []uuid.UUID
	Blocked       bool // computed: some DependsOn task is not done
	CreatedBy     store.Principal
	CreatedParent *store.Principal // the person an agent creator acts for
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AddTaskInput is a new task.
type AddTaskInput struct {
	Repo, Effort string
	Title, Body  string
	DependsOn    []string // ids or 8+ character prefixes
	Owner        string   // optional: agent name, handle or ref
}

// UpdateTaskInput changes a task: Action is take, release, done, reopen or
// assign (with Owner).
type UpdateTaskInput struct {
	ID     string
	Action string
	Owner  string
	Force  bool
}

// ListTasksInput filters an effort's tasks.
type ListTasksInput struct {
	Repo, Effort string
	States       []string
	Mine         bool
}

func coordinationNotImplemented() error {
	return errf(CodeUnavailable, "claims and tasks are not implemented yet")
}

// AddTask adds a task to an effort's plan.
func (s *Service) AddTask(ctx context.Context, actor Actor, in AddTaskInput) (Task, error) {
	return Task{}, coordinationNotImplemented()
}

// UpdateTask applies an action to a task and returns it, plus the tasks a
// done unblocked.
func (s *Service) UpdateTask(ctx context.Context, actor Actor, in UpdateTaskInput) (Task, []Task, error) {
	return Task{}, nil, coordinationNotImplemented()
}

// ListTasks lists an effort's tasks: not done first, then by creation.
func (s *Service) ListTasks(ctx context.Context, actor Actor, in ListTasksInput) ([]Task, error) {
	return nil, coordinationNotImplemented()
}

// ClaimTrust says how actor relates to a claim's owner, like TrustOf.
func ClaimTrust(actor Actor, c Claim) string {
	return TrustOf(actor, Note{Author: c.Owner, AuthorOwner: c.OwnerParent})
}

// TaskTrust says how actor relates to a task's creator, like TrustOf.
func TaskTrust(actor Actor, t Task) string {
	return TrustOf(actor, Note{Author: t.CreatedBy, AuthorOwner: t.CreatedParent})
}
