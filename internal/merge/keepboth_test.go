package merge

import (
	"math/rand"
	"strings"
	"testing"
)

func TestMerge3KeepBothInsertions(t *testing.T) {
	cases := []struct {
		name, base, ours, theirs, want string
		kept                           int
	}{
		{
			name:   "AddCommand list",
			base:   "\troot.AddCommand(\n\t\tnewA(),\n\t)\n",
			ours:   "\troot.AddCommand(\n\t\tnewA(),\n\t\tnewOurs(),\n\t)\n",
			theirs: "\troot.AddCommand(\n\t\tnewA(),\n\t\tnewTheirs(),\n\t)\n",
			want:   "\troot.AddCommand(\n\t\tnewA(),\n\t\tnewTheirs(),\n\t\tnewOurs(),\n\t)\n",
			kept:   1,
		},
		{
			name:   "sections after the same heading, both ending in a blank line",
			base:   "# Doc\n\n## Old\nold\n",
			ours:   "# Doc\n\n## Ours\nours\n\n## Old\nold\n",
			theirs: "# Doc\n\n## Theirs\ntheirs\n\n## Old\nold\n",
			want:   "# Doc\n\n## Theirs\ntheirs\n\n## Ours\nours\n\n## Old\nold\n",
			kept:   1,
		},
		{
			name:   "functions at the end, both ending in a closing brace",
			base:   "package p\n\nfunc A() {\n}\n",
			ours:   "package p\n\nfunc A() {\n}\n\nfunc Ours() {\n}\n",
			theirs: "package p\n\nfunc A() {\n}\n\nfunc Theirs() {\n}\n",
			want:   "package p\n\nfunc A() {\n}\n\nfunc Theirs() {\n}\n\nfunc Ours() {\n}\n",
			kept:   1,
		},
		{
			// The closing brace is base context the diff aligns on both
			// sides: it stays once, the insertions go before it.
			name:   "shared closing line aligned with base",
			base:   "func A() {\n\tx()\n}\n",
			ours:   "func A() {\n\tx()\n\tours()\n}\n",
			theirs: "func A() {\n\tx()\n\ttheirs()\n}\n",
			want:   "func A() {\n\tx()\n\ttheirs()\n\tours()\n}\n",
			kept:   1,
		},
		{
			// Both insert a function between A and B. Each side's own closing
			// brace and blank line are kept once per side.
			name:   "functions in the middle",
			base:   "func A() {\n}\n\nfunc B() {\n}\n",
			ours:   "func A() {\n}\n\nfunc Ours() {\n}\n\nfunc B() {\n}\n",
			theirs: "func A() {\n}\n\nfunc Theirs() {\n}\n\nfunc B() {\n}\n",
			want:   "func A() {\n}\n\nfunc Theirs() {\n}\n\nfunc Ours() {\n}\n\nfunc B() {\n}\n",
			kept:   1,
		},
		{
			name:   "identical insertions once",
			base:   "a\nb\n",
			ours:   "a\nnew\nb\n",
			theirs: "a\nnew\nb\n",
			want:   "a\nnew\nb\n",
		},
		{
			name:   "end of file without a final newline",
			base:   "a\n",
			ours:   "a\nours",
			theirs: "a\ntheirs",
			want:   "a\ntheirs\nours",
			kept:   1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Merge3(lines(c.base), lines(c.ours), lines(c.theirs), Labels{})
			if res.Conflicts != 0 || res.KeptBoth != c.kept || string(res.Bytes()) != c.want {
				t.Fatalf("conflicts=%d kept=%d, got:\n%s\nwant:\n%s", res.Conflicts, res.KeptBoth, res.Bytes(), c.want)
			}
		})
	}
}

func TestMerge3NotPureInsertionsConflict(t *testing.T) {
	cases := []struct{ name, base, ours, theirs string }{
		{"same line changed", "root.AddCommand(a, b)\n", "root.AddCommand(a, b, c)\n", "root.AddCommand(a, b, d)\n"},
		{"one side changed, the other inserted next to it", "a\nb\nc\n", "a\nB\nc\n", "a\nb\nx\nc\n"},
		{"one side deleted, the other inserted before it", "a\nb\nc\n", "a\nc\n", "a\nx\nb\nc\n"},
		{"both create the file", "", "x\n", "y\n"},
		{"markers already in ours", "a\nb\n", "a\n<<<<<<< local\no\n=======\nt\n>>>>>>> effort\nb\n", "a\ny\nb\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Merge3(lines(c.base), lines(c.ours), lines(c.theirs), Labels{Ours: "ours", Theirs: "theirs"})
			if res.Conflicts == 0 || res.KeptBoth != 0 {
				t.Fatalf("conflicts=%d kept=%d, got:\n%s", res.Conflicts, res.KeptBoth, res.Bytes())
			}
		})
	}
}

