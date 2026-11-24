package service

// The board: typed notes between agents in an effort. This file is the
// contract the CLI, web UI and MCP tools build on; the signatures of the
// functions below are fixed. Helpers live in notes.go.

import (
	"context"
	"strings"
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

// Everyone signed in may read and write every effort's board for now, the
// same as the rest of vx, where any principal may open workspaces and land
// in any effort. Readers learn whose note it is from TrustOf, not from
// permissions.

// PostNote adds a plan, decision, interface, blocker or handoff note.
func (s *Service) PostNote(ctx context.Context, actor Actor, in PostNoteInput) (Note, error) {
	if !postKinds[in.Kind] {
		switch in.Kind {
		case NoteQuestion:
			return Note{}, errf(CodeInvalid, "questions go through Ask (vx ask), which gives them a deadline")
		case NoteAnswer:
			return Note{}, errf(CodeInvalid, "answers go through AnswerNote (vx answer), which needs the question's id")
		}
		return Note{}, errf(CodeInvalid, "note kind %q is not one of plan, decision, interface, blocker, handoff", in.Kind)
	}
	body, err := cleanBody(in.Body)
	if err != nil {
		return Note{}, err
	}
	targets, err := cleanTargets(in.Targets)
	if err != nil {
		return Note{}, err
	}
	v, err := s.boardEffort(ctx, in.Repo, in.Effort)
	if err != nil {
		return Note{}, err
	}
	row, err := s.insertNote(ctx, actor, "note.post", store.Note{
		EffortID: v.Effort.ID, Kind: string(in.Kind), Targets: targets, AuthorID: actor.ID, Body: body,
		State: NoteOpen, CreatedAt: s.Now(),
	}, nil)
	if err != nil {
		return Note{}, err
	}
	return s.newNoteCache().note(ctx, row, s.Now())
}

// Ask posts a question, optionally to one principal, that escalates after
// its deadline.
func (s *Service) Ask(ctx context.Context, actor Actor, in AskInput) (Note, error) {
	body, err := cleanBody(in.Body)
	if err != nil {
		return Note{}, err
	}
	targets, err := cleanTargets(in.Targets)
	if err != nil {
		return Note{}, err
	}
	within := in.AnswerWithin
	switch {
	case within == 0:
		within = DefaultAnswerWithin
	case within < 0:
		return Note{}, errf(CodeInvalid, "answer within %s is negative; give a positive duration such as 10m", within)
	case within > MaxAnswerWithin:
		return Note{}, errf(CodeInvalid, "answer within %s is too long; questions escalate after at most %s", within, MaxAnswerWithin)
	}
	var to *store.Principal
	if strings.TrimSpace(in.To) != "" {
		p, err := s.resolvePrincipal(ctx, actor, in.To)
		if err != nil {
			return Note{}, err
		}
		to = &p
	}
	v, err := s.boardEffort(ctx, in.Repo, in.Effort)
	if err != nil {
		return Note{}, err
	}
	now := s.Now()
	answerBy := now.Add(within)
	row := store.Note{
		EffortID: v.Effort.ID, Kind: string(NoteQuestion), Targets: targets, AuthorID: actor.ID, Body: body,
		State: NoteOpen, AnswerBy: &answerBy, CreatedAt: now,
	}
	extra := map[string]string{}
	if to != nil {
		row.ToID = &to.ID
		extra["to"] = to.Ref()
	}
	if row, err = s.insertNote(ctx, actor, "note.ask", row, extra); err != nil {
		return Note{}, err
	}
	return s.newNoteCache().note(ctx, row, now)
}

// AnswerNote answers a question (by id or an 8+ character id prefix) and
// returns the answer and the updated question.
func (s *Service) AnswerNote(ctx context.Context, actor Actor, questionID string, body string) (Note, Note, error) {
	body, err := cleanBody(body)
	if err != nil {
		return Note{}, Note{}, err
	}
	found, err := s.findNote(ctx, questionID)
	if err != nil {
		return Note{}, Note{}, err
	}
	now := s.Now()
	var answer, question store.Note
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		q, err := tx.NoteForUpdate(ctx, found.ID)
		if err != nil {
			return err
		}
		if q.Kind != string(NoteQuestion) {
			return errf(CodeInvalid, "note %s is a %s, not a question; only questions take answers", shortID(q.ID), q.Kind)
		}
		answer, err = tx.InsertNote(ctx, store.Note{
			EffortID: q.EffortID, ThreadID: q.ID, Kind: string(NoteAnswer), Targets: q.Targets,
			AuthorID: actor.ID, Body: body, State: NoteClosed, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		if err := tx.MarkNoteAnswered(ctx, q.ID); err != nil {
			return err
		}
		if question, err = tx.NoteByID(ctx, q.ID); err != nil {
			return err
		}
		return tx.LogOp(ctx, &q.RepoID, &actor.ID, "note.answer", answer.ID.String(), nil,
			map[string]string{"effort": q.EffortSlug, "question": q.ID.String()})
	})
	if err != nil {
		return Note{}, Note{}, classify(err)
	}
	c := s.newNoteCache()
	a, err := c.note(ctx, answer, now)
	if err != nil {
		return Note{}, Note{}, err
	}
	qn, err := c.note(ctx, question, now)
	return a, qn, err
}

// ListNotes lists an effort's notes, newest first.
func (s *Service) ListNotes(ctx context.Context, actor Actor, in ListNotesInput) ([]Note, error) {
	targets, err := cleanTargets(in.Targets)
	if err != nil {
		return nil, err
	}
	var kinds []string
	for _, k := range in.Kinds {
		if !allKinds[k] {
			return nil, errf(CodeInvalid, "note kind %q is not one of plan, decision, interface, question, answer, blocker, handoff", k)
		}
		kinds = append(kinds, string(k))
	}
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultNoteListLimit
	}
	limit = min(limit, maxNoteListLimit)
	v, err := s.boardEffort(ctx, in.Repo, in.Effort)
	if err != nil {
		return nil, err
	}
	rows, err := s.listNotes(ctx, store.NoteFilter{EffortID: v.Effort.ID, Kinds: kinds, Since: in.Since, ThreadID: in.ThreadID}, targets, limit)
	if err != nil {
		return nil, err
	}
	return s.newNoteCache().notes(ctx, rows, s.Now())
}

