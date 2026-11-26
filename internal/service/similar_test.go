package service_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/service"
)

// A second agent asking what is already open is told about the open
// question, and the board links the two.
func TestAskSimilarOpenQuestion(t *testing.T) {
	b := newBoardEnv(t)
	first := b.ask(b.agent, "", "Does limit.Allow take the client IP or the user id?", time.Hour, "limit.Allow")
	if first.SimilarTo != nil || service.SimilarNotice(first) != "" {
		t.Fatalf("first question linked: %+v", first)
	}

	// Same target, nearly the same words, another agent: posted, linked.
	second := b.ask(b.bobBE, "", "Does Allow take the user id or the client IP?", time.Hour, "limit.Allow")
	if second.SimilarTo == nil || *second.SimilarTo != first.ID {
		t.Fatalf("second question not linked to the first: %+v", second.SimilarTo)
	}
	want := "a similar open question exists: " + first.ID.String()[:8] + " by agent:alice/claude (vx board " + first.ID.String()[:8] + ")."
	if got := service.SimilarNotice(second); got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}

	// The board shows both directions.
	notes, err := b.svc.ListNotes(b.ctx, b.alice, service.ListNotesInput{Repo: "shop-api", Effort: b.effort.Slug})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		switch n.ID {
		case first.ID:
			if !slices.Equal(n.Similar, []uuid.UUID{second.ID}) {
				t.Errorf("first.Similar = %v", n.Similar)
			}
		case second.ID:
			if n.SimilarTo == nil || *n.SimilarTo != first.ID {
				t.Errorf("second.SimilarTo = %v", n.SimilarTo)
			}
		}
	}

	// Not similar: other words, another target, another addressee.
	for _, q := range []struct {
		to, body, target string
	}{
		{"", "Who reviews the migration for limits?", "limit.Allow"},
		{"", "Does limit.Allow take the client IP or the user id?", "auth.Login"},
		{"", "Does limit.Allow take the client IP or the user id?", ""},
	} {
		var targets []string
		if q.target != "" {
			targets = []string{q.target}
		}
		if n := b.ask(b.tests, q.to, q.body, time.Hour, targets...); n.SimilarTo != nil {
			t.Errorf("%q on %v linked to %s", q.body, targets, n.SimilarTo)
		}
	}
	byBob := b.ask(b.agent, "user:bob", "Who owns pricing?", time.Hour)
	if n := b.ask(b.tests, "agent:bob/backend", "Who owns pricing?", time.Hour); n.SimilarTo != nil {
		t.Errorf("question to another addressee linked to %s", n.SimilarTo)
	}
	// To anyone matches a question to someone.
	if n := b.ask(b.tests, "", "who owns the pricing", time.Hour); n.SimilarTo == nil || *n.SimilarTo != byBob.ID {
		t.Errorf("question to anyone not linked: %v", n.SimilarTo)
	}

	// Once the first is answered, a new question like it is not linked.
	if _, _, err := b.svc.AnswerNote(b.ctx, b.alice, first.ID.String(), "The client IP."); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.svc.AnswerNote(b.ctx, b.alice, second.ID.String(), "The client IP."); err != nil {
		t.Fatal(err)
	}
	if n := b.ask(b.bobBE, "", "Does limit.Allow take the client IP?", time.Hour, "limit.Allow"); n.SimilarTo != nil {
		t.Errorf("linked to an answered question: %s", n.SimilarTo)
	}
	// Escalated questions are still open.
	late := b.ask(b.agent, "", "Is the burst size 20 or 50?", time.Minute)
	b.now = b.now.Add(time.Hour)
	if n := b.ask(b.bobBE, "", "Burst size: 20 or 50?", time.Hour); n.SimilarTo == nil || *n.SimilarTo != late.ID {
		t.Errorf("not linked to an escalated question: %v", n.SimilarTo)
	}
	if got := service.SimilarNotice(service.Note{}); got != "" {
		t.Fatalf("notice without a similar question = %q", got)
	}
}
