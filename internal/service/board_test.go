package service_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// boardEnv is env plus a second person with an agent, a second agent of
// alice's and a clock the test moves.
type boardEnv struct {
	*env
	repo   store.Repo
	effort store.Effort
	tests  service.Actor // agent:alice/tests
	bob    service.Actor // user:bob
	bobBE  service.Actor // agent:bob/backend
	now    time.Time
}

func newBoardEnv(t *testing.T) *boardEnv {
	t.Helper()
	e := newEnv(t)
	repo, eff := e.setup()
	b := &boardEnv{env: e, repo: repo, effort: eff, now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	e.svc.Now = func() time.Time { return b.now }
	actor := func(p store.Principal, err error) service.Actor {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.svc.ResolveActor(e.ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	b.tests = actor(e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/tests", ParentID: &e.alice.ID}))
	b.bob = actor(e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"}))
	b.bobBE = actor(e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindAgent, Handle: "bob/backend", ParentID: &b.bob.ID}))
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, "shop-api", "bob", service.RoleWrite); err != nil { // shop-api is in alice's personal org
		t.Fatal(err)
	}
	return b
}

func (b *boardEnv) post(actor service.Actor, kind service.NoteKind, body string, targets ...string) service.Note {
	b.t.Helper()
	n, err := b.svc.PostNote(b.ctx, actor, service.PostNoteInput{Repo: "shop-api", Effort: b.effort.Slug, Kind: kind, Targets: targets, Body: body})
	if err != nil {
		b.t.Fatal(err)
	}
	return n
}

func (b *boardEnv) ask(actor service.Actor, to, body string, within time.Duration, targets ...string) service.Note {
	b.t.Helper()
	n, err := b.svc.Ask(b.ctx, actor, service.AskInput{Repo: "shop-api", Effort: b.effort.Slug, To: to, Body: body, AnswerWithin: within, Targets: targets})
	if err != nil {
		b.t.Fatal(err)
	}
	return n
}

// land lands files in a new workspace of actor's.
func (b *boardEnv) land(actor service.Actor, intent string, files map[string]*string) {
	b.t.Helper()
	ws, err := b.svc.OpenWorkspace(b.ctx, actor, "shop-api", b.effort.Slug, intent)
	if err != nil {
		b.t.Fatal(err)
	}
	tree := b.snapshot(b.repo, ws.Workspace.Base, files)
	res, err := b.svc.Land(b.ctx, actor, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
	if err != nil || !res.Landed {
		b.t.Fatalf("land %s: %+v %v", intent, res, err)
	}
}

func wantCode(t *testing.T, err error, code service.Code, contains string) {
	t.Helper()
	var se *service.Error
	if !errors.As(err, &se) || se.Code != code || !strings.Contains(err.Error(), contains) {
		t.Fatalf("err = %v, want code %d containing %q", err, code, contains)
	}
}

func ids(notes []service.Note) []string {
	var out []string
	for _, n := range notes {
		out = append(out, n.Body)
	}
	return out
}

func TestBoardPostNoteValidates(t *testing.T) {
	b := newBoardEnv(t)
	n := b.post(b.agent, service.NotePlan, "  limit\x00 logins\r\n\tper ip‮  ", "internal/http/", " auth.Login ", "internal/http/")
	if n.Body != "limit logins\n\tper ip" {
		t.Fatalf("body = %q", n.Body)
	}
	if strings.Join(n.Targets, ",") != "internal/http/,auth.Login" {
		t.Fatalf("targets = %q", n.Targets)
	}
	if n.ThreadID != n.ID || n.State != service.NoteOpen || n.AnswerBy != nil || !n.CreatedAt.Equal(b.now) ||
		n.RepoName != "shop-api" || n.EffortSlug != b.effort.Slug || n.Author.ID != b.agent.ID ||
		n.AuthorOwner == nil || n.AuthorOwner.ID != b.alice.ID || n.To != nil {
		t.Fatalf("note = %+v", n)
	}
	// Exactly 2000 characters after trimming is fine.
	b.post(b.alice, service.NoteDecision, " "+strings.Repeat("é", service.MaxNoteBody)+" ")

	in := func(kind service.NoteKind, body string, targets ...string) error {
		_, err := b.svc.PostNote(b.ctx, b.agent, service.PostNoteInput{Repo: "shop-api", Effort: b.effort.Slug, Kind: kind, Body: body, Targets: targets})
		return err
	}
	many := make([]string, service.MaxNoteTargets+1)
	for i := range many {
		many[i] = fmt.Sprintf("f%d.go", i)
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{in(service.NoteQuestion, "x"), "Ask"},
		{in(service.NoteAnswer, "x"), "AnswerNote"},
		{in("rumor", "x"), "not one of"},
		{in(service.NotePlan, " \x01\n "), "empty"},
		{in(service.NotePlan, strings.Repeat("x", service.MaxNoteBody+1)), "2001 characters"},
		{in(service.NotePlan, "x", many...), "21 targets"},
		{in(service.NotePlan, "x", "a.go", " "), "target 2 is empty"},
		{in(service.NotePlan, "x", "a b.go"), "whitespace"},
		{in(service.NotePlan, "x", strings.Repeat("a", service.MaxNoteTarget+1)), "keep targets to 300"},
	} {
		wantCode(t, tc.err, service.CodeInvalid, tc.want)
	}
	_, err := b.svc.PostNote(b.ctx, b.agent, service.PostNoteInput{Repo: "shop-api", Effort: "nope", Kind: service.NotePlan, Body: "x"})
	wantCode(t, err, service.CodeNotFound, "no effort nope")

	ops, err := b.svc.Store.ListOps(b.ctx, b.repo.ID, 5)
	if err != nil || ops[0].Op != "note.post" || ops[0].Target == "" {
		t.Fatalf("op log = %+v, %v", ops, err)
	}
}

func TestBoardAskResolvesTo(t *testing.T) {
	b := newBoardEnv(t)
	for to, want := range map[string]string{
		"tests":             "agent:alice/tests", // agent name relative to the caller's person
		"alice/tests":       "agent:alice/tests",
		"agent:alice/tests": "agent:alice/tests",
		"user:bob":          "user:bob",
		"bob":               "user:bob", // no agent alice/bob, so the person bob
		"bob/backend":       "agent:bob/backend",
	} {
		q := b.ask(b.agent, to, "which limit?", 0)
		if q.To == nil || q.To.Ref() != want {
			t.Errorf("to %q resolved to %v, want %s", to, q.To, want)
		}
	}
	q := b.ask(b.agent, "", "anyone?", 0)
	if q.To != nil || q.Kind != service.NoteQuestion || q.State != service.NoteOpen ||
		q.AnswerBy == nil || !q.AnswerBy.Equal(b.now.Add(service.DefaultAnswerWithin)) {
		t.Fatalf("question = %+v", q)
	}
	ask := func(to string, within time.Duration) error {
		_, err := b.svc.Ask(b.ctx, b.agent, service.AskInput{Repo: "shop-api", Effort: b.effort.Slug, To: to, Body: "x", AnswerWithin: within})
		return err
	}
	wantCode(t, ask("nobody", 0), service.CodeNotFound, "no agent alice/nobody and no person nobody")
	wantCode(t, ask("user:alice/tests", 0), service.CodeNotFound, "no principal user:alice/tests")
	wantCode(t, ask("carol/x", 0), service.CodeNotFound, "no agent carol/x")
	wantCode(t, ask("team:x", 0), service.CodeInvalid, "not a principal ref")
	wantCode(t, ask("", 25*time.Hour), service.CodeInvalid, "too long")
	wantCode(t, ask("", -time.Second), service.CodeInvalid, "negative")
	if q := b.ask(b.agent, "", "a day", service.MaxAnswerWithin); !q.AnswerBy.Equal(b.now.Add(24 * time.Hour)) {
		t.Fatalf("answer_by = %v", q.AnswerBy)
	}
	ops, _ := b.svc.Store.ListOps(b.ctx, b.repo.ID, 1)
	if ops[0].Op != "note.ask" {
		t.Fatalf("op = %s", ops[0].Op)
	}
}

func TestBoardEscalationAndAnswers(t *testing.T) {
	b := newBoardEnv(t)
	q := b.ask(b.agent, "bob/backend", "5 or 10 a minute?", time.Minute, "internal/http/limit.go")
	b.now = b.now.Add(2 * time.Minute)
	got, err := b.svc.GetNote(b.ctx, b.alice, q.ID.String())
	if err != nil || got.State != service.NoteEscalated {
		t.Fatalf("state after deadline = %q, %v", got.State, err)
	}

	// Anyone may answer, by an id prefix; the answer joins the thread.
	a, q2, err := b.svc.AnswerNote(b.ctx, b.bobBE, q.ID.String()[:8], "5")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != service.NoteAnswer || a.ThreadID != q.ID || strings.Join(a.Targets, ",") != "internal/http/limit.go" ||
		a.Author.ID != b.bobBE.ID || a.State != service.NoteClosed {
		t.Fatalf("answer = %+v", a)
	}
	if q2.ID != q.ID || q2.State != service.NoteAnswered {
		t.Fatalf("question after answer = %+v", q2)
	}
	// A second answer is allowed.
	if _, _, err := b.svc.AnswerNote(b.ctx, b.tests, q.ID.String(), "or 10 on staging"); err != nil {
		t.Fatal(err)
	}
	thread, err := b.svc.ListNotes(b.ctx, b.alice, service.ListNotesInput{Repo: "shop-api", Effort: b.effort.Slug, ThreadID: q.ID})
	if err != nil || strings.Join(ids(thread), "|") != "or 10 on staging|5|5 or 10 a minute?" {
		t.Fatalf("thread = %q, %v", ids(thread), err)
	}

	plan := b.post(b.alice, service.NotePlan, "x")
	_, _, err = b.svc.AnswerNote(b.ctx, b.bob, plan.ID.String(), "y")
	wantCode(t, err, service.CodeInvalid, "is a plan, not a question")
	_, _, err = b.svc.AnswerNote(b.ctx, b.bob, "abc", "y")
	wantCode(t, err, service.CodeInvalid, "too short")
	_, _, err = b.svc.AnswerNote(b.ctx, b.bob, "ffffffff", "y")
	wantCode(t, err, service.CodeNotFound, "no note")
	_, _, err = b.svc.AnswerNote(b.ctx, b.bob, q.ID.String(), " ")
	wantCode(t, err, service.CodeInvalid, "empty")

	ops, _ := b.svc.Store.ListOps(b.ctx, b.repo.ID, 3)
	if ops[1].Op != "note.answer" {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestBoardListNotesIsOpenToEveryone(t *testing.T) {
	b := newBoardEnv(t)
	mine := b.post(b.agent, service.NotePlan, "by claude", "internal/http/")
	b.post(b.alice, service.NoteDecision, "by alice", "auth.Login")
	b.now = b.now.Add(time.Minute)
	b.post(b.bobBE, service.NoteHandoff, "by bob's agent", "README.md")

	// Bob neither owns the effort nor has worked in it, and reads everything.
	all, err := b.svc.ListNotes(b.ctx, b.bob, service.ListNotesInput{Repo: "shop-api", Effort: b.effort.Slug})
	if err != nil || strings.Join(ids(all), "|") != "by bob's agent|by alice|by claude" {
		t.Fatalf("notes = %q, %v", ids(all), err)
	}
	for _, tc := range []struct {
		actor service.Actor
		want  string
	}{{b.agent, service.TrustSelf}, {b.alice, service.TrustSamePerson}, {b.tests, service.TrustSamePerson}, {b.bob, service.TrustTeammate}, {b.bobBE, service.TrustTeammate}} {
		if got := service.TrustOf(tc.actor, mine); got != tc.want {
			t.Errorf("TrustOf(%s) = %s, want %s", tc.actor.Ref(), got, tc.want)
		}
	}

	list := func(in service.ListNotesInput) string {
		t.Helper()
		in.Repo, in.Effort = "shop-api", b.effort.Slug
		notes, err := b.svc.ListNotes(b.ctx, b.bobBE, in)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(ids(notes), "|")
	}
	if got := list(service.ListNotesInput{Targets: []string{"internal/http/limit.go", "auth"}}); got != "by alice|by claude" {
		t.Fatalf("targets = %q", got)
	}
	if got := list(service.ListNotesInput{Kinds: []service.NoteKind{service.NoteHandoff, service.NotePlan}}); got != "by bob's agent|by claude" {
		t.Fatalf("kinds = %q", got)
	}
	if got := list(service.ListNotesInput{Since: b.now}); got != "by bob's agent" {
		t.Fatalf("since = %q", got)
	}
	if got := list(service.ListNotesInput{Limit: 1}); got != "by bob's agent" {
		t.Fatalf("limit = %q", got)
	}
	_, err = b.svc.ListNotes(b.ctx, b.bob, service.ListNotesInput{Repo: "shop-api", Effort: b.effort.Slug, Kinds: []service.NoteKind{"rumor"}})
	wantCode(t, err, service.CodeInvalid, "not one of")

	// Target filtering pages through more notes than one query reads.
	for i := range 600 {
		if _, err := b.svc.Store.InsertNote(b.ctx, store.Note{EffortID: b.effort.ID, Kind: "plan", Targets: []string{"other.go"}, AuthorID: b.bob.ID, Body: fmt.Sprint(i), CreatedAt: b.now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := list(service.ListNotesInput{Targets: []string{"internal/http"}}); got != "by claude" {
		t.Fatalf("targets after many notes = %q", got)
	}
	if got := list(service.ListNotesInput{Targets: []string{"other.go"}, Limit: 550}); strings.Count(got, "|") != 549 {
		t.Fatalf("got %d matches, want 550", strings.Count(got, "|")+1)
	}
}

func TestBoardInbox(t *testing.T) {
	b := newBoardEnv(t)
	toAgent := b.ask(b.agent, "bob/backend", "to bob's agent", time.Minute)
	toBob := b.ask(b.tests, "user:bob", "to bob", time.Hour)
	b.ask(b.bobBE, "", "to anyone", time.Minute) // in alice's effort: escalates to alice

	inbox := func(a service.Actor) service.Inbox {
		t.Helper()
		in, err := b.svc.GetInbox(b.ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	if in := inbox(b.bobBE); strings.Join(ids(in.Questions), "|") != "to bob's agent" || len(in.Escalated) != 0 {
		t.Fatalf("bob/backend inbox = %+v", in)
	}
	if in := inbox(b.bob); strings.Join(ids(in.Questions), "|") != "to bob|to bob's agent" || len(in.Escalated) != 0 {
		t.Fatalf("bob inbox = %q, escalated %d", ids(in.Questions), len(in.Escalated))
	}

	b.now = b.now.Add(2 * time.Minute)
	in := inbox(b.bob)
	if strings.Join(ids(in.Escalated), "|") != "to bob's agent" || in.Questions[1].State != service.NoteEscalated {
		t.Fatalf("bob escalated = %q", ids(in.Escalated))
	}
	if in := inbox(b.alice); strings.Join(ids(in.Escalated), "|") != "to anyone|to bob's agent" {
		t.Fatalf("alice (effort owner) escalated = %q", ids(in.Escalated))
	}
	if in := inbox(b.bobBE); len(in.Escalated) != 0 || in.Questions[0].State != service.NoteEscalated {
		t.Fatalf("agents get no escalated list: %+v", in)
	}

	if _, _, err := b.svc.AnswerNote(b.ctx, b.bobBE, toAgent.ID.String(), "done"); err != nil {
		t.Fatal(err)
	}
	if in := inbox(b.bob); strings.Join(ids(in.Questions), "|") != "to bob" || len(in.Escalated) != 0 {
		t.Fatalf("bob after answer = %q, escalated %q", ids(in.Questions), ids(in.Escalated))
	}
	if in := inbox(b.agent); strings.Join(ids(in.Answers), "|") != "done" || in.Answers[0].ThreadID != toAgent.ID {
		t.Fatalf("asker's answers = %q", ids(in.Answers))
	}
	// Answers to an agent's questions are the agent's, not its person's.
	if in := inbox(b.alice); len(in.Answers) != 0 {
		t.Fatalf("alice answers = %q", ids(in.Answers))
	}
	b.now = b.now.Add(8 * 24 * time.Hour)
	if in := inbox(b.agent); len(in.Answers) != 0 {
		t.Fatalf("week-old answers still in the inbox: %q", ids(in.Answers))
	}
	_ = toBob
}

func TestBoardContext(t *testing.T) {
	b := newBoardEnv(t)
	limit := "package http\n"
	other := "x\n"
	b.land(b.agent, "Add limiter", map[string]*string{"internal/http/limit.go": &limit})
	b.land(b.tests, "Touch other", map[string]*string{"other.go": &other})
	open, err := b.svc.OpenWorkspace(b.ctx, b.bobBE, "shop-api", b.effort.Slug, "Tune the limiter")
	if err != nil {
		t.Fatal(err)
	}

	b.post(b.agent, service.NoteInterface, "dir note", "internal/http/")
	b.post(b.alice, service.NoteDecision, "symbol note", "auth.Login")
	b.post(b.tests, service.NotePlan, "unrelated", "README.md")

	c, err := b.svc.Context(b.ctx, b.bob, "shop-api", b.effort.Slug, []string{"internal/http/limit.go", "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids(c.Notes), "|") != "symbol note|dir note" {
		t.Fatalf("notes = %q", ids(c.Notes))
	}
	if len(c.Working) != 1 || c.Working[0].Workspace.ID != open.Workspace.ID || c.Working[0].Owner.Ref() != "agent:bob/backend" {
		t.Fatalf("working = %+v", c.Working)
	}
	// The symbol "auth" does not match the file auth.go.
	if len(c.RecentChanges) != 1 || c.RecentChanges[0].Intent != "Add limiter" ||
		len(c.RecentChanges[0].Files) != 1 || c.RecentChanges[0].Files[0].Path != "internal/http/limit.go" {
		t.Fatalf("recent = %+v", c.RecentChanges)
	}

	// File targets match the changes that touched them, newest first.
	c, err = b.svc.Context(b.ctx, b.bob, "shop-api", b.effort.Slug, []string{"auth.go", "other.go"})
	if err != nil || len(c.RecentChanges) != 2 || c.RecentChanges[0].Intent != "Touch other" || c.RecentChanges[1].Intent != "Add auth" {
		t.Fatalf("recent = %d, %v", len(c.RecentChanges), err)
	}
	// Without targets: the latest of everything.
	c, err = b.svc.Context(b.ctx, b.bob, "shop-api", b.effort.Slug, nil)
	if err != nil || len(c.Notes) != 3 || len(c.RecentChanges) != 3 {
		t.Fatalf("everything = %d notes, %d changes, %v", len(c.Notes), len(c.RecentChanges), err)
	}
	_, err = b.svc.Context(b.ctx, b.bob, "shop-api", b.effort.Slug, []string{"a b"})
	wantCode(t, err, service.CodeInvalid, "whitespace")
}

// Notes vx itself posted read as trust "vx" for everyone, not "teammate":
// overlaps, rebases, review comments and CI results are neither a
// teammate's words nor the caller's.
func TestTrustOfSystemNoteIsVx(t *testing.T) {
	b := newBoardEnv(t)
	n := b.post(b.agent, service.NotePlan, "by claude", "internal/http/")
	n.System = true
	for _, actor := range []service.Actor{b.agent, b.alice, b.bob} {
		if got := service.TrustOf(actor, n); got != service.TrustVx || service.TrustVx != "vx" {
			t.Errorf("TrustOf(%s, system note) = %q, want vx", actor.Ref(), got)
		}
	}
}
