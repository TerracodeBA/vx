package service

// Helpers for the board in board.go: validation, lookups and turning stored
// notes into Notes.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

const (
	MaxNoteTarget    = 300 // characters in one target
	MinNoteIDPrefix  = 8   // shortest note id prefix AnswerNote and GetNote accept
	maxNoteListLimit = 1000
	maxInboxNotes    = 200
	inboxAnswerAge   = 7 * 24 * time.Hour // answers older than this leave the inbox
	contextNotes     = 50
	contextChanges   = 20
	contextChainScan = 500 // landed changes Context looks back through
	noteScanPage     = 500 // notes read per query while filtering by target
)

var postKinds = map[NoteKind]bool{
	NotePlan: true, NoteDecision: true, NoteInterface: true, NoteBlocker: true, NoteHandoff: true,
}

var allKinds = map[NoteKind]bool{
	NotePlan: true, NoteDecision: true, NoteInterface: true, NoteQuestion: true, NoteAnswer: true,
	NoteBlocker: true, NoteHandoff: true,
}

// cleanBody drops control characters other than newline and tab (and
// bidirectional overrides, which can make text read differently than it
// is), trims the result and checks its length.
func cleanBody(body string) (string, error) {
	out := strings.TrimSpace(stripControls(body))
	switch n := utf8.RuneCountInString(out); {
	case n == 0:
		return "", errf(CodeInvalid, "the note is empty; say what others should know")
	case n > MaxNoteBody:
		return "", errf(CodeInvalid, "the note is %d characters; keep it to %d and point to files or changes for details", n, MaxNoteBody)
	}
	return out, nil
}

