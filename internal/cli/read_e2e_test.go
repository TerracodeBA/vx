package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// vx post --supersedes replaces a note: the board shows the new one, says
// what it replaces, and lists the old one only with --all.
func TestPostSupersedes(t *testing.T) {
	_, a, b := boardPair(t)
	var posted struct{ Note noteJSON }
	if err := json.Unmarshal([]byte(a.must("post", "decision", "Limits are per IP", "--on", "limit.Allow", "--json")), &posted); err != nil {
		t.Fatal(err)
	}
	old := posted.Note.ID
	out := a.must("post", "decision", "Limits are per user id", "--on", "limit.Allow", "--supersedes", old[:8])
	if !strings.Contains(out, "replacing "+old[:8]) {
		t.Errorf("post --supersedes = %q", out)
	}
	if out := b.must("board"); strings.Contains(out, "Limits are per IP") || !strings.Contains(out, "replaces "+old[:8]) {
		t.Errorf("board shows the replaced note or not what replaced it:\n%s", out)
	}
	if out := b.must("board", "--all"); !strings.Contains(out, "Limits are per IP") || !strings.Contains(out, "replaced by ") {
		t.Errorf("board --all:\n%s", out)
	}
	if out := b.must("context", "limit.Allow"); strings.Contains(out, "Limits are per IP") {
		t.Errorf("context shows the replaced note:\n%s", out)
	}
	if out, code := b.run("post", "plan", "Mine now", "--supersedes", old); code == 0 || !strings.Contains(out, "vx: permission denied: note ") || !strings.Contains(out, "only replace your own notes") {
		t.Errorf("superseding a teammate's note = %d %q", code, out)
	}
	if out := a.must("post", "--help"); !strings.Contains(out, "symbols") || !strings.Contains(out, "pkg.Func") || !strings.Contains(out, "directories") {
		t.Errorf("post --help does not say targets can be directories and symbols:\n%s", out)
	}
}

// Notes count as read once shown: vx context, vx board and vx wait mark
// them, so the sync feed and vx status do not bring them up again, and vx
// inbox shows answers once.
func TestNotesReadOnceShown(t *testing.T) {
	_, a, b := boardPair(t)

	// Shown by vx context: not in the next sync.
	b.must("new", "Use X")
	b.write("x.go", "package x\n")
	a.must("post", "interface", "func X() int", "--on", "x.go")
	if out := b.must("status"); !strings.Contains(out, "1 unread note(s) by others on files you changed") {
		t.Errorf("status before reading:\n%s", out)
	}
	b.must("context", "x.go")
	if out := b.must("sync"); strings.Contains(out, "New on the board") {
		t.Errorf("sync repeats a note vx context showed:\n%s", out)
	}
	if out := b.must("status"); !strings.Contains(out, "No unread notes by others on files you changed") {
		t.Errorf("status after reading:\n%s", out)
	}

	// Own notes never count.
	a.must("new", "Own")
	a.write("x.go", "package x\n\nfunc X() int { return 42 }\n")
	a.must("post", "plan", "Implementing X", "--on", "x.go")
	if out := a.must("status"); strings.Contains(out, "unread note(s) by others") {
		t.Errorf("status counts the agent's own notes:\n%s", out)
	}

	// Shown by vx wait: not in the next sync.
	waited := make(chan string, 1)
	go func() {
		out, _ := b.run("wait", "--timeout", "30s", "--interval", "50ms")
		waited <- out
	}()
	time.Sleep(300 * time.Millisecond)
	a.must("post", "decision", "x.go keeps its package name", "--on", "x.go")
	select {
	case out := <-waited:
		if !strings.Contains(out, "x.go keeps its package name") {
			t.Fatalf("wait = %q", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("vx wait did not return on the decision")
	}
	if out := b.must("sync"); strings.Contains(out, "x.go keeps its package name") {
		t.Errorf("sync repeats the note that woke vx wait:\n%s", out)
	}

	// Answers: shown once by vx inbox, then counted as read.
	var asked struct{ Question noteJSON }
	if err := json.Unmarshal([]byte(b.must("ask", "Per IP?", "--to", "alice/backend", "--json")), &asked); err != nil {
		t.Fatal(err)
	}
	a.must("answer", asked.Question.ID[:8], "Per user id")
	if out := b.must("inbox"); !strings.Contains(out, "│ Per user id") {
		t.Errorf("first inbox:\n%s", out)
	}
	out := b.must("inbox")
	if strings.Contains(out, "Per user id") || !strings.Contains(out, "1 read (vx inbox --all)") {
		t.Errorf("second inbox repeats the answer:\n%s", out)
	}
	if out := b.must("inbox", "--all"); !strings.Contains(out, "[teammate] answer alice/backend in rate-limiting just now re ") || !strings.Contains(out, ": Per user id\n") {
		t.Errorf("inbox --all:\n%s", out)
	}
}
