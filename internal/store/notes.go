package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrAmbiguous is returned when an id prefix matches more than one row.
var ErrAmbiguous = errors.New("ambiguous")

// Note is a stored board note. RepoName and EffortSlug are read from the
// note's repo and effort.
type Note struct {
	ID         uuid.UUID
	Seq        int64 // insertion order; breaks ties between equal created_at
	RepoID     uuid.UUID
	EffortID   uuid.UUID
	ThreadID   uuid.UUID // the root note; equals ID for a root
	Kind       string
	Targets    []string
	AuthorID   uuid.UUID
	ToID       *uuid.UUID
	Body       string
	State      string // open, answered, closed; "escalated" is computed by readers
	AnswerBy   *time.Time
	CreatedAt  time.Time
	RepoName   string
	EffortSlug string
	// Supersedes is the earlier note this one replaces. SupersededBy is read
	// from the newest note that replaced this one.
	Supersedes   *uuid.UUID
	SupersededBy *uuid.UUID
	System       bool // posted by vx itself
	// CCID is the person a question was copied to because the agent it is
	// addressed to was not working when it was asked.
	CCID *uuid.UUID
	// SimilarTo is the open question this question closely matched when it
	// was asked. Similar is read from the later questions that matched this
	// one, oldest first.
	SimilarTo *uuid.UUID
	Similar   []uuid.UUID
}

const noteCols = `n.id, n.seq, n.repo_id, n.effort_id, n.thread_id, n.kind, n.targets, n.author_id, n.to_id,
	n.body, n.state, n.answer_by, n.created_at, r.name, e.slug, n.supersedes,
	(select s.id from notes s where s.supersedes = n.id order by s.created_at desc, s.seq desc limit 1), n.system, n.cc_id,
	n.similar_to, coalesce((select array_agg(s.id order by s.created_at, s.seq) from notes s where s.similar_to = n.id), '{}')`

// notSuperseded is a where clause that leaves out notes a newer one replaced.
const notSuperseded = `not exists (select 1 from notes s where s.supersedes = n.id)`

const noteFrom = ` from notes n join efforts e on e.id = n.effort_id join repos r on r.id = n.repo_id `

func scanNote(row interface{ Scan(...any) error }) (Note, error) {
	var n Note
	err := row.Scan(&n.ID, &n.Seq, &n.RepoID, &n.EffortID, &n.ThreadID, &n.Kind, &n.Targets, &n.AuthorID, &n.ToID,
		&n.Body, &n.State, &n.AnswerBy, &n.CreatedAt, &n.RepoName, &n.EffortSlug, &n.Supersedes, &n.SupersededBy, &n.System, &n.CCID,
		&n.SimilarTo, &n.Similar)
	if n.Targets == nil {
		n.Targets = []string{}
	}
	return n, err
}

