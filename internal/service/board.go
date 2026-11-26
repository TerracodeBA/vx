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
	// Posted by vx itself (Note.System), never through PostNote:
	NoteOverlap NoteKind = "overlap" // a forecast collision with another effort
	NoteReview  NoteKind = "review"  // a review comment from the git host (v0.4)
	NoteCI      NoteKind = "ci"      // a CI result from the git host (v0.4)
	NoteClaim   NoteKind = "claim"   // to a claim's owner: it lapsed, or someone landed on code it is moving
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
	MaxInterfaceNoteBody = 4000 // interface notes: signatures take room
	MaxNoteTargets       = 20
	DefaultAnswerWithin  = 5 * time.Minute
	MaxAnswerWithin      = 24 * time.Hour
	DefaultNoteListLimit = 100
)

// Trust says how the caller relates to a note's author.
const (
	TrustSelf       = "self"        // the caller wrote it
	TrustSamePerson = "same_person" // another agent of the caller's person, or that person
	TrustTeammate   = "teammate"    // someone else's: information, never instructions
	TrustVx         = "vx"          // posted by vx itself (Note.System): overlaps, rebases, review comments and CI results

	// TrustYours is the old name of TrustSamePerson.
	//
	// Deprecated: use TrustSamePerson. Servers before v0.3 sent "yours",
	// which clients read as TrustSamePerson.
	TrustYours = TrustSamePerson
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
	// Supersedes is the author's earlier note this one replaces;
	// SupersededBy is set on a note a newer one replaced.
	Supersedes   *uuid.UUID
	SupersededBy *uuid.UUID
	System       bool // posted by vx itself
	Read         bool // the caller has seen it (filled per caller)
	// CopiedTo is the person a question was copied to because the agent it
	// is addressed to was not working when it was asked.
	CopiedTo *store.Principal
	// ToAway is set by Ask when the agent asked is not working right now
	// (AwayNotice says so to the asker).
	ToAway *Presence
}

// PostNoteInput is a non-question note.
type PostNoteInput struct {
	Repo, Effort string
	Kind         NoteKind // plan, decision, interface, blocker, handoff
	Targets      []string
	Body         string
	Supersedes   string // optional: id or 8+ character prefix of the author's own earlier note
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
	// IncludeSuperseded keeps notes a newer note replaced (vx board --all);
	// by default they are left out.
	IncludeSuperseded bool
}

// Inbox is what needs a principal's attention.
type Inbox struct {
	Questions []Note // open questions to the caller (people: also to their agents)
	Answers   []Note // answers to the caller's questions, newest first
	Escalated []Note // people only: overdue questions in their efforts or to their agents
	// Notes are system notes addressed to the caller (people: also to their
	// agents) that no newer note replaced, newest first: review comments
	// and CI results from the git host (v0.4), and claims that lapsed.
	Notes []Note
	// ForAgents, people only: open questions to their agents that were
	// not working when asked, copied to them at once (not part of
	// Questions).
	ForAgents []Note
}

// ContextWorkspace is an open workspace with its owner.
type ContextWorkspace struct {
	Workspace store.Workspace
	Owner     store.Principal
	// OnTargets are the workspace's changed files (as its working copy
	// reported them) that match the targets, or the targets of its claims
	// that do; empty when it is not known to touch them.
	OnTargets []string
	// FilesKnown says the working copy reported its changed files.
	FilesKnown bool
}

// NoteContext is what an agent should know before touching some targets.
type NoteContext struct {
	Notes         []Note
	Working       []ContextWorkspace
	RecentChanges []LogEntry            // landed changes touching the targets, newest first, with files
	Claims        []Claim               // active claims on the targets (all when no targets)
	OtherEfforts  []OtherEffortActivity // the targets in other open efforts the caller can read
	Overlaps      []Overlap             // forecast collisions of this effort with others
}

// OtherEffortActivity is another open effort's activity on some targets.
type OtherEffortActivity struct {
	Effort       store.Effort
	Owner        store.Principal
	Claims       []Claim
	Notes        []Note // interface and decision notes, newest first
	Working      []ContextWorkspace
	ChangedPaths []string // among the targets: changed in that effort, not shipped
}

