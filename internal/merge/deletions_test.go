package merge

import (
	"fmt"
	"math/rand"
	"testing"
)

func TestMerge3AdjacentDeletions(t *testing.T) {
	cases := []struct {
		name, base, ours, theirs, want string
		conflict                       bool
	}{
		{name: "neighbouring lines", base: "a\nb\nc\nd\n", ours: "a\nc\nd\n", theirs: "a\nb\nd\n", want: "a\nd\n"},
		{
			name:   "neighbouring functions",
			base:   "func A() {\n\ta()\n}\nfunc B() {\n\tb()\n}\nfunc C() {\n}\n",
			ours:   "func B() {\n\tb()\n}\nfunc C() {\n}\n",
			theirs: "func A() {\n\ta()\n}\nfunc C() {\n}\n",
			want:   "func C() {\n}\n",
		},
		{name: "at the end of the file", base: "a\nb\nc\n", ours: "a\nc\n", theirs: "a\nb\n", want: "a\n"},
		{name: "interleaved lines", base: "a\nb\nc\nd\ne\n", ours: "a\nc\ne\n", theirs: "a\nb\nd\ne\n", want: "a\ne\n"},
		{name: "overlapping deletions", base: "a\nb\nc\nd\n", ours: "a\nd\n", theirs: "a\nb\nd\n", conflict: true},
		{name: "deletion next to an edit", base: "a\nb\nc\nd\n", ours: "a\nc\nd\n", theirs: "a\nb\nC\nd\n", conflict: true},
		{name: "deletion next to an insertion", base: "a\nb\nc\nd\n", ours: "a\nc\nd\n", theirs: "a\nb\nx\nd\n", conflict: true},
		{name: "markers already in ours", base: "a\nb\nc\n<<<<<<< x\n=======\n>>>>>>> y\n", ours: "a\nc\n<<<<<<< x\n=======\n>>>>>>> y\n", theirs: "a\nb\n<<<<<<< x\n=======\n>>>>>>> y\n", conflict: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Merge3(lines(c.base), lines(c.ours), lines(c.theirs), Labels{Ours: "ours", Theirs: "theirs"})
			if c.conflict {
				if res.Conflicts == 0 || res.BothDeleted != 0 {
					t.Fatalf("want a conflict, got conflicts=%d both=%d:\n%s", res.Conflicts, res.BothDeleted, res.Bytes())
				}
				return
			}
			if res.Conflicts != 0 || res.BothDeleted != 1 || string(res.Bytes()) != c.want {
				t.Fatalf("conflicts=%d both=%d, got:\n%s\nwant:\n%s", res.Conflicts, res.BothDeleted, res.Bytes(), c.want)
			}
		})
	}
}

// deleteBlock returns lines without the block [i, i+n).
func deleteBlock(ls []string, i, n int) []string {
	return append(append([]string(nil), ls[:i]...), ls[i+n:]...)
}

// Two sides deleting separate blocks of distinct lines, neighbouring or
// not, always merge to the base without both blocks, in either order.
func TestMerge3SeparateDeletionsProperties(t *testing.T) {
	r := rand.New(rand.NewSource(27))
	for iter := 0; iter < 3000; iter++ {
		size := 2 + r.Intn(12)
		base := make([]string, size)
		for i := range base {
			base[i] = fmt.Sprintf("line %d\n", i)
		}
		// Two disjoint blocks: [a, a+na) and [b, b+nb) with a+na <= b.
		a := r.Intn(size - 1)
		na := 1 + r.Intn(size-1-a)
		b := a + na + r.Intn(size-a-na)
		if b >= size {
			continue
		}
		nb := 1 + r.Intn(size-b)
		ours, theirs := deleteBlock(base, a, na), deleteBlock(base, b, nb)
		var want []string
		for i, l := range base {
			if !(i >= a && i < a+na) && !(i >= b && i < b+nb) {
				want = append(want, l)
			}
		}
		for _, swap := range []bool{false, true} {
			o, th := ours, theirs
			if swap {
				o, th = theirs, ours
			}
			res := Merge3(base, o, th, Labels{})
			if res.Conflicts != 0 || !equalLines(res.Lines, want) {
				t.Fatalf("base %q\nours %q\ntheirs %q\ngot %q (conflicts %d), want %q", base, o, th, res.Lines, res.Conflicts, want)
			}
		}
	}
}

// With repeated lines and random deletions on both sides, a clean merge
// only ever deletes: the result is a subsequence of ours and of theirs, and
// overlapping deletions of distinct lines still conflict.
func TestMerge3DeletionsOnlyDelete(t *testing.T) {
	r := rand.New(rand.NewSource(26))
	pool := []string{"}\n", "\n", "x\n", "y\n", "z\n"}
	del := func(ls []string) []string {
		out := append([]string(nil), ls...)
		for n := 1 + r.Intn(3); n > 0 && len(out) > 0; n-- {
			i := r.Intn(len(out))
			out = append(out[:i], out[i+1:]...)
		}
		return out
	}
	for iter := 0; iter < 3000; iter++ {
		base := make([]string, 1+r.Intn(10))
		for i := range base {
			base[i] = pool[r.Intn(len(pool))]
		}
		ours, theirs := del(base), del(base)
		res := Merge3(base, ours, theirs, Labels{})
		if res.Conflicts == 0 && (!isSubsequence(res.Lines, ours) || !isSubsequence(res.Lines, theirs)) {
			t.Fatalf("base %q ours %q theirs %q: clean merge %q is not a subsequence of both", base, ours, theirs, res.Lines)
		}
	}
	// Overlap: both delete line 2 of distinct lines.
	base := lines("a\nb\nc\nd\ne\n")
	if res := Merge3(base, lines("a\nd\ne\n"), lines("a\nb\ne\n"), Labels{}); res.Conflicts == 0 {
		t.Fatalf("overlapping deletions merged: %q", res.Lines)
	}
}