func (q Q) queryNotes(ctx context.Context, sql string, args ...any) ([]Note, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// InsertNote stores a note. A zero ID gets a new one; a zero ThreadID makes
// the note a thread root; a zero CreatedAt means now. The repo is taken from
// the effort. Supersedes, System and SimilarTo are stored as given;
// SupersededBy and Similar are ignored.
func (q Q) InsertNote(ctx context.Context, n Note) (Note, error) {
	if n.ID == uuid.Nil {
		n.ID = NewID()
	}
	if n.ThreadID == uuid.Nil {
		n.ThreadID = n.ID
	}
	if n.State == "" {
		n.State = "open"
	}
	if n.Targets == nil {
		n.Targets = []string{}
	}
	var created *time.Time
	if !n.CreatedAt.IsZero() {
		created = &n.CreatedAt
	}
	tag, err := q.db.Exec(ctx, `insert into notes (id, repo_id, effort_id, thread_id, kind, targets, author_id, to_id, body, state, answer_by, created_at, supersedes, system, cc_id, similar_to)
		select $1::uuid, e.repo_id, e.id, $3::uuid, $4::text, $5::text[], $6::uuid, $7::uuid, $8::text, $9::text, $10::timestamptz, coalesce($11::timestamptz, now()), $12::uuid, $13::boolean, $14::uuid, $15::uuid
		from efforts e where e.id = $2::uuid`,
		n.ID, n.EffortID, n.ThreadID, n.Kind, n.Targets, n.AuthorID, n.ToID, n.Body, n.State, n.AnswerBy, created, n.Supersedes, n.System, n.CCID, n.SimilarTo)
	if err != nil {
		return Note{}, err
	}
	if tag.RowsAffected() == 0 {
		return Note{}, fmt.Errorf("effort: %w", ErrNotFound)
	}
	return q.NoteByID(ctx, n.ID)
}

// NoteByID loads a note.
func (q Q) NoteByID(ctx context.Context, id uuid.UUID) (Note, error) {
	n, err := scanNote(q.db.QueryRow(ctx, `select `+noteCols+noteFrom+`where n.id = $1`, id))
	return n, notFound(err, "note "+id.String())
}

// NoteForUpdate loads a note and locks its row until the transaction ends.
func (q Q) NoteForUpdate(ctx context.Context, id uuid.UUID) (Note, error) {
	n, err := scanNote(q.db.QueryRow(ctx, `select `+noteCols+noteFrom+`where n.id = $1 for update of n`, id))
	return n, notFound(err, "note "+id.String())
}

// NoteByPrefix finds the one note whose id starts with prefix (lowercase
// hex and dashes). It returns ErrNotFound or ErrAmbiguous otherwise.
func (q Q) NoteByPrefix(ctx context.Context, prefix string) (Note, error) {
	prefix = strings.ToLower(prefix)
	for _, c := range prefix {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c == '-') {
			return Note{}, fmt.Errorf("note %q: %w", prefix, ErrNotFound)
		}
	}
	notes, err := q.queryNotes(ctx, `select `+noteCols+noteFrom+`where n.id::text like $1 || '%' limit 2`, prefix)
	if err != nil {
		return Note{}, err
	}
	switch len(notes) {
	case 0:
		return Note{}, fmt.Errorf("note %s: %w", prefix, ErrNotFound)
	case 1:
		return notes[0], nil
	}
	return Note{}, fmt.Errorf("note prefix %s: %w", prefix, ErrAmbiguous)
}

// NoteCursor is a position in a newest-first listing: the listing continues
// with notes older than it.
type NoteCursor struct {
	CreatedAt time.Time
	Seq       int64
}

// Cursor returns the position right after n.
func (n Note) Cursor() *NoteCursor { return &NoteCursor{CreatedAt: n.CreatedAt, Seq: n.Seq} }

// NoteFilter selects an effort's notes. Zero fields do not filter.
type NoteFilter struct {
	EffortID uuid.UUID
	Kinds    []string
	Since    time.Time // created at or after
	ThreadID uuid.UUID
	Before   *NoteCursor
	Limit    int // 0: 100
	// Current leaves out notes a newer note superseded.
	Current bool
}

// ListNotes lists an effort's notes, newest first.
func (q Q) ListNotes(ctx context.Context, f NoteFilter) ([]Note, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	var since *time.Time
	if !f.Since.IsZero() {
		since = &f.Since
	}
	var thread *uuid.UUID
	if f.ThreadID != uuid.Nil {
		thread = &f.ThreadID
	}
	var beforeAt *time.Time
	var beforeSeq int64
	if f.Before != nil {
		beforeAt, beforeSeq = &f.Before.CreatedAt, f.Before.Seq
	}
	return q.queryNotes(ctx, `select `+noteCols+noteFrom+`where n.effort_id = $1
		and (coalesce(cardinality($2::text[]), 0) = 0 or n.kind = any($2))
		and ($3::timestamptz is null or n.created_at >= $3)
		and ($4::uuid is null or n.thread_id = $4)
		and ($5::timestamptz is null or (n.created_at, n.seq) < ($5, $6))
		and (not $8::boolean or `+notSuperseded+`)
		order by n.created_at desc, n.seq desc limit $7`,
		f.EffortID, f.Kinds, since, thread, beforeAt, beforeSeq, f.Limit, f.Current)
}