// GetInbox returns what needs the caller's attention across efforts.
func (s *Service) GetInbox(ctx context.Context, actor Actor) (Inbox, error) {
	ids := []uuid.UUID{actor.ID}
	person := actor.Kind == store.KindHuman
	if person {
		agents, err := s.Store.ListAgents(ctx, actor.ID)
		if err != nil {
			return Inbox{}, err
		}
		for _, a := range agents {
			ids = append(ids, a.ID)
		}
	}
	now := s.Now()
	c := s.newNoteCache()
	var in Inbox
	rows, err := s.Store.OpenQuestionsTo(ctx, ids, maxInboxNotes)
	if err != nil {
		return Inbox{}, err
	}
	if in.Questions, err = c.notes(ctx, rows, now); err != nil {
		return Inbox{}, err
	}
	if rows, err = s.Store.AnswersToQuestionsBy(ctx, actor.ID, now.Add(-inboxAnswerAge), maxInboxNotes); err != nil {
		return Inbox{}, err
	}
	if in.Answers, err = c.notes(ctx, rows, now); err != nil {
		return Inbox{}, err
	}
	if person {
		if rows, err = s.Store.OverdueQuestions(ctx, actor.ID, ids, now, maxInboxNotes); err != nil {
			return Inbox{}, err
		}
		if in.Escalated, err = c.notes(ctx, rows, now); err != nil {
			return Inbox{}, err
		}
	}
	return in, nil
}

// Context returns notes on the targets, open workspaces and recent changes
// touching them. Without targets it returns the effort's latest notes and
// changes.
func (s *Service) Context(ctx context.Context, actor Actor, repo, effort string, targets []string) (NoteContext, error) {
	targets, err := cleanTargets(targets)
	if err != nil {
		return NoteContext{}, err
	}
	v, err := s.boardEffort(ctx, repo, effort)
	if err != nil {
		return NoteContext{}, err
	}
	now := s.Now()
	c := s.newNoteCache()
	var out NoteContext
	rows, err := s.listNotes(ctx, store.NoteFilter{EffortID: v.Effort.ID}, targets, contextNotes)
	if err != nil {
		return NoteContext{}, err
	}
	if out.Notes, err = c.notes(ctx, rows, now); err != nil {
		return NoteContext{}, err
	}
	for _, w := range v.Workspaces {
		owner, err := c.principal(ctx, w.OwnerID)
		if err != nil {
			return NoteContext{}, err
		}
		out.Working = append(out.Working, ContextWorkspace{Workspace: w, Owner: owner})
	}
	chain, err := s.Store.Chain(ctx, v.Repo.ID, v.Effort.Head, v.Effort.Base, contextChainScan)
	if err != nil {
		return NoteContext{}, err
	}
	for _, r := range chain {
		files, err := s.Store.FileChanges(ctx, v.Repo.ID, r.Hash)
		if err != nil {
			return NoteContext{}, err
		}
		if len(targets) == 0 || filesMatch(files, targets) {
			out.RecentChanges = append(out.RecentChanges, LogEntry{ChangesetRow: r, Files: files})
			if len(out.RecentChanges) == contextChanges {
				break
			}
		}
	}
	return out, nil
}

// GetNote returns one note by id or an 8+ character id prefix.
func (s *Service) GetNote(ctx context.Context, actor Actor, id string) (Note, error) {
	row, err := s.findNote(ctx, id)
	if err != nil {
		return Note{}, err
	}
	return s.newNoteCache().note(ctx, row, s.Now())
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
	symbols := !looksLikePath(target) && !looksLikePath(want)
	prefix := func(a, b string) bool {
		a = trimSep(a)
		if len(b) <= len(a) || b[:len(a)] != a {
			return false
		}
		return b[len(a)] == '/' || (symbols && b[len(a)] == '.')
	}
	return prefix(target, want) || prefix(want, target)
}

// fileExts are extensions that make a target without a slash a file name
// ("auth.go") rather than a symbol ("auth.Login").
var fileExts = map[string]bool{
	"go": true, "mod": true, "sum": true, "proto": true, "sql": true, "md": true, "txt": true,
	"json": true, "yaml": true, "yml": true, "toml": true, "ini": true, "env": true, "lock": true,
	"js": true, "mjs": true, "cjs": true, "ts": true, "tsx": true, "jsx": true, "css": true, "scss": true, "html": true, "svg": true,
	"py": true, "rb": true, "rs": true, "java": true, "kt": true, "swift": true, "c": true, "h": true, "cc": true, "cpp": true, "hpp": true,
	"cs": true, "php": true, "sh": true, "bash": true, "zsh": true, "ps1": true, "tf": true, "xml": true, "csv": true,
}

// looksLikePath reports whether a target names a file or directory: it has
// a slash, or its last dot-separated part is a known file extension.
func looksLikePath(s string) bool {
	if strings.Contains(s, "/") {
		return true
	}
	i := strings.LastIndexByte(s, '.')
	return i >= 0 && fileExts[strings.ToLower(s[i+1:])]
}

func trimSep(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '/' || s[len(s)-1] == '.') {
		s = s[:len(s)-1]
	}
	return s
}