// stripControls drops control characters other than newline and tab, and
// bidirectional overrides, from text others will read.
func stripControls(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || !(unicode.IsControl(r) || isBidiControl(r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isBidiControl(r rune) bool {
	return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f
}

// cleanTargets trims targets, drops duplicates and checks each one.
func cleanTargets(targets []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for i, t := range targets {
		t = strings.TrimSpace(t)
		switch {
		case t == "":
			return nil, errf(CodeInvalid, "target %d is empty; give a path (internal/http/limit.go, internal/http/) or a symbol (auth.Login)", i+1)
		case strings.IndexFunc(t, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
			return nil, errf(CodeInvalid, "target %q contains whitespace; give one path or symbol per target", t)
		case utf8.RuneCountInString(t) > MaxNoteTarget:
			return nil, errf(CodeInvalid, "target %.40q... is %d characters; keep targets to %d", t, utf8.RuneCountInString(t), MaxNoteTarget)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) > MaxNoteTargets {
		return nil, errf(CodeInvalid, "%d targets; give at most %d, or a directory that covers them", len(out), MaxNoteTargets)
	}
	return out, nil
}

// matchAny reports whether any of a note's targets matches any wanted one.
func matchAny(targets, want []string) bool {
	for _, t := range targets {
		for _, w := range want {
			if TargetsMatch(t, w) {
				return true
			}
		}
	}
	return false
}

func filesMatch(files []store.FileChange, want []string) bool {
	for _, f := range files {
		for _, w := range want {
			if TargetsMatch(f.Path, w) {
				return true
			}
		}
	}
	return false
}

// boardEffort loads the effort a board call is about, checking need on its
// repository.
func (s *Service) boardEffort(ctx context.Context, actor Actor, repo, effort string, need RepoRole) (EffortView, error) {
	repo, effort = strings.TrimSpace(repo), strings.TrimSpace(effort)
	if repo == "" || effort == "" {
		return EffortView{}, errf(CodeInvalid, "say which repo and effort the board is in; inside a clone vx fills them in")
	}
	v, err := s.effortFor(ctx, actor, repo, effort, need)
	if errors.Is(err, store.ErrNotFound) {
		return EffortView{}, errf(CodeNotFound, "no effort %s in repo %s; `vx effort list --repo %s` shows them", effort, repo, repo)
	}
	return v, err
}

// insertNote stores a note and logs op in one transaction.
func (s *Service) insertNote(ctx context.Context, actor Actor, op string, n store.Note, extra map[string]string) (store.Note, error) {
	var out store.Note
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if out, err = tx.InsertNote(ctx, n); err != nil {
			return err
		}
		after := map[string]any{"effort": out.EffortSlug, "kind": out.Kind, "targets": out.Targets}
		for k, v := range extra {
			after[k] = v
		}
		return tx.LogOp(ctx, &out.RepoID, &actor.ID, op, out.ID.String(), nil, after)
	})
	return out, classify(err)
}

// listNotes lists notes matching f and, when want is set, with a target
// matching one of want. Target matching happens here with TargetsMatch, so
// the store pages through the effort until it has limit matches.
func (s *Service) listNotes(ctx context.Context, f store.NoteFilter, want []string, limit int) ([]store.Note, error) {
	if len(want) == 0 {
		f.Limit = limit
		return s.Store.ListNotes(ctx, f)
	}
	var out []store.Note
	f.Limit = noteScanPage
	for {
		rows, err := s.Store.ListNotes(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if matchAny(r.Targets, want) {
				out = append(out, r)
				if len(out) == limit {
					return out, nil
				}
			}
		}
		if len(rows) < f.Limit {
			return out, nil
		}
		f.Before = rows[len(rows)-1].Cursor()
	}
}

// findNote resolves a full note id or a unique prefix of at least
// MinNoteIDPrefix characters.
func (s *Service) findNote(ctx context.Context, id string) (store.Note, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if full, err := uuid.Parse(id); err == nil {
		n, err := s.Store.NoteByID(ctx, full)
		if errors.Is(err, store.ErrNotFound) {
			return store.Note{}, errf(CodeNotFound, "no note %s; `vx board` lists the effort's notes", id)
		}
		return n, err
	}
	if len(id) < MinNoteIDPrefix {
		return store.Note{}, errf(CodeInvalid, "note id %q is too short: give at least %d characters", id, MinNoteIDPrefix)
	}
	n, err := s.Store.NoteByPrefix(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Note{}, errf(CodeNotFound, "no note has an id starting with %s; `vx board` lists the effort's notes", id)
	case errors.Is(err, store.ErrAmbiguous):
		return store.Note{}, errf(CodeInvalid, "several notes have ids starting with %s; give more characters", id)
	}
	return n, err
}

func shortID(id uuid.UUID) string { return id.String()[:MinNoteIDPrefix] }

var refKinds = map[string]store.PrincipalKind{"user": store.KindHuman, "agent": store.KindAgent, "service": store.KindService}

// resolvePrincipal finds who a question is addressed to: a ref
// ("user:bob", "agent:bob/backend"), an agent handle ("bob/backend"), an
// agent name relative to the caller's person ("backend", as tokenOwner
// reads it) or, failing that, a person's or service's handle ("bob").
func (s *Service) resolvePrincipal(ctx context.Context, actor Actor, to string) (store.Principal, error) {
	to = strings.TrimSpace(to)
	if prefix, handle, ok := strings.Cut(to, ":"); ok {
		kind, known := refKinds[prefix]
		if !known {
			return store.Principal{}, errf(CodeInvalid, "%q is not a principal ref; refs look like user:bob, agent:bob/backend or service:ci", to)
		}
		p, err := s.Store.PrincipalByHandle(ctx, handle)
		if err == nil && p.Kind != kind {
			err = store.ErrNotFound
		}
		if errors.Is(err, store.ErrNotFound) {
			return store.Principal{}, errf(CodeNotFound, "no principal %s", to)
		}
		return p, err
	}
	if strings.Contains(to, "/") {
		p, err := s.Store.PrincipalByHandle(ctx, to)
		if errors.Is(err, store.ErrNotFound) {
			return store.Principal{}, errf(CodeNotFound, "no agent %s", to)
		}
		return p, err
	}
	agent := actor.Human().Handle + "/" + to
	p, err := s.Store.PrincipalByHandle(ctx, agent)
	if !errors.Is(err, store.ErrNotFound) {
		return p, err
	}
	p, err = s.Store.PrincipalByHandle(ctx, to)
	if errors.Is(err, store.ErrNotFound) {
		return store.Principal{}, errf(CodeNotFound,
			"no agent %s and no person %s; give an agent name (backend), a handle (bob/backend) or a ref (user:bob). `vx agent list` shows your agents", agent, to)
	}
	return p, err
}

// noteCache turns stored notes into Notes, loading each principal once.
type noteCache struct {
	s          *Service
	principals map[uuid.UUID]store.Principal
}

func (s *Service) newNoteCache() *noteCache {
	return &noteCache{s: s, principals: map[uuid.UUID]store.Principal{}}
}

func (c *noteCache) principal(ctx context.Context, id uuid.UUID) (store.Principal, error) {
	if p, ok := c.principals[id]; ok {
		return p, nil
	}
	p, err := c.s.Store.PrincipalByID(ctx, id)
	if err != nil {
		return store.Principal{}, fmt.Errorf("principal %s: %w", id, err)
	}
	c.principals[id] = p
	return p, nil
}

func (c *noteCache) note(ctx context.Context, r store.Note, now time.Time) (Note, error) {
	n := Note{
		ID: r.ID, RepoName: r.RepoName, EffortSlug: r.EffortSlug, EffortID: r.EffortID, ThreadID: r.ThreadID,
		Kind: NoteKind(r.Kind), Targets: r.Targets, Body: r.Body, State: noteState(r, now), AnswerBy: r.AnswerBy,
		CreatedAt: r.CreatedAt,
	}
	var err error
	if n.Author, err = c.principal(ctx, r.AuthorID); err != nil {
		return Note{}, err
	}
	if n.Author.ParentID != nil {
		owner, err := c.principal(ctx, *n.Author.ParentID)
		if err != nil {
			return Note{}, err
		}
		n.AuthorOwner = &owner
	}
	if r.ToID != nil {
		to, err := c.principal(ctx, *r.ToID)
		if err != nil {
			return Note{}, err
		}
		n.To = &to
	}
	return n, nil
}

func (c *noteCache) notes(ctx context.Context, rows []store.Note, now time.Time) ([]Note, error) {
	out := make([]Note, 0, len(rows))
	for _, r := range rows {
		n, err := c.note(ctx, r, now)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// noteState is the state a reader sees: an open question past its deadline
// reads as escalated.
func noteState(r store.Note, now time.Time) string {
	if r.Kind == string(NoteQuestion) && r.State == NoteOpen && r.AnswerBy != nil && now.After(*r.AnswerBy) {
		return NoteEscalated
	}
	return r.State
}
