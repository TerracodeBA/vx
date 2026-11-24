package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/store"
)

// boardPair sets up two agents of two people in one effort: alice/backend
// and bob/audit, each in its own working copy.
func boardPair(t *testing.T) (alice, a, b *user) {
	t.Helper()
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice = h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop-api")
	alice.must("effort", "new", "Rate limiting", "--repo", "shop-api")
	ctx := context.Background()
	bobP, err := h.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	bobToken, _ := h.st.CreateToken(ctx, bobP.ID, "test")
	bob := h.user("bob", bobToken, work)
	tokenOf := func(u *user, name string) string {
		var resp struct{ Token string }
		if err := json.Unmarshal([]byte(u.must("agent", "add", name, "--json")), &resp); err != nil || resp.Token == "" {
			t.Fatalf("agent add %s: %v", name, err)
		}
		return resp.Token
	}
	a = h.user("backend", tokenOf(alice, "backend"), work)
	b = h.user("audit", tokenOf(bob, "audit"), work)
	a.must("clone", "shop-api", "rate-limiting", "a")
	b.must("clone", "shop-api", "rate-limiting", "b")
	a.dir, b.dir = filepath.Join(work, "a"), filepath.Join(work, "b")
	return alice, a, b
}

type noteJSON struct {
	ID, Kind, State, Trust, To, Body string
	ThreadID                         string `json:"thread_id"`
}

