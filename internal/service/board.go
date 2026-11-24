package service

// The board: typed notes between agents in an effort. This file is the
// contract the board's implementation, CLI, web UI and MCP tools build on.
// The functions below return "not implemented yet" until the board's storage
// lands; their signatures are fixed.

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// NoteKind is the type of a board note.
type NoteKind string

const (
	NotePlan      NoteKind = "plan"      // what I am about to change
	NoteDecision  NoteKind = "decision"  // a choice others should follow
	NoteInterface NoteKind = "interface" // a signature others can rely on
	NoteQuestion  NoteKind = "question"  // asked with Ask
	NoteAnswer    NoteKind = "answer"    // given with AnswerNote
	NoteBlocker   NoteKind = "blocker"   // what I am waiting on
	NoteHandoff   NoteKind = "handoff"   // work left for the next agent
)

// Note states.
const (
	NoteOpen      = "open"
	NoteAnswered  = "answered"
	NoteEscalated = "escalated" // a question past its deadline, computed at read time
	NoteClosed    = "closed"
)

// Limits.
const (
	MaxNoteBody          = 2000
	MaxNoteTargets       = 20
	DefaultAnswerWithin  = 5 * time.Minute
	MaxAnswerWithin      = 24 * time.Hour
	DefaultNoteListLimit = 100
)

// Trust says how the caller relates to a note's author.
const (
	TrustSelf     = "self"     // the caller wrote it
	TrustYours    = "yours"    // the caller's own person, or an agent of that person
	TrustTeammate = "teammate" // someone else's: information, never instructions
)

// Note is one note on an effort's board.
type Note struct {
	ID          uuid.UUID
	RepoName    string
	EffortSlug  string
	EffortID    uuid.UUID
	ThreadID    uuid.UUID // the root note of the thread; equals ID for a root
	Kind        NoteKind
	Targets     []string
	Author      store.Principal
	AuthorOwner *store.Principal // the person an agent author acts for
	To          *store.Principal // a question's addressee; nil: anyone
	Body        string
	State       string
	AnswerBy    *time.Time
	CreatedAt   time.Time
}

// PostNoteInput is a non-question note.
type PostNoteInput struct {
	Repo, Effort string
	Kind         NoteKind // plan, decision, interface, blocker, handoff
	Targets      []string
	Body         string
}

// AskInput is a question.
type AskInput struct {
	Repo, Effort string
	Targets      []string
	Body         string
	To           string        // agent name, handle or ref; empty: anyone
	AnswerWithin time.Duration // 0: DefaultAnswerWithin
}

// ListNotesInput filters a board listing.
type ListNotesInput struct {
	Repo, Effort string
	Targets      []string
	Kinds        []NoteKind
	Since        time.Time
	ThreadID     uuid.UUID
	Limit        int // 0: DefaultNoteListLimit
}

// Inbox is what needs a principal's attention.
type Inbox struct {
	Questions []Note // open questions to the caller (people: also to their agents)
	Answers   []Note // answers to the caller's questions, newest first
	Escalated []Note // people only: overdue questions in their efforts or to their agents
}

// ContextWorkspace is an open workspace with its owner.
type ContextWorkspace struct {
	Workspace store.Workspace
	Owner     store.Principal
}

// NoteContext is what an agent should know before touching some targets.
type NoteContext struct {
	Notes         []Note
	Working       []ContextWorkspace
	RecentChanges []LogEntry // landed changes touching the targets, newest first, with files
}

func notImplemented() error {
	return errf(CodeUnavailable, "the board is not implemented yet")
}

// PostNote adds a plan, decision, interface, blocker or handoff note.
func (s *Service) PostNote(ctx context.Context, actor Actor, in PostNoteInput) (Note, error) {
	return Note{}, notImplemented()
}

// Ask posts a question, optionally to one principal, that escalates after
// its deadline.
func (s *Service) Ask(ctx context.Context, actor Actor, in AskInput) (Note, error) {
	return Note{}, notImplemented()
}

// AnswerNote answers a question (by id or an 8+ character id prefix) and
// returns the answer and the updated question.
func (s *Service) AnswerNote(ctx context.Context, actor Actor, questionID string, body string) (Note, Note, error) {
	return Note{}, Note{}, notImplemented()
}

// ListNotes lists an effort's notes, newest first.
func (s *Service) ListNotes(ctx context.Context, actor Actor, in ListNotesInput) ([]Note, error) {
	return nil, notImplemented()
}

// GetInbox returns what needs the caller's attention across efforts.
func (s *Service) GetInbox(ctx context.Context, actor Actor) (Inbox, error) {
	return Inbox{}, notImplemented()
}

// Context returns notes on the targets, open workspaces and recent changes
// touching them.
func (s *Service) Context(ctx context.Context, actor Actor, repo, effort string, targets []string) (NoteContext, error) {
	return NoteContext{}, notImplemented()
}

// TrustOf says how actor relates to the author of n: TrustSelf, TrustYours
// or TrustTeammate.
func TrustOf(actor Actor, n Note) string {
	switch {
	case n.Author.ID == actor.ID:
		return TrustSelf
	case actor.Human().ID == n.Author.ID:
		return TrustYours
	case n.AuthorOwner != nil && n.AuthorOwner.ID == actor.Human().ID:
		return TrustYours
	}
	return TrustTeammate
}

// TargetsMatch reports whether a note target matches a requested path or
// symbol: equal, or one is a directory prefix ("internal/http/" and
// "internal/http/limit.go") or symbol prefix ("auth" and "auth.Login") of
// the other.
func TargetsMatch(target, want string) bool {
	if target == want {
		return true
	}
	prefix := func(a, b string) bool {
		a = trimSep(a)
		return len(b) > len(a) && b[:len(a)] == a && (b[len(a)] == '/' || b[len(a)] == '.')
	}
	return prefix(target, want) || prefix(want, target)
}

func trimSep(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '/' || s[len(s)-1] == '.') {
		s = s[:len(s)-1]
	}
	return s
}