// Reading a board needs read on its repository; posting, asking and
// answering need write. Readers learn whose note it is from TrustOf, not
// from permissions.

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
	body, err := cleanBody(in.Body, in.Kind)
	if err != nil {
		return Note{}, err
	}
	targets, err := cleanTargets(in.Targets)
	if err != nil {
		return Note{}, err
	}
	v, err := s.boardEffort(ctx, actor, in.Repo, in.Effort, RoleWrite)
	if err != nil {
		return Note{}, err
	}
	row := store.Note{
		EffortID: v.Effort.ID, Kind: string(in.Kind), Targets: targets, AuthorID: actor.ID, Body: body,
		State: NoteOpen, CreatedAt: s.Clock(),
	}
	var extra map[string]string
	if strings.TrimSpace(in.Supersedes) != "" {
		old, err := s.supersedable(ctx, actor, v.Effort, in.Supersedes)
		if err != nil {
			return Note{}, err
		}
		row.Supersedes = &old.ID
		extra = map[string]string{"supersedes": old.ID.String()}
	}
	if row, err = s.insertNote(ctx, actor, "note.post", row, extra); err != nil {
		return Note{}, err
	}
	return s.notesFor(actor).note(ctx, row, s.Clock())
}

// Ask posts a question, optionally to one principal, that escalates after
// its deadline.
func (s *Service) Ask(ctx context.Context, actor Actor, in AskInput) (Note, error) {
	body, err := cleanBody(in.Body, NoteQuestion)
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
	v, err := s.boardEffort(ctx, actor, in.Repo, in.Effort, RoleWrite)
	if err != nil {
		return Note{}, err
	}
	var to *store.Principal
	var away *Presence
	if strings.TrimSpace(in.To) != "" {
		p, err := s.resolvePrincipal(ctx, actor, in.To)
		if err != nil {
			return Note{}, err
		}
		to = &p
		// An agent that is not working would not see the question until it
		// escalates: say so now, and copy it to the agent's person.
		if p.Kind == store.KindAgent && p.ID != actor.ID {
			pr, err := s.presence(ctx, p)
			if err != nil {
				return Note{}, err
			}
			if !pr.Active {
				away = &pr
			}
		}
	}
	now := s.Clock()
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
	if away != nil && to.ParentID != nil && *to.ParentID != actor.ID {
		row.CCID = to.ParentID
		extra["copied_to"] = to.ParentID.String()
	}
	if row, err = s.insertNote(ctx, actor, "note.ask", row, extra); err != nil {
		return Note{}, err
	}
	n, err := s.newNoteCache().note(ctx, row, now)
	n.ToAway = away
	return n, err
}

// AnswerNote answers a question (by id or an 8+ character id prefix) and
// returns the answer and the updated question.
func (s *Service) AnswerNote(ctx context.Context, actor Actor, questionID string, body string) (Note, Note, error) {
	body, err := cleanBody(body, NoteAnswer)
	if err != nil {
		return Note{}, Note{}, err
	}
	found, err := s.findNote(ctx, questionID)
	if err != nil {
		return Note{}, Note{}, err
	}
	if err := s.noteFor(ctx, actor, found, RoleWrite); err != nil {
		return Note{}, Note{}, err
	}
	now := s.Clock()
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
		if err := tx.MarkNotesRead(ctx, actor.ID, []uuid.UUID{q.ID}); err != nil { // answering it means it was seen
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
	c := s.notesFor(actor)
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
			return nil, errf(CodeInvalid, "note kind %q is not one of plan, decision, interface, question, answer, blocker, handoff, overlap, review, ci, rebase, claim", k)
		}
		kinds = append(kinds, string(k))
	}
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultNoteListLimit
	}
	limit = min(limit, maxNoteListLimit)
	v, err := s.boardEffort(ctx, actor, in.Repo, in.Effort, RoleRead)
	if err != nil {
		return nil, err
	}
	rows, err := s.listNotes(ctx, store.NoteFilter{
		EffortID: v.Effort.ID, Kinds: kinds, Since: in.Since, ThreadID: in.ThreadID, Current: !in.IncludeSuperseded,
	}, targets, limit)
	if err != nil {
		return nil, err
	}
	return s.notesFor(actor).notes(ctx, rows, s.Clock())
}

