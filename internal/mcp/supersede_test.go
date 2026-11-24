package mcp

import (
	"testing"
)

// vx_post_note takes supersedes; the replaced note drops out of vx_board
// and vx_context unless asked for with all, and notes a tool showed come
// back marked read.
func TestMCPSupersedeAndRead(t *testing.T) {
	f := newFixture(t)
	_, eff, _ := f.seed()
	aliceAgent, bobAgent := f.connect("alice/claude"), f.connect("bob/claude")
	with := func(kv ...any) map[string]any {
		m := map[string]any{"repo": "shop-api", "effort": eff.Slug}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	old := call[noteOutput](t, aliceAgent, "vx_post_note", with("kind", "plan", "targets", []string{"a.go"}, "body", "first plan")).Note
	if old.Read {
		t.Fatalf("a new note reads as read: %+v", old)
	}
	ns := call[notesOutput](t, bobAgent, "vx_board", with()).Notes
	if len(ns) != 1 || ns[0].Read {
		t.Fatalf("bob's first board = %+v", ns)
	}
	if ns := call[notesOutput](t, bobAgent, "vx_board", with()).Notes; len(ns) != 1 || !ns[0].Read {
		t.Fatalf("bob's second board does not mark the note read: %+v", ns)
	}

	newer := call[noteOutput](t, aliceAgent, "vx_post_note", with("kind", "plan", "targets", []string{"a.go"}, "body", "second plan", "supersedes", old.ID[:8])).Note
	if newer.Supersedes != old.ID {
		t.Fatalf("new note = %+v", newer)
	}
	if ns := call[notesOutput](t, bobAgent, "vx_board", with()).Notes; len(ns) != 1 || ns[0].ID != newer.ID {
		t.Fatalf("board after superseding = %+v", ns)
	}
	all := call[notesOutput](t, bobAgent, "vx_board", with("all", true)).Notes
	if len(all) != 2 || all[1].ID != old.ID || all[1].SupersededBy != newer.ID {
		t.Fatalf("board with all = %+v", all)
	}
	if cx := call[contextOutput](t, bobAgent, "vx_context", with("targets", []string{"a.go"})); len(cx.Notes) != 1 || cx.Notes[0].ID != newer.ID {
		t.Fatalf("context after superseding = %+v", cx.Notes)
	}
}
