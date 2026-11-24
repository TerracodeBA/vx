package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

func TestNotesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	bob, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	agent, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "bob/backend", ParentID: &bob.ID})
	repo, err := s.CreateRepo(ctx, store.Repo{Name: "shop-api"})
	if err != nil {
		t.Fatal(err)
	}
	base := hash.Of(hash.KindTree, []byte("base"))
	eff, err := s.CreateEffort(ctx, store.Effort{RepoID: repo.ID, Name: "Rate limiting", BaseBranch: "main", Base: base, OwnerID: bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	plan, err := s.InsertNote(ctx, store.Note{EffortID: eff.ID, Kind: "plan", Targets: []string{"internal/http/"}, AuthorID: agent.ID, Body: "limit logins", CreatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ThreadID != plan.ID || plan.RepoID != repo.ID || plan.RepoName != "shop-api" || plan.EffortSlug != eff.Slug ||
		plan.State != "open" || !plan.CreatedAt.Equal(t0) || len(plan.Targets) != 1 || plan.Targets[0] != "internal/http/" {
		t.Fatalf("plan = %+v", plan)
	}
	deadline := t0.Add(5 * time.Minute)
	q, err := s.InsertNote(ctx, store.Note{EffortID: eff.ID, Kind: "question", AuthorID: agent.ID, ToID: &bob.ID, Body: "which limit?", AnswerBy: &deadline, CreatedAt: t0.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if q.ToID == nil || *q.ToID != bob.ID || q.AnswerBy == nil || !q.AnswerBy.Equal(deadline) || len(q.Targets) != 0 {
		t.Fatalf("question = %+v", q)
	}

	// The schema enforces the kinds, the body length and deadlines on questions only.
	for name, bad := range map[string]store.Note{
		"kind":     {EffortID: eff.ID, Kind: "rumor", AuthorID: bob.ID, Body: "x"},
		"empty":    {EffortID: eff.ID, Kind: "plan", AuthorID: bob.ID, Body: ""},
		"long":     {EffortID: eff.ID, Kind: "plan", AuthorID: bob.ID, Body: strings.Repeat("x", 2001)},
		"deadline": {EffortID: eff.ID, Kind: "question", AuthorID: bob.ID, Body: "x"},
	} {
		if _, err := s.InsertNote(ctx, bad); err == nil {
			t.Errorf("%s: insert accepted", name)
		}
	}
	if _, err := s.InsertNote(ctx, store.Note{EffortID: uuid.New(), Kind: "plan", AuthorID: bob.ID, Body: "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown effort err = %v", err)
	}

	answer, err := s.InsertNote(ctx, store.Note{EffortID: eff.ID, ThreadID: q.ID, Kind: "answer", AuthorID: bob.ID, Body: "5 a minute", State: "closed", CreatedAt: t0.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNoteAnswered(ctx, q.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.NoteByID(ctx, q.ID); err != nil || got.State != "answered" {
		t.Fatalf("question after answer = %+v, %v", got, err)
	}
	if got, err := s.NoteByPrefix(ctx, answer.ID.String()[:8]); err != nil || got.ID != answer.ID {
		t.Fatalf("NoteByPrefix = %v, %v", got.ID, err)
	}
	if _, err := s.NoteByPrefix(ctx, "%"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wildcard prefix err = %v", err)
	}
	if _, err := s.NoteByPrefix(ctx, ""); !errors.Is(err, store.ErrAmbiguous) {
		t.Fatalf("empty prefix err = %v", err)
	}

	all, err := s.ListNotes(ctx, store.NoteFilter{EffortID: eff.ID})
	if err != nil || len(all) != 3 || all[0].ID != answer.ID || all[2].ID != plan.ID {
		t.Fatalf("ListNotes = %d, %v", len(all), err)
	}
	if got, _ := s.ListNotes(ctx, store.NoteFilter{EffortID: eff.ID, Kinds: []string{"plan", "answer"}}); len(got) != 2 {
		t.Fatalf("kinds filter = %d", len(got))
	}
	if got, _ := s.ListNotes(ctx, store.NoteFilter{EffortID: eff.ID, ThreadID: q.ID}); len(got) != 2 {
		t.Fatalf("thread filter = %d", len(got))
	}
	if got, _ := s.ListNotes(ctx, store.NoteFilter{EffortID: eff.ID, Since: t0.Add(time.Second)}); len(got) != 2 {
		t.Fatalf("since filter = %d", len(got))
	}
	page, _ := s.ListNotes(ctx, store.NoteFilter{EffortID: eff.ID, Limit: 2})
	rest, _ := s.ListNotes(ctx, store.NoteFilter{EffortID: eff.ID, Before: page[1].Cursor()})
	if len(page) != 2 || len(rest) != 1 || rest[0].ID != plan.ID {
		t.Fatalf("paging = %d + %d", len(page), len(rest))
	}

	// Inbox queries.
	q2, _ := s.InsertNote(ctx, store.Note{EffortID: eff.ID, Kind: "question", AuthorID: bob.ID, ToID: &agent.ID, Body: "done yet?", AnswerBy: &deadline, CreatedAt: t0})
	if got, err := s.OpenQuestionsTo(ctx, []uuid.UUID{bob.ID, agent.ID}, 10); err != nil || len(got) != 1 || got[0].ID != q2.ID {
		t.Fatalf("OpenQuestionsTo = %d, %v", len(got), err)
	}
	if got, err := s.AnswersToQuestionsBy(ctx, agent.ID, t0, 10); err != nil || len(got) != 1 || got[0].ID != answer.ID {
		t.Fatalf("AnswersToQuestionsBy = %d, %v", len(got), err)
	}
	if got, _ := s.AnswersToQuestionsBy(ctx, agent.ID, t0.Add(time.Hour), 10); len(got) != 0 {
		t.Fatalf("old answers returned: %d", len(got))
	}
	if got, _ := s.OverdueQuestions(ctx, bob.ID, nil, deadline.Add(-time.Second), 10); len(got) != 0 {
		t.Fatalf("overdue before deadline = %d", len(got))
	}
	// Bob owns the effort, so its overdue questions reach him.
	if got, _ := s.OverdueQuestions(ctx, bob.ID, nil, deadline.Add(time.Second), 10); len(got) != 1 || got[0].ID != q2.ID {
		t.Fatalf("overdue for the owner = %d", len(got))
	}
	if got, _ := s.OverdueQuestions(ctx, uuid.New(), []uuid.UUID{agent.ID}, deadline.Add(time.Second), 10); len(got) != 1 {
		t.Fatalf("overdue to the agent = %d", len(got))
	}
}