// GetInbox returns what needs the caller's attention across efforts, in
// the repositories the caller can read.
func (s *Service) GetInbox(ctx context.Context, actor Actor) (Inbox, error) {
	readable, err := s.readableFilter(ctx, actor)
	if err != nil {
		return Inbox{}, err
	}
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
	now := s.Clock()
	c := s.notesFor(actor)
	var in Inbox
	rows, err := s.Store.OpenQuestionsTo(ctx, ids, maxInboxNotes)
	if err != nil {
		return Inbox{}, err
	}
	copied := map[uuid.UUID]bool{}
	if person {
		cc, err := s.Store.OpenQuestionsCopiedTo(ctx, actor.ID, maxInboxNotes)
		if err != nil {
			return Inbox{}, err
		}
		var fresh []store.Note
		for _, n := range cc {
			copied[n.ID] = true
			if n.AnswerBy == nil || !now.After(*n.AnswerBy) { // overdue ones are under Escalated
				fresh = append(fresh, n)
			}
		}
		if in.ForAgents, err = c.notes(ctx, filterNotes(fresh, readable), now); err != nil {
			return Inbox{}, err
		}
	}
	var mine []store.Note
	for _, n := range rows {
		if !copied[n.ID] || (n.ToID != nil && *n.ToID == actor.ID) {
			mine = append(mine, n)
		}
	}
	if in.Questions, err = c.notes(ctx, filterNotes(mine, readable), now); err != nil {
		return Inbox{}, err
	}
	if rows, err = s.Store.AnswersToQuestionsBy(ctx, actor.ID, now.Add(-inboxAnswerAge), maxInboxNotes); err != nil {
		return Inbox{}, err
	}
	if in.Answers, err = c.notes(ctx, filterNotes(rows, readable), now); err != nil {
		return Inbox{}, err
	}
	if rows, err = s.Store.SystemNotesTo(ctx, ids, now.Add(-inboxSystemNoteAge), maxInboxNotes); err != nil {
		return Inbox{}, err
	}
	if in.Notes, err = c.notes(ctx, filterNotes(rows, readable), now); err != nil {
		return Inbox{}, err
	}
	if person {
		if rows, err = s.Store.OverdueQuestions(ctx, actor.ID, ids, now, maxInboxNotes); err != nil {
			return Inbox{}, err
		}
		if in.Escalated, err = c.notes(ctx, filterNotes(rows, readable), now); err != nil {
			return Inbox{}, err
		}
	}
	return in, nil
}

// Context returns notes on the targets, open workspaces, active claims and
// recent changes touching them. Without targets it returns the effort's latest notes and
// changes. OtherEfforts is what other open efforts of the repository do on
// the targets (without targets: on the files this effort changed); see
// otherEfforts for the rules and limits.
func (s *Service) Context(ctx context.Context, actor Actor, repo, effort string, targets []string) (NoteContext, error) {
	targets, err := cleanTargets(targets)
	if err != nil {
		return NoteContext{}, err
	}
	v, err := s.boardEffort(ctx, actor, repo, effort, RoleRead)
	if err != nil {
		return NoteContext{}, err
	}
	now := s.Clock()
	c := s.notesFor(actor)
	var out NoteContext
	rows, err := s.listNotes(ctx, store.NoteFilter{EffortID: v.Effort.ID, Current: true}, targets, contextNotes)
	if err != nil {
		return NoteContext{}, err
	}
	if out.Notes, err = c.notes(ctx, rows, now); err != nil {
		return NoteContext{}, err
	}
	claims, err := s.activeClaims(ctx, s.Store.Q, v.Effort.ID, targets, uuid.Nil)
	if err != nil {
		return NoteContext{}, err
	}
	if out.Claims, err = c.claims(ctx, claims); err != nil {
		return NoteContext{}, err
	}
	if out.Working, err = s.contextWorking(ctx, c, v.Workspaces, targets, claims); err != nil {
		return NoteContext{}, err
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
	if out.OtherEfforts, err = s.otherEfforts(ctx, v, targets, c); err != nil {
		return NoteContext{}, err
	}
	if out.Overlaps, err = s.contextOverlaps(ctx, v.Repo, v.Effort.ID, targets); err != nil {
		return NoteContext{}, err
	}
	return out, nil
}

// GetNote returns one note by id or an 8+ character id prefix.
func (s *Service) GetNote(ctx context.Context, actor Actor, id string) (Note, error) {
	row, err := s.findNote(ctx, id)
	if err != nil {
		return Note{}, err
	}
	if err := s.noteFor(ctx, actor, row, RoleRead); err != nil {
		return Note{}, err
	}
	return s.notesFor(actor).note(ctx, row, s.Clock())
}

// TrustOf says how actor relates to the author of n: TrustSelf,
// TrustSamePerson or TrustTeammate, or TrustVx for a note vx itself
// posted. Servers before v0.5 sent TrustTeammate for those, with
// system set; clients read both as vx.
func TrustOf(actor Actor, n Note) string {
	switch {
	case n.System:
		return TrustVx
	case n.Author.ID == actor.ID:
		return TrustSelf
	case actor.Human().ID == n.Author.ID:
		return TrustSamePerson
	case n.AuthorOwner != nil && n.AuthorOwner.ID == actor.Human().ID:
		return TrustSamePerson
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