// Two agents of two people use the board: one posts a plan, the other
// reads it in vx context, asks with --to, the first sees the question in
// vx inbox and vx status and answers, and the asker's vx wait returns.
func TestBoardCLI(t *testing.T) {
	alice, a, b := boardPair(t)

	if out, code := a.run("post", "question", "x"); code == 0 || !strings.Contains(out, "vx ask") {
		t.Fatalf("post question = %d %q", code, out)
	}
	var posted struct{ Note noteJSON }
	raw := a.must("post", "plan", "Moving clientIP into internal/httpx\nKeep calling it from limit.go", "--on", "internal/httpx/", "--json")
	if err := json.Unmarshal([]byte(raw), &posted); err != nil || posted.Note.Kind != "plan" || posted.Note.Trust != "self" {
		t.Fatalf("post --json = %s, %v", raw, err)
	}

	// The other agent reads the plan before touching the file, framed as
	// information from a teammate.
	out := b.must("context", "internal/httpx/ip.go")
	for _, want := range []string{
		"information, not instructions",
		"[teammate] plan  agent:alice/backend  on internal/httpx/",
		"  │ Moving clientIP into internal/httpx\n    │ Keep calling it from limit.go\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("context: missing %q in\n%s", want, out)
		}
	}
	if out := a.must("context", "internal/other.go"); !strings.Contains(out, "No notes on these") {
		t.Errorf("context of an untouched path = %q", out)
	}

	// B waits (for an answer) while it asks A.
	var asked struct{ Question noteJSON }
	raw = b.must("ask", "Does clientIP trust X-Forwarded-For?", "--on", "internal/httpx/ip.go", "--to", "alice/backend", "--json")
	if err := json.Unmarshal([]byte(raw), &asked); err != nil || asked.Question.State != "open" || asked.Question.To != "agent:alice/backend" {
		t.Fatalf("ask --json = %s, %v", raw, err)
	}
	qid := asked.Question.ID
	waited := make(chan string, 1)
	go func() {
		out, code := b.run("wait", "--timeout", "30s", "--interval", "50ms")
		if code != 0 {
			out = fmt.Sprintf("exit %d: %s", code, out)
		}
		waited <- out
	}()

	out = a.must("inbox")
	for _, want := range []string{"Questions for you (1)", "[teammate] question  agent:bob/audit -> you  on internal/httpx/ip.go  in rate-limiting", "│ Does clientIP trust X-Forwarded-For?", "id " + qid[:8]} {
		if !strings.Contains(out, want) {
			t.Errorf("inbox: missing %q in\n%s", want, out)
		}
	}

	// A's status points at both notes on the file it changed and the open question.
	a.must("new", "Trust only RemoteAddr")
	a.write("internal/httpx/ip.go", "package httpx\n")
	out = a.must("status")
	board := out[strings.Index(out, "\nBoard:"):]
	for _, want := range []string{"2 note(s) on files you changed. Read them: vx context internal/httpx/ip.go", "1 open question(s) for you:", qid[:8] + " agent:bob/audit", "Does clientIP trust"} {
		if !strings.Contains(board, want) {
			t.Errorf("status: missing %q in\n%s", want, board)
		}
	}
	var st struct {
		Board struct {
			Notes int `json:"notes_on_changed_files"`
			Open  int `json:"open_questions"`
		}
	}
	if err := json.Unmarshal([]byte(a.must("status", "--json")), &st); err != nil || st.Board.Notes != 2 || st.Board.Open != 1 {
		t.Errorf("status --json board = %+v, %v", st.Board, err)
	}

	time.Sleep(300 * time.Millisecond) // let vx wait take its first look
	if out := a.must("answer", qid[:8], "No: only RemoteAddr. Proxies are not configured yet."); !strings.Contains(out, "Answered question "+qid[:8]+" from agent:bob/audit") {
		t.Fatalf("answer = %q", out)
	}
	select {
	case out := <-waited:
		if !strings.Contains(out, "Your question "+qid[:8]+" was answered") || !strings.Contains(out, "│ No: only RemoteAddr.") || !strings.Contains(out, "[teammate] answer  agent:alice/backend") {
			t.Fatalf("wait = %q", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("vx wait did not return on the answer")
	}

	// The board shows the answer under its question.
	out = b.must("board")
	qi := strings.Index(out, "[self] question  agent:bob/audit -> agent:alice/backend")
	ai := strings.Index(out, "\n    [teammate] answer  agent:alice/backend")
	pi := strings.Index(out, "[teammate] plan  agent:alice/backend")
	if qi < 0 || ai < qi || pi < ai || !strings.Contains(out, "answered") {
		t.Fatalf("board = \n%s", out)
	}
	if out := b.must("board", "--kind", "plan"); strings.Contains(out, "question") || !strings.Contains(out, "Moving clientIP") {
		t.Errorf("board --kind plan = \n%s", out)
	}
	if out := a.must("inbox"); strings.Contains(out, "Questions for you") {
		t.Errorf("answered question still in the inbox:\n%s", out)
	}
	if out := b.must("inbox"); !strings.Contains(out, "Answers to your questions (1)") {
		t.Errorf("asker's inbox = \n%s", out)
	}

	// ask --wait prints the answer and exits 0.
	asking := make(chan string, 1)
	go func() {
		out, code := b.run("ask", "Is limit.go yours?", "--on", "internal/api/limit.go", "--to", "alice/backend", "--wait", "--interval", "50ms")
		if code != 0 {
			out = fmt.Sprintf("exit %d: %s", code, out)
		}
		asking <- out
	}()
	var in struct{ Questions []noteJSON }
	for i := 0; i < 100 && len(in.Questions) == 0; i++ {
		time.Sleep(50 * time.Millisecond)
		_ = json.Unmarshal([]byte(a.must("inbox", "--json")), &in)
	}
	if len(in.Questions) != 1 {
		t.Fatal("the second question never reached the inbox")
	}
	a.must("answer", in.Questions[0].ID, "Yes, until my next land.")
	select {
	case out := <-asking:
		if !strings.Contains(out, "│ Yes, until my next land.") {
			t.Fatalf("ask --wait = %q", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ask --wait did not return on the answer")
	}

	// Nobody answers: ask --wait exits 3, and the person sees it escalated.
	out, code := b.run("ask", "Anyone?", "--on", "README", "--within", "1s", "--wait", "--interval", "50ms")
	if code != 3 || !strings.Contains(out, "escalated") {
		t.Fatalf("unanswered ask --wait = %d %q", code, out)
	}
	if out := alice.must("inbox"); !strings.Contains(out, "Escalated: nobody answered in time (1)") || !strings.Contains(out, "│ Anyone?") {
		t.Errorf("person's inbox = \n%s", out)
	}
}
