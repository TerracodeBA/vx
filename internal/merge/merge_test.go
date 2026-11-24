package merge

import (
	"math/rand"
	"strings"
	"testing"
)

func lines(s string) []string { return SplitLines([]byte(s)) }

func apply(a, b []string, edits []Edit) []string {
	var out []string
	for _, e := range edits {
		switch e.Kind {
		case Equal:
			out = append(out, a[e.A0:e.A1]...)
		case Insert:
			out = append(out, b[e.B0:e.B1]...)
		}
	}
	return out
}

func TestSplitJoin(t *testing.T) {
	for _, s := range []string{"", "a", "a\n", "a\nb", "a\nb\n", "\n\n"} {
		if got := string(JoinLines(SplitLines([]byte(s)))); got != s {
			t.Errorf("round trip %q = %q", s, got)
		}
	}
}

func TestDiffRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	alphabet := []string{"a\n", "b\n", "c\n", "d\n"}
	gen := func() []string {
		n := r.Intn(30)
		out := make([]string, n)
		for i := range out {
			out[i] = alphabet[r.Intn(len(alphabet))]
		}
		return out
	}
	for iter := 0; iter < 2000; iter++ {
		a, b := gen(), gen()
		edits := Diff(a, b)
		if got := apply(a, b, edits); !equalLines(got, b) {
			t.Fatalf("diff does not reproduce b\na=%q\nb=%q\nedits=%+v", a, b, edits)
		}
		// Edits must cover a and b contiguously.
		ai, bi := 0, 0
		for _, e := range edits {
			if e.A0 != ai || e.B0 != bi {
				t.Fatalf("gap in edits %+v", edits)
			}
			ai, bi = e.A1, e.B1
		}
		if ai != len(a) || bi != len(b) {
			t.Fatalf("edits do not cover inputs")
		}
		// Minimality check against LCS length.
		add, del := Stats(edits)
		l := lcs(a, b)
		if add != len(b)-l || del != len(a)-l {
			t.Fatalf("diff not minimal: +%d -%d, lcs %d, a=%d b=%d", add, del, l, len(a), len(b))
		}
	}
}

