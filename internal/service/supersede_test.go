package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func TestNoteSupersedes(t *testing.T) {
	b := newBoardEnv(t)
	old := b.post(b.agent, service.NoteDecision, "limits per ip", "limit.Allow")
	b.now = b.now.Add(time.Minute)
	supersede := func(actor service.Actor, id, body string) (service.Note, error) {
		return b.svc.PostNote(b.ctx, actor, service.PostNoteInput{Repo: "shop-api", Effort: b.effort.Slug, Kind: service.NoteDecision,
			Targets: []string{"limit.Allow"}, Body: body, Supersedes: id})
	}

	// Someone else's note cannot be replaced, nor a question.
	_, err := supersede(b.bobBE, old.ID.String(), "per user")
	wantCode(t, err, service.CodePermission, "only replace your own notes")
	q := b.ask(b.bobBE, "", "per ip?", 0)
	_, err = supersede(b.bobBE, q.ID.String(), "per user")
	wantCode(t, err, service.CodeInvalid, "only plan, decision")
	_, err = supersede(b.agent, "zz", "per user")
	wantCode(t, err, service.CodeInvalid, "too short")

	// Another agent of the same person replaces it, by prefix.
	n, err := supersede(b.tests, old.ID.String()[:8], "limits per user id")
	if err != nil {
		t.Fatal(err)
	}
	if n.Supersedes == nil || *n.Supersedes != old.ID || n.SupersededBy != nil {
		t.Fatalf("new note = %+v", n)
	}
	got, err := b.svc.GetNote(b.ctx, b.bob, old.ID.String())
	if err != nil || got.SupersededBy == nil || *got.SupersededBy != n.ID {
		t.Fatalf("old note = %+v, %v", got, err)
	}
	// A replaced note cannot be replaced again: the newer one can.
	_, err = supersede(b.agent, old.ID.String(), "again")
	wantCode(t, err, service.CodeInvalid, "already replaced by "+n.ID.String()[:8])

	// Lists and context leave the old note out unless asked for it.
	list := func(all bool) string {
		t.Helper()
		notes, err := b.svc.ListNotes(b.ctx, b.bob, service.ListNotesInput{Repo: "shop-api", Effort: b.effort.Slug, IncludeSuperseded: all,
			Kinds: []service.NoteKind{service.NoteDecision}})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(ids(notes), "|")
	}
	if got := list(false); got != "limits per user id" {
		t.Fatalf("board = %q", got)
	}
	if got := list(true); got != "limits per user id|limits per ip" {
		t.Fatalf("board --all = %q", got)
	}
	nc, err := b.svc.Context(b.ctx, b.bob, "shop-api", b.effort.Slug, []string{"limit.Allow"})
	if err != nil || strings.Join(ids(nc.Notes), "|") != "limits per user id" {
		t.Fatalf("context = %q, %v", ids(nc.Notes), err)
	}

	// Only notes of the same effort.
	other, err := b.svc.CreateEffort(b.ctx, b.alice, service.CreateEffortInput{Repo: "shop-api", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.svc.PostNote(b.ctx, b.agent, service.PostNoteInput{Repo: "shop-api", Effort: other.Slug, Kind: service.NotePlan,
		Body: "x", Supersedes: n.ID.String()})
	wantCode(t, err, service.CodeInvalid, "only replace one in its own effort")

	// System notes are stored as such and nobody replaces them through PostNote.
	vx, err := b.svc.Store.PrincipalByHandle(b.ctx, service.SystemPrincipalHandle)
	if err != nil {
		t.Fatal(err)
	}
	sys, err := b.svc.Store.InsertNote(b.ctx, store.Note{EffortID: b.effort.ID, Kind: "overlap", AuthorID: vx.ID, Body: "overlap", System: true})
	if err != nil || !sys.System {
		t.Fatalf("system note = %+v, %v", sys, err)
	}
	_, err = b.svc.PostNote(b.ctx, b.alice, service.PostNoteInput{Repo: "shop-api", Effort: b.effort.Slug, Kind: service.NotePlan, Body: "x", Supersedes: sys.ID.String()})
	if err == nil {
		t.Fatal("replaced a system note")
	}
}

func TestNoteReadState(t *testing.T) {
	b := newBoardEnv(t)
	plan := b.post(b.bobBE, service.NotePlan, "moving clientIP", "internal/http/")
	q := b.ask(b.bobBE, b.agent.Ref(), "per ip?", 0)
	read := func(actor service.Actor) map[string]bool {
		t.Helper()
		notes, err := b.svc.ListNotes(b.ctx, actor, service.ListNotesInput{Repo: "shop-api", Effort: b.effort.Slug})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, n := range notes {
			out[n.Body] = n.Read
		}
		return out
	}
	if got := read(b.agent); got["moving clientIP"] || got["per ip?"] {
		t.Fatalf("read before anything was shown: %v", got)
	}
	if err := b.svc.MarkNotesRead(b.ctx, b.agent, []uuid.UUID{plan.ID, plan.ID, uuid.New()}); err != nil {
		t.Fatal(err)
	}
	if got := read(b.agent); !got["moving clientIP"] || got["per ip?"] {
		t.Fatalf("after marking the plan: %v", got)
	}
	// Read state is per principal: alice and alice's other agent did not see it.
	if got := read(b.tests); got["moving clientIP"] {
		t.Fatalf("another agent's read state: %v", got)
	}
	// Answering a question means it was seen; the asker sees the answer unread.
	ans, question, err := b.svc.AnswerNote(b.ctx, b.agent, q.ID.String(), "per user")
	if err != nil || !question.Read || ans.Read {
		t.Fatalf("answer = %+v, question = %+v, %v", ans, question, err)
	}
	in, err := b.svc.GetInbox(b.ctx, b.bobBE)
	if err != nil || len(in.Answers) != 1 || in.Answers[0].Read {
		t.Fatalf("inbox before reading = %+v, %v", in.Answers, err)
	}
	if err := b.svc.MarkNotesRead(b.ctx, b.bobBE, []uuid.UUID{ans.ID}); err != nil {
		t.Fatal(err)
	}
	if in, err = b.svc.GetInbox(b.ctx, b.bobBE); err != nil || len(in.Answers) != 1 || !in.Answers[0].Read {
		t.Fatalf("inbox after reading = %+v, %v", in.Answers, err)
	}

	// Notes in a repository the caller cannot read are skipped silently.
	carolP, err := b.svc.Store.CreatePrincipal(b.ctx, store.Principal{Kind: store.KindHuman, Handle: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	carol, err := b.svc.ResolveActor(b.ctx, carolP)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.svc.MarkNotesRead(b.ctx, carol, []uuid.UUID{plan.ID}); err != nil {
		t.Fatal(err)
	}
	if seen, err := b.svc.Store.NotesRead(b.ctx, carol.ID, []uuid.UUID{plan.ID}); err != nil || seen[plan.ID] {
		t.Fatalf("carol marked a note she cannot read: %v, %v", seen, err)
	}
	many := make([]uuid.UUID, service.MaxMarkRead+1)
	wantCode(t, b.svc.MarkNotesRead(b.ctx, b.agent, many), service.CodeInvalid, "at most")
}

// Interface notes may be up to 4000 characters, in the service and in the
// schema; other notes stay at 2000.
func TestInterfaceNoteLength(t *testing.T) {
	b := newBoardEnv(t)
	post := func(kind service.NoteKind, n int) error {
		_, err := b.svc.PostNote(b.ctx, b.agent, service.PostNoteInput{Repo: "shop-api", Effort: b.effort.Slug, Kind: kind, Body: strings.Repeat("é", n)})
		return err
	}
	if err := post(service.NoteInterface, service.MaxInterfaceNoteBody); err != nil || service.MaxInterfaceNoteBody != 4000 {
		t.Fatalf("4000-character interface note: %v", err)
	}
	wantCode(t, post(service.NoteInterface, service.MaxInterfaceNoteBody+1), service.CodeInvalid, "keep it to 4000")
	wantCode(t, post(service.NotePlan, service.MaxNoteBody+1), service.CodeInvalid, "interface notes may have 4000")
	if _, err := b.svc.Store.InsertNote(b.ctx, store.Note{EffortID: b.effort.ID, Kind: "decision", AuthorID: b.agent.ID, Body: strings.Repeat("x", 2001)}); err == nil {
		t.Fatal("the schema took a 2001-character decision")
	}
}