// insertOnly returns base with up to four random lines inserted. Inserted
// lines often repeat base lines or each other ("}", blank lines) so the
// diff has to choose between alignments.
func insertOnly(r *rand.Rand, base []string) []string {
	pool := []string{"}\n", "\n", "{\n", "x\n", "y\n", "z\n"}
	out := append([]string(nil), base...)
	for n := r.Intn(5); n > 0; n-- {
		i := r.Intn(len(out) + 1)
		var l string
		if len(base) > 0 && r.Intn(3) == 0 {
			l = base[r.Intn(len(base))]
		} else {
			l = pool[r.Intn(len(pool))]
		}
		out = append(out[:i], append([]string{l}, out[i:]...)...)
	}
	return out
}

func counts(ls []string) map[string]int {
	m := map[string]int{}
	for _, l := range ls {
		m[l]++
	}
	return m
}

func isSubsequence(sub, of []string) bool {
	j := 0
	for _, l := range of {
		if j < len(sub) && sub[j] == l {
			j++
		}
	}
	return j == len(sub)
}

func TestMerge3KeepBothProperties(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	shapes := []string{"a\n", "}\n", "\n", "b\n", "{\n"}
	kept := 0
	for iter := 0; iter < 5000; iter++ {
		base := make([]string, 1+r.Intn(12))
		for i := range base {
			base[i] = shapes[r.Intn(len(shapes))]
		}
		ours, theirs := insertOnly(r, base), insertOnly(r, base)
		res := Merge3(base, ours, theirs, Labels{})
		kept += res.KeptBoth
		if res.Conflicts != 0 {
			t.Fatalf("pure insertions conflicted\nbase=%q\nours=%q\ntheirs=%q\ngot=%q", base, ours, theirs, res.Lines)
		}
		// Nothing either side had is lost, and no line appears more often
		// than base plus what each side added (identical insertions at the
		// same place are kept once, so fewer is fine).
		if !isSubsequence(ours, res.Lines) || !isSubsequence(theirs, res.Lines) {
			t.Fatalf("result drops lines\nbase=%q\nours=%q\ntheirs=%q\ngot=%q", base, ours, theirs, res.Lines)
		}
		co, ct, cb, cr := counts(ours), counts(theirs), counts(base), counts(res.Lines)
		for l := range merged(co, ct) {
			if most := co[l] + ct[l] - cb[l]; cr[l] < max(co[l], ct[l]) || cr[l] > most {
				t.Fatalf("line %q appears %d times, want between %d and %d\nbase=%q\nours=%q\ntheirs=%q\ngot=%q", l, cr[l], max(co[l], ct[l]), most, base, ours, theirs, res.Lines)
			}
		}
		// Identical insertions are kept once.
		if same := Merge3(base, ours, ours, Labels{}); same.Conflicts != 0 || same.KeptBoth != 0 || !equalLines(same.Lines, ours) {
			t.Fatalf("identical insertions not kept once: %q", same.Lines)
		}
		// Swapping the sides keeps the same lines, only the order changes.
		swapped := Merge3(base, theirs, ours, Labels{})
		if swapped.Conflicts != 0 || swapped.KeptBoth != res.KeptBoth || !equalCounts(counts(swapped.Lines), cr) {
			t.Fatalf("swapped sides differ\nbase=%q\nours=%q\ntheirs=%q\na=%q\nb=%q", base, ours, theirs, res.Lines, swapped.Lines)
		}
	}
	if kept < 500 {
		t.Fatalf("only %d regions kept both: the generator rarely inserts at the same place", kept)
	}
}

func TestMerge3ChangedBaseStillConflicts(t *testing.T) {
	r := rand.New(rand.NewSource(13))
	for iter := 0; iter < 2000; iter++ {
		base := make([]string, 1+r.Intn(10))
		for i := range base {
			base[i] = string(rune('a'+i)) + "\n"
		}
		// Both sides change the same base line differently, then insert
		// lines elsewhere: the changed line must still conflict.
		at := r.Intn(len(base))
		ours, theirs := append([]string(nil), base...), append([]string(nil), base...)
		ours[at], theirs[at] = "ours!\n", "theirs!\n"
		if r.Intn(2) == 0 {
			ours = append(ours[:at], ours[at+1:]...) // ours deleted it instead
		}
		res := Merge3(base, ours, theirs, Labels{Ours: "ours", Theirs: "theirs"})
		if res.Conflicts == 0 {
			t.Fatalf("changed line merged\nbase=%q\nours=%q\ntheirs=%q\ngot=%q", base, ours, theirs, res.Lines)
		}
		if !strings.Contains(string(res.Bytes()), "theirs!\n") {
			t.Fatalf("conflict lost theirs: %q", res.Lines)
		}
	}
}

func merged(a, b map[string]int) map[string]bool {
	m := map[string]bool{}
	for k := range a {
		m[k] = true
	}
	for k := range b {
		m[k] = true
	}
	return m
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