func lcs(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

func TestUnified(t *testing.T) {
	a := []byte("one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n")
	b := []byte("one\ntwo\nTHREE\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\n")
	got := Unified("f.txt", "f.txt", a, b, 3)
	want := `--- a/f.txt
+++ b/f.txt
@@ -1,6 +1,6 @@
 one
 two
-three
+THREE
 four
 five
 six
@@ -8,3 +8,4 @@
 eight
 nine
 ten
+eleven
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if Unified("f", "f", a, a, 3) != "" {
		t.Fatal("equal inputs should give empty diff")
	}
	added := Unified("", "new.txt", nil, []byte("x"), 3)
	if !strings.Contains(added, "--- /dev/null") || !strings.Contains(added, "@@ -0,0 +1 @@") || !strings.Contains(added, "\\ No newline at end of file") {
		t.Fatalf("added file diff:\n%s", added)
	}
	if !strings.HasPrefix(Unified("a", "a", []byte{0, 1}, []byte{0, 2}, 3), "Binary files") {
		t.Fatal("binary diff")
	}
}

func TestMerge3Clean(t *testing.T) {
	base := lines("package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {\n\tclear()\n}\n")
	ours := lines("package auth\n\nfunc Login(ctx Context) {\n\tcheck()\n}\n\nfunc Logout() {\n\tclear()\n}\n")
	theirs := lines("package auth\n\nfunc Login() {\n\tcheck()\n}\n\nfunc Logout() {\n\tclear()\n\taudit()\n}\n")
	res := Merge3(base, ours, theirs, Labels{})
	if res.Conflicts != 0 {
		t.Fatalf("unexpected conflict:\n%s", res.Bytes())
	}
	want := "package auth\n\nfunc Login(ctx Context) {\n\tcheck()\n}\n\nfunc Logout() {\n\tclear()\n\taudit()\n}\n"
	if string(res.Bytes()) != want {
		t.Fatalf("got:\n%s", res.Bytes())
	}
}

func TestMerge3SameChange(t *testing.T) {
	base := lines("a\nb\nc\n")
	both := lines("a\nB\nc\n")
	res := Merge3(base, both, both, Labels{})
	if res.Conflicts != 0 || string(res.Bytes()) != "a\nB\nc\n" {
		t.Fatalf("got %d conflicts:\n%s", res.Conflicts, res.Bytes())
	}
}

func TestMerge3Conflict(t *testing.T) {
	base := lines("a\nb\nc\n")
	ours := lines("a\nours\nc\n")
	theirs := lines("a\ntheirs\nc\n")
	res := Merge3(base, ours, theirs, Labels{Base: "base", Ours: "workspace", Theirs: "effort"})
	if res.Conflicts != 1 {
		t.Fatalf("conflicts = %d", res.Conflicts)
	}
	want := "a\n<<<<<<< workspace\nours\n||||||| base\nb\n=======\ntheirs\n>>>>>>> effort\nc\n"
	if string(res.Bytes()) != want {
		t.Fatalf("got:\n%s", res.Bytes())
	}
}

func TestMerge3InsertsAtEnds(t *testing.T) {
	base := lines("m\n")
	ours := lines("top\nm\n")
	theirs := lines("m\nbottom\n")
	res := Merge3(base, ours, theirs, Labels{})
	if res.Conflicts != 0 || string(res.Bytes()) != "top\nm\nbottom\n" {
		t.Fatalf("got %d:\n%s", res.Conflicts, res.Bytes())
	}
	// Both append different lines at the end: conflict.
	res = Merge3(base, lines("m\nx\n"), lines("m\ny\n"), Labels{})
	if res.Conflicts != 1 {
		t.Fatalf("expected conflict, got:\n%s", res.Bytes())
	}
	// Empty base: both create the file differently.
	res = Merge3(nil, lines("x\n"), lines("y\n"), Labels{})
	if res.Conflicts != 1 {
		t.Fatal("expected conflict on add/add")
	}
}

func TestMerge3Properties(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	mutate := func(in []string) []string {
		out := append([]string(nil), in...)
		for n := r.Intn(4); n > 0; n-- {
			switch op := r.Intn(3); {
			case op == 0 && len(out) > 0:
				i := r.Intn(len(out))
				out = append(out[:i], out[i+1:]...)
			case op == 1:
				i := r.Intn(len(out) + 1)
				out = append(out[:i], append([]string{string(rune('A'+r.Intn(26))) + "\n"}, out[i:]...)...)
			case len(out) > 0:
				out[r.Intn(len(out))] = string(rune('a'+r.Intn(26))) + "!\n"
			}
		}
		return out
	}
	for iter := 0; iter < 3000; iter++ {
		base := make([]string, r.Intn(20))
		for i := range base {
			base[i] = string(rune('a'+i)) + "\n"
		}
		ours := mutate(base)
		// Unchanged side: result must be the other side.
		if res := Merge3(base, ours, base, Labels{}); res.Conflicts != 0 || !equalLines(res.Lines, ours) {
			t.Fatalf("merge with unchanged theirs should be ours\nbase=%q\nours=%q\ngot=%q", base, ours, res.Lines)
		}
		if res := Merge3(base, base, ours, Labels{}); res.Conflicts != 0 || !equalLines(res.Lines, ours) {
			t.Fatalf("merge with unchanged ours should be theirs")
		}
		if res := Merge3(base, ours, ours, Labels{}); res.Conflicts != 0 || !equalLines(res.Lines, ours) {
			t.Fatalf("identical changes should merge cleanly")
		}
		theirs := mutate(base)
		a := Merge3(base, ours, theirs, Labels{})
		b := Merge3(base, theirs, ours, Labels{})
		if a.Conflicts != b.Conflicts {
			t.Fatalf("merge not symmetric in conflicts: %d vs %d", a.Conflicts, b.Conflicts)
		}
		if a.Conflicts == 0 && !equalLines(a.Lines, b.Lines) {
			t.Fatalf("clean merge not symmetric\nbase=%q\nours=%q\ntheirs=%q\na=%q\nb=%q", base, ours, theirs, a.Lines, b.Lines)
		}
	}
}

func TestMerge3Bytes(t *testing.T) {
	bin := []byte{0, 1, 2}
	if _, ok := Merge3Bytes(bin, []byte{0, 9}, []byte{0, 8}, Labels{}); ok {
		t.Fatal("conflicting binary should not merge")
	}
	res, ok := Merge3Bytes(bin, bin, []byte{0, 8}, Labels{})
	if !ok || string(res.Bytes()) != string([]byte{0, 8}) {
		t.Fatal("binary changed on one side should take that side")
	}
}
