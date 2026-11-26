package service

// The For you page (v0.5, #37): what waits for a person, from a phone.
// It gathers, newest first: questions escalated to them or their agents,
// reviews they may decide (landing policies), failed CI and review
// comments on their shipments, and overlap and rebase notes on their
// efforts. Everything comes through functions that filter by what the
// viewer may read (GetInbox, ListReviews, ListNotes), so it shows nothing
// the board, inbox or Reviews page would not.

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ForYou item kinds.
const (
	ForYouQuestion = "question" // an escalated question, or one asked of the person
	ForYouReview   = "review"   // a change waiting for the person's approval
	ForYouCI       = "ci"       // CI failed on a shipment of the person's (or their agents') changes
	ForYouComment  = "comment"  // a review comment on a pull request from the git host
	ForYouOverlap  = "overlap"  // the person's effort collides with another
	ForYouRebase   = "rebase"   // the person's effort needs a rebase
	// ForYouAgreement: an organization the person owns needs the data
	// processing agreement accepted (dpa.go).
	ForYouAgreement = "agreement"
)

// ForYouItem is one thing on the For you page: a note or a review.
type ForYouItem struct {
	Kind      string
	At        time.Time
	Note      *Note
	Review    *Review
	Agreement *AgreementNotice
}

// ForYou is what waits for a principal, newest first, with counts.
type ForYou struct {
	Items  []ForYouItem
	Counts map[string]int
}

const (
	forYouEfforts     = 50 // owned efforts scanned for overlap and rebase notes
	forYouEffortNotes = 20
	forYouMax         = 100
)

// ForYou returns what waits for the caller across the repositories they
// can read (read). Agents get their own questions and notes; their efforts
// are their person's.
func (s *Service) ForYou(ctx context.Context, actor Actor) (ForYou, error) {
	out := ForYou{Counts: map[string]int{}}
	in, err := s.GetInbox(ctx, actor)
	if err != nil {
		return ForYou{}, err
	}
	seen := map[uuid.UUID]bool{}
	same := map[string]bool{}
	addNote := func(kind string, n Note) {
		if seen[n.ID] {
			return
		}
		seen[n.ID] = true
		// CI results and PR comments go to every agent whose change the
		// shipment carries: a person sees each once.
		if kind == ForYouCI || kind == ForYouComment {
			k := kind + "\x00" + n.EffortID.String() + "\x00" + n.Body
			if same[k] {
				return
			}
			same[k] = true
		}
		out.Items = append(out.Items, ForYouItem{Kind: kind, At: noteTime(n), Note: &n})
	}
	for _, n := range in.Escalated {
		addNote(ForYouQuestion, n)
	}
	for _, n := range in.Questions {
		// Questions to the person themselves, and their agents' questions
		// once overdue; open questions to an agent are the agent's to answer.
		if n.State == NoteEscalated || (n.To != nil && n.To.ID == actor.ID) {
			addNote(ForYouQuestion, n)
		}
	}
	for _, n := range in.Notes {
		switch {
		case n.Kind == NoteCI && CIFailedNote(n):
			addNote(ForYouCI, n)
		case n.Kind == NoteReview:
			addNote(ForYouComment, n)
		}
	}
	pending, err := s.ListReviews(ctx, actor, ReviewQuery{})
	if err != nil {
		return ForYou{}, err
	}
	for _, rv := range pending {
		if rv.CanDecide {
			out.Items = append(out.Items, ForYouItem{Kind: ForYouReview, At: rv.CreatedAt, Review: &rv})
		}
	}
	if err := s.forYouEffortNotes(ctx, actor, addNote); err != nil {
		return ForYou{}, err
	}
	notices, err := s.AgreementNotices(ctx, actor)
	if err != nil {
		return ForYou{}, err
	}
	for i := range notices {
		out.Items = append(out.Items, ForYouItem{Kind: ForYouAgreement, At: notices[i].At, Agreement: &notices[i]})
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].At.After(out.Items[j].At) })
	if len(out.Items) > forYouMax {
		out.Items = out.Items[:forYouMax]
	}
	for _, it := range out.Items {
		out.Counts[it.Kind]++
	}
	return out, nil
}

// forYouEffortNotes adds the current overlap and needs-rebase notes of the
// open efforts the caller (a person) or their person (an agent) owns.
func (s *Service) forYouEffortNotes(ctx context.Context, actor Actor, add func(string, Note)) error {
	owner := actor.ID
	if actor.Parent != nil {
		owner = actor.Parent.ID
	}
	readable, err := s.readableFilter(ctx, actor)
	if err != nil {
		return err
	}
	effs, err := s.Store.OpenEffortsOwnedBy(ctx, []uuid.UUID{owner}, forYouEfforts)
	if err != nil {
		return err
	}
	names := map[uuid.UUID]string{}
	for _, e := range effs {
		if !readable(e.RepoID) {
			continue
		}
		name, ok := names[e.RepoID]
		if !ok {
			repo, err := s.Store.RepoByID(ctx, e.RepoID)
			if err != nil {
				return err
			}
			name, names[e.RepoID] = repo.Name, repo.Name
		}
		notes, err := s.ListNotes(ctx, actor, ListNotesInput{Repo: name, Effort: e.Slug, Kinds: []NoteKind{NoteOverlap, NoteRebase}, Limit: forYouEffortNotes})
		if err != nil {
			if isHidden(err) {
				continue
			}
			return err
		}
		for _, n := range notes {
			switch {
			case n.Kind == NoteOverlap && !OverlapResolvedNote(n):
				add(ForYouOverlap, n)
			case n.Kind == NoteRebase && RebaseNeededNote(n):
				add(ForYouRebase, n)
			}
		}
	}
	return nil
}

// noteTime is when a note asks for attention: an escalated question from
// its deadline, anything else from when it was posted.
func noteTime(n Note) time.Time {
	if n.State == NoteEscalated && n.AnswerBy != nil && n.AnswerBy.After(n.CreatedAt) {
		return *n.AnswerBy
	}
	return n.CreatedAt
}

// CIFailedNote reports whether a CI note says the checks failed (see
// ApplyCI for the text).
func CIFailedNote(n Note) bool { return n.Kind == NoteCI && strings.HasPrefix(n.Body, "CI failed") }

// OverlapResolvedNote reports whether an overlap note says the collision
// is gone (see overlapNoteText).
func OverlapResolvedNote(n Note) bool {
	return n.Kind == NoteOverlap && strings.HasPrefix(n.Body, "Resolved:")
}

// RebaseNeededNote reports whether a rebase note asks for a rebase: a
// conflict, or the base moved under a notify-only policy (see
// autoRebaseEffort for the texts). "Rebased automatically" needs nothing.
func RebaseNeededNote(n Note) bool {
	return n.Kind == NoteRebase && !strings.HasPrefix(n.Body, "Rebased automatically")
}
