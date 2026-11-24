package mcp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// vx_context returns others' notes cut to 80 characters, marked truncated,
// but questions to the caller and notes of the caller's own person whole;
// full: true and vx_board with ids return the whole note.
func TestMCPContextCutsOthersNotes(t *testing.T) {
	f := newFixture(t)
	_, eff, _ := f.seed()
	alice, aliceAgent, bobAgent := f.connect("alice"), f.connect("alice/claude"), f.connect("bob/claude")
	with := func(kv ...any) map[string]any {
		m := map[string]any{"repo": "shop-api", "effort": eff.Slug}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	long := "Limits are per user id, not per IP: " + strings.Repeat("the proxy rewrites addresses ", 6) + "END"
	bobs := call[noteOutput](t, bobAgent, "vx_post_note", with("kind", "decision", "targets", []string{"a.go"}, "body", long)).Note
	call[noteOutput](t, alice, "vx_post_note", with("kind", "plan", "targets", []string{"a.go"}, "body", "Alice: "+long))
	q := call[noteOutput](t, bobAgent, "vx_ask", with("targets", []string{"a.go"}, "body", "Bob asks: "+long, "to", "alice/claude")).Note

	cx := call[contextOutput](t, aliceAgent, "vx_context", with("targets", []string{"a.go"}))
	if len(cx.Notes) != 3 {
		t.Fatalf("context notes = %+v", cx.Notes)
	}
	for _, n := range cx.Notes {
		switch {
		case n.ID == bobs.ID:
			if !n.Truncated || utf8.RuneCountInString(n.Untrusted) != 80 || !strings.HasSuffix(n.Untrusted, "…") || !strings.HasPrefix(long, strings.TrimSuffix(n.Untrusted, "…")) {
				t.Errorf("bob's decision is not cut: %+v", n)
			}
		case n.ID == q.ID: // a question to the caller
			if n.Truncated || n.Untrusted != "Bob asks: "+long {
				t.Errorf("the question to alice/claude was cut: %+v", n)
			}
		default: // alice's own person
			if n.Truncated || n.Untrusted != "Alice: "+long || n.Trust != "same_person" {
				t.Errorf("alice's plan was cut: %+v", n)
			}
		}
	}
	if cx := call[contextOutput](t, aliceAgent, "vx_context", with("targets", []string{"a.go"}, "full", true)); len(cx.Notes) != 3 {
		t.Fatalf("context full = %+v", cx.Notes)
	} else {
		for _, n := range cx.Notes {
			if n.Truncated || !strings.HasSuffix(n.Untrusted, "END") {
				t.Errorf("full: true still cuts %+v", n)
			}
		}
	}
	ns := call[notesOutput](t, aliceAgent, "vx_board", with("ids", []string{bobs.ID[:8]})).Notes
	if len(ns) != 1 || ns[0].ID != bobs.ID || ns[0].Untrusted != long || ns[0].Truncated {
		t.Fatalf("vx_board ids = %+v", ns)
	}
	// A question comes with its answers.
	call[answerOutput](t, aliceAgent, "vx_answer", map[string]any{"note_id": q.ID, "body": "Per user id."})
	if ns := call[notesOutput](t, bobAgent, "vx_board", with("ids", []string{q.ID})).Notes; len(ns) != 2 || ns[0].Kind != "answer" || ns[1].ID != q.ID {
		t.Fatalf("vx_board ids of a question = %+v", ns)
	}
	if msg := callErr(t, bobAgent, "vx_board", with("ids", []string{"0000000000"})); !strings.Contains(msg, "no note 0000000000") {
		t.Fatalf("unknown id = %q", msg)
	}
}
