package web

import (
	"reflect"
	"testing"

	"github.com/TerracodeBA/vx/internal/merge"
)

func TestWordSegs(t *testing.T) {
	a, b := wordSegs("func Login() {", "func Login(ctx Context) {")
	if len(a) != 2 || len(b) != 3 || !b[1].Hi || b[1].Text != "ctx Context" {
		t.Fatalf("segs = %+v / %+v", a, b)
	}
	if a, b := wordSegs("abc", "xyz"); a != nil || b != nil {
		t.Fatal("lines with nothing in common are not highlighted")
	}
}

func TestSplitRowsPairsChanges(t *testing.T) {
	old := merge.SplitLines([]byte("a\nb\nc\n"))
	cur := merge.SplitLines([]byte("a\nB\nB2\nc\n"))
	lines := renderHunks(merge.Diff(old, cur), old, cur, 3)
	markWords(lines)
	rows := splitRows(lines)
	var kinds []string
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"ctx", "chg", "chg", "ctx"}) {
		t.Fatalf("rows = %v", kinds)
	}
	if rows[1].Left == nil || rows[1].Right == nil || rows[2].Left != nil || rows[2].Right == nil {
		t.Fatal("deletions and additions should pair row by row")
	}
}

func TestDiffBlocks(t *testing.T) {
	for _, tc := range []struct {
		a, d int
		want string
	}{{10, 0, "aaaaa"}, {0, 10, "ddddd"}, {5, 5, "aaadd"}, {100, 1, "aaaad"}, {0, 0, "nnnnn"}} {
		got := ""
		for _, b := range diffBlocks(tc.a, tc.d) {
			got += b
		}
		if got != tc.want {
			t.Errorf("blocks(%d, %d) = %s, want %s", tc.a, tc.d, got, tc.want)
		}
	}
}

func TestColorLinesUsesWholeFile(t *testing.T) {
	// Line 4 only reads as a comment because of the "/*" two lines above it,
	// which the diff shows as context.
	a := []byte("package p\n\n/* one\ntwo */\nvar x = 1\n")
	b := []byte("package p\n\n/* one\nthree */\nvar x = 2\n")
	la, lb := merge.SplitLines(a), merge.SplitLines(b)
	lines := renderHunks(merge.Diff(la, lb), la, lb, 3)
	markWords(lines)
	colorLines(lines, "p.go", a, b)
	var del, add *diffLine
	for i := range lines {
		switch {
		case lines[i].Kind == "del" && lines[i].Old == 4:
			del = &lines[i]
		case lines[i].Kind == "add" && lines[i].New == 4:
			add = &lines[i]
		}
	}
	if del == nil || add == nil {
		t.Fatalf("lines = %+v", lines)
	}
	for _, l := range []*diffLine{del, add} {
		hi := false
		for _, s := range l.Segs {
			if s.Class != "tk-c" {
				t.Fatalf("line %q: span %+v is not a comment", l.Text, s)
			}
			hi = hi || s.Hi
		}
		if !hi {
			t.Fatalf("line %q lost its word highlight: %+v", l.Text, l.Segs)
		}
	}
}