// OpenQuestionsTo returns open questions (overdue ones included) addressed
// to any of the principals, newest first.
func (q Q) OpenQuestionsTo(ctx context.Context, to []uuid.UUID, limit int) ([]Note, error) {
	return q.queryNotes(ctx, `select `+noteCols+noteFrom+`where n.to_id = any($1) and n.state = 'open' and n.kind = 'question'
		order by n.created_at desc, n.seq desc limit $2`, to, limit)
}

// OpenQuestionsIn returns the effort's open questions (overdue ones
// included), newest first.
func (q Q) OpenQuestionsIn(ctx context.Context, effortID uuid.UUID, limit int) ([]Note, error) {
	return q.queryNotes(ctx, `select `+noteCols+noteFrom+`where n.effort_id = $1 and n.state = 'open' and n.kind = 'question'
		order by n.created_at desc, n.seq desc limit $2`, effortID, limit)
}

// OpenQuestionsCopiedTo returns open questions copied to person (their
// agent was not working when they were asked), newest first.
func (q Q) OpenQuestionsCopiedTo(ctx context.Context, person uuid.UUID, limit int) ([]Note, error) {
	return q.queryNotes(ctx, `select `+noteCols+noteFrom+`where n.cc_id = $1 and n.state = 'open' and n.kind = 'question'
		order by n.created_at desc, n.seq desc limit $2`, person, limit)
}

// AnswersToQuestionsBy returns answers created at or after since in threads
// whose root is a question asked by author, newest first.
func (q Q) AnswersToQuestionsBy(ctx context.Context, author uuid.UUID, since time.Time, limit int) ([]Note, error) {
	return q.queryNotes(ctx, `select `+noteCols+noteFrom+`join notes root on root.id = n.thread_id
		where root.author_id = $1 and root.kind = 'question' and n.kind = 'answer' and n.created_at >= $2
		order by n.created_at desc, n.seq desc limit $3`, author, since, limit)
}

// OverdueQuestions returns open questions whose deadline is before now and
// that are either in an effort owned by owner or addressed to one of to,
// newest first.
func (q Q) OverdueQuestions(ctx context.Context, owner uuid.UUID, to []uuid.UUID, now time.Time, limit int) ([]Note, error) {
	return q.queryNotes(ctx, `select `+noteCols+noteFrom+`where n.kind = 'question' and n.state = 'open' and n.answer_by < $3
		and (e.owner_id = $1 or n.to_id = any($2))
		order by n.created_at desc, n.seq desc limit $4`, owner, to, now, limit)
}

// MarkNoteAnswered marks a question answered.
func (q Q) MarkNoteAnswered(ctx context.Context, id uuid.UUID) error {
	_, err := q.db.Exec(ctx, `update notes set state = 'answered' where id = $1 and kind = 'question'`, id)
	return err
}

// MarkNotesRead records that principal was shown the notes. Ids of notes
// that do not exist are skipped; marking a note twice keeps the first time.
func (q Q) MarkNotesRead(ctx context.Context, principal uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := q.db.Exec(ctx, `insert into note_reads (principal_id, note_id)
		select $1, n.id from notes n where n.id = any($2::uuid[])
		on conflict do nothing`, principal, ids)
	return err
}

// NotesRead returns which of the notes principal has seen.
func (q Q) NotesRead(ctx context.Context, principal uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx, `select note_id from note_reads where principal_id = $1 and note_id = any($2::uuid[])`, principal, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// NoteRepos returns the repository of each of the notes that exist.
func (q Q) NoteRepos(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx, `select id, repo_id from notes where id = any($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, repo uuid.UUID
		if err := rows.Scan(&id, &repo); err != nil {
			return nil, err
		}
		out[id] = repo
	}
	return out, rows.Err()
}
