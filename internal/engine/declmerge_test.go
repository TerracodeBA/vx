package engine

import (
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/merge"
)

// lineConflicts reports whether the line merge of the three versions has a
// conflict, which is when MergeTrees tries a merge by declaration.
func lineConflicts(base, ours, theirs string) bool {
	r := merge.Merge3(merge.SplitLines([]byte(base)), merge.SplitLines([]byte(ours)), merge.SplitLines([]byte(theirs)), merge.Labels{})
	return r.Conflicts > 0
}

func mustDeclMerge(t *testing.T, base, ours, theirs string) string {
	t.Helper()
	out, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs))
	if !ok {
		t.Fatalf("merge by declaration failed\nbase:\n%s\nours:\n%s\ntheirs:\n%s", base, ours, theirs)
	}
	assertGofmt(t, out)
	return string(out)
}

func assertGofmt(t *testing.T, out []byte) {
	t.Helper()
	if _, err := parser.ParseFile(token.NewFileSet(), "x.go", out, parser.ParseComments); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, out)
	}
	f, err := format.Source(out)
	if err != nil || string(f) != string(out) {
		t.Fatalf("result is not gofmt'ed:\n%s", out)
	}
}

// The case the line merge cannot do: one side deletes a function, the
// other edits the doc comment of the function right after it.
func TestDeclMergeAdjacentEdits(t *testing.T) {
	base := `package auth

// A is first.
func A() int {
	return 1
}

// B is second.
func B() int {
	return 2
}
`
	ours := `package auth

// B is second, and better documented.
func B() int {
	return 2
}
`
	theirs := `package auth

// A is first.
func A() int {
	return 1
}

// Bee is second.
// It changed.
func B() int {
	return 20
}
`
	if !lineConflicts(base, ours, theirs) {
		t.Fatal("fixture should conflict line by line")
	}
	// Both edited B: no merge.
	if _, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs)); ok {
		t.Fatal("both sides changed B, the merge must conflict")
	}
	theirs = `package auth

// A is first.
func A() int {
	return 1
}

// C is new, added right above B.
func C() int {
	return 3
}

// B is second.
func B() int {
	return 2
}
`
	ours = `package auth

// B is second.
func B() int {
	return 22
}
`
	if !lineConflicts(base, ours, theirs) {
		t.Fatal("fixture should conflict line by line")
	}
	got := mustDeclMerge(t, base, ours, theirs)
	want := `package auth

// C is new, added right above B.
func C() int {
	return 3
}

// B is second.
func B() int {
	return 22
}
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestDeclMergeImports(t *testing.T) {
	base := `package p

import (
	"fmt"
	"strings"
)

func A() string { return fmt.Sprint(1) }

func B() string { return strings.ToUpper("b") }
`
	// Ours drops B and its import, adds C using os; theirs edits A to use
	// strings and adds D using errors.
	ours := `package p

import (
	"fmt"
	"os"
)

func A() string { return fmt.Sprint(1) }

func C() string { return os.Getenv("C") }
`
	theirs := `package p

import (
	"errors"
	"fmt"
	"strings"
)

func A() string { return fmt.Sprint(strings.ToLower("A")) }

func B() string { return strings.ToUpper("b") }

func D() error { return errors.New("d") }
`
	got := mustDeclMerge(t, base, ours, theirs)
	for _, want := range []string{`"errors"`, `"fmt"`, `"os"`, `"strings"`, "func C()", "func D()", "strings.ToLower"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in\n%s", want, got)
		}
	}
	if strings.Contains(got, "func B()") {
		t.Fatalf("B was deleted by ours:\n%s", got)
	}

	// An import one side removed, unused by the merged code, stays out.
	theirs2 := strings.Replace(theirs, `fmt.Sprint(strings.ToLower("A"))`, `fmt.Sprint(2)`, 1)
	theirs2 = strings.Replace(theirs2, "\nfunc B() string { return strings.ToUpper(\"b\") }\n", "\nfunc B() string { return strings.ToUpper(\"bee\") }\n", 1)
	if _, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs2)); ok {
		t.Fatal("ours deleted B, theirs edited it: must conflict")
	}

	// Named imports; the same name for two packages does not merge.
	ours3 := strings.Replace(ours, `"os"`, `x "os"`, 1)
	ours3 = strings.Replace(ours3, "os.Getenv", "x.Getenv", 1)
	theirs3 := strings.Replace(theirs, `"errors"`, `x "errors"`, 1)
	theirs3 = strings.Replace(theirs3, "errors.New", "x.New", 1)
	if _, ok := mergeGoDecls([]byte(base), []byte(ours3), []byte(theirs3)); ok {
		t.Fatal("x names two packages: must conflict")
	}
}

func TestDeclMergeMethodsAndGenerics(t *testing.T) {
	base := `package p

type List[T any] struct{ items []T }

func (l *List[T]) Len() int { return len(l.items) }

type Set map[string]bool

func (s Set) Len() int { return len(s) }

func Map[T, U any](xs []T, f func(T) U) []U {
	var out []U
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}
`
	ours := `package p

type List[T any] struct{ items []T }

func (l *List[T]) Len() int { return len(l.items) + 0 }

func (l *List[T]) Push(x T) { l.items = append(l.items, x) }

type Set map[string]bool

func (s Set) Len() int { return len(s) }

func Map[T, U any](xs []T, f func(T) U) []U {
	var out []U
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}
`
	theirs := `package p

type List[T any] struct{ items []T }

func (l *List[T]) Len() int { return len(l.items) }

type Set map[string]bool

func (s Set) Len() int { return len(s) * 1 }

func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}
`
	got := mustDeclMerge(t, base, ours, theirs)
	for _, want := range []string{"len(l.items) + 0", "Push(x T)", "len(s) * 1", "make([]U, 0, len(xs))"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in\n%s", want, got)
		}
	}
	// The same method name on another type is another declaration, but the
	// same method twice is not.
	theirs2 := strings.Replace(theirs, "len(s) * 1", "len(s) * 1", 1)
	theirs2 = strings.Replace(theirs2, "return len(l.items) }", "return len(l.items) - 0 }", 1)
	if _, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs2)); ok {
		t.Fatal("both changed List.Len: must conflict")
	}
}

func TestDeclMergeComments(t *testing.T) {
	base := `// Package p does things.
package p

// ---- helpers ----

// A is documented.
func A() {} // A's line comment

/*
B has a block comment.
*/
func B() {}

// trailing comment
`
	ours := `// Package p does things.
package p

// ---- helpers ----

// A is documented better.
func A() {} // A's line comment

/*
B has a block comment.
*/
func B() {}

// trailing comment
`
	theirs := `// Package p does things.
package p

// ---- helpers ----

// A is documented.
func A() {} // A's line comment

/*
B has a block comment, changed.
*/
func B() { println() }

// trailing comment, changed
`
	got := mustDeclMerge(t, base, ours, theirs)
	for _, want := range []string{"// Package p does things.", "// ---- helpers ----", "// A is documented better.", "// A's line comment", "B has a block comment, changed.", "// trailing comment, changed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	// Doc comments stay with their declaration.
	if strings.Index(got, "B has a block comment") > strings.Index(got, "func B()") {
		t.Fatalf("doc comment moved:\n%s", got)
	}
}

func TestDeclMergeBuildTagsAndHeader(t *testing.T) {
	base := "//go:build linux\n\npackage p\n\nfunc A() {}\n\nfunc B() {}\n"
	ours := "//go:build linux && amd64\n\npackage p\n\nfunc A() { println() }\n\nfunc B() {}\n"
	theirs := "//go:build linux\n\npackage p\n\nfunc A() {}\n\nfunc B() { print() }\n"
	got := mustDeclMerge(t, base, ours, theirs)
	if !strings.HasPrefix(got, "//go:build linux && amd64\n") || !strings.Contains(got, "println()") || !strings.Contains(got, "print()") {
		t.Fatalf("got\n%s", got)
	}
	theirs2 := strings.Replace(theirs, "//go:build linux", "//go:build darwin", 1)
	if _, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs2)); ok {
		t.Fatal("both changed the build constraint: must conflict")
	}
	theirs3 := strings.Replace(theirs, "package p", "package q", 1)
	if _, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs3)); ok {
		t.Fatal("package names differ: must conflict")
	}
}

func TestDeclMergeCgoAndGenerated(t *testing.T) {
	base := "package p\n\n// #include <stdio.h>\nimport \"C\"\n\nimport \"fmt\"\n\nfunc A() { fmt.Println() }\n\nfunc B() {}\n"
	ours := "package p\n\n// #include <stdio.h>\nimport \"C\"\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc A() { fmt.Println(os.Args) }\n\nfunc B() {}\n"
	theirs := "package p\n\n// #include <stdio.h>\nimport \"C\"\n\nimport (\n\t\"fmt\"\n\t\"io\"\n)\n\nfunc A() { fmt.Println() }\n\nfunc B() { _ = io.EOF }\n"
	if _, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs)); ok {
		t.Fatal(`both changed the imports of a file that imports "C": must conflict`)
	}
	// One side changing the imports keeps the preamble verbatim.
	theirs = "package p\n\n// #include <stdio.h>\nimport \"C\"\n\nimport \"fmt\"\n\nfunc A() { fmt.Println() }\n\nfunc B() { println() }\n"
	got := mustDeclMerge(t, base, ours, theirs)
	if !strings.Contains(got, "// #include <stdio.h>\nimport \"C\"") {
		t.Fatalf("cgo preamble lost:\n%s", got)
	}
	gen := "// Code generated by x. DO NOT EDIT.\n\n"
	if _, ok := mergeGoDecls([]byte(gen+base), []byte(gen+ours), []byte(gen+theirs)); ok {
		t.Fatal("generated files are never merged by declaration")
	}
}

func TestDeclMergeValueDecls(t *testing.T) {
	base := "package p\n\nconst (\n\tA = iota\n\tB\n)\n\nvar x = 1\n\nvar _ = x\n\nfunc init() { x = 2 }\n\nfunc init() { x = 3 }\n"
	// Both add a const to the iota block: that is one declaration.
	ours := strings.Replace(base, "\tB\n", "\tB\n\tC\n", 1)
	theirs := strings.Replace(base, "\tB\n", "\tB\n\tD\n", 1)
	if _, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs)); ok {
		t.Fatal("both extended one const block: must conflict")
	}
	// One side deletes a var, the other extends it into a group: the keys
	// differ, the names do not.
	ours = strings.Replace(base, "var x = 1\n\n", "", 1)
	ours = strings.Replace(ours, "var _ = x\n\n", "", 1)
	ours = strings.Replace(ours, "func init() { x = 2 }\n\nfunc init() { x = 3 }\n", "", 1)
	theirs = strings.Replace(base, "var x = 1", "var x, y = 1, 2", 1)
	if _, ok := mergeGoDecls([]byte(base), []byte(ours), []byte(theirs)); ok {
		t.Fatal("ours deleted x, theirs changed it: must conflict")
	}
	// Edits to different init funcs and blank vars merge.
	ours = strings.Replace(base, "x = 2", "x = 20", 1) + "\nvar _ = A\n"
	theirs = strings.Replace(base, "x = 3", "x = 30", 1)
	theirs = strings.Replace(theirs, "var x = 1", "var x = 10", 1)
	got := mustDeclMerge(t, base, ours, theirs)
	for _, want := range []string{"x = 20", "x = 30", "var x = 10", "var _ = A", "var _ = x"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
}

func TestDeclMergeThroughMergeTrees(t *testing.T) {
	f := newFixture(t)
	base := "package p\n\n// A is first.\nfunc A() int {\n\treturn 1\n}\n\n// B is second.\nfunc B() int {\n\treturn 2\n}\n"
	ours := "package p\n\n// B is second.\nfunc B() int {\n\treturn 2\n}\n"
	theirs := "package p\n\n// A is first.\nfunc A() int {\n\treturn 1\n}\n\n// B is second, said theirs.\nfunc B() int {\n\treturn 2\n}\n"
	r := f.merge(map[string]string{"p.go": base, "gen/p.go": base}, map[string]string{"p.go": ours, "gen/p.go": ours}, map[string]string{"p.go": theirs, "gen/p.go": theirs})
	if !r.Clean() || strings.Join(r.MergedByDecl, ",") != "gen/p.go,p.go" || strings.Join(r.AutoMerged, ",") != "gen/p.go,p.go" {
		t.Fatalf("conflicts %+v, merged by decl %v, auto %v", r.Conflicts, r.MergedByDecl, r.AutoMerged)
	}
	got := f.read(r.Tree)["p.go"]
	if got != "package p\n\n// B is second, said theirs.\nfunc B() int {\n\treturn 2\n}\n" {
		t.Fatalf("p.go = %q", got)
	}
	// A generated-file rule keeps ours instead.
	r = f.merge(map[string]string{"gen/p.go": base}, map[string]string{"gen/p.go": ours}, map[string]string{"gen/p.go": theirs}, WithGenerated(func(p string) (string, bool) { return "make gen", strings.HasPrefix(p, "gen/") }))
	if !r.Clean() || len(r.MergedByDecl) != 0 || len(r.Generated) != 1 {
		t.Fatalf("generated: %+v", r)
	}
	// Overlapping edits keep the text conflict exactly.
	ours2 := strings.Replace(theirs, "said theirs", "said ours", 1)
	r = f.merge(map[string]string{"p.go": base}, map[string]string{"p.go": ours2}, map[string]string{"p.go": theirs})
	if r.Clean() || r.Conflicts[0].Kind != ConflictContent || len(r.MergedByDecl) != 0 {
		t.Fatalf("overlap: %+v", r)
	}
	want := merge.Merge3(merge.SplitLines([]byte(base)), merge.SplitLines([]byte(ours2)), merge.SplitLines([]byte(theirs)), merge.Labels{Base: "base", Ours: "ours", Theirs: "theirs"})
	if string(r.Conflicts[0].Markers) != string(want.Bytes()) {
		t.Fatalf("markers changed:\n%s", r.Conflicts[0].Markers)
	}
}

// genDecl is one declaration of a generated test file.
type genDecl struct{ key, text string }

func funcText(name, doc string, ret int) string {
	return fmt.Sprintf("// %s %s.\nfunc %s() int {\n\treturn %d\n}\n", name, doc, name, ret)
}

func render(decls []genDecl, imports []string) string {
	var b strings.Builder
	b.WriteString("package p\n")
	if len(imports) > 0 {
		b.WriteString("\nimport (\n")
		for _, i := range imports {
			b.WriteString("\t\"" + i + "\"\n")
		}
		b.WriteString(")\n")
	}
	for _, d := range decls {
		b.WriteString("\n" + d.text)
	}
	return b.String()
}

// sideEdit applies random edits to base: modify, delete or add, on the
// declarations in mine only (adds use fresh names). It returns the new
// declaration list, the imports and what each key ends up as (missing:
// deleted).
func sideEdit(rng *rand.Rand, base []genDecl, mine map[string]bool, side string) ([]genDecl, []string, map[string]string, map[string]bool) {
	out := append([]genDecl(nil), base...)
	final := map[string]string{}
	deleted := map[string]bool{}
	var imports []string
	for i := 0; i < len(out); i++ {
		d := out[i]
		if !mine[d.key] {
			continue
		}
		switch rng.Intn(3) {
		case 0: // modify
			name := strings.TrimPrefix(d.key, "func ")
			d.text = funcText(name, "changed by "+side, rng.Intn(1000))
			out[i] = d
			final[d.key] = d.text
		case 1: // delete
			deleted[d.key] = true
			out = append(out[:i], out[i+1:]...)
			i--
		default: // leave alone
		}
	}
	adds := rng.Intn(3)
	for j := 0; j < adds; j++ {
		name := fmt.Sprintf("%s%d", strings.ToUpper(side[:1]), j)
		text := funcText(name, "added by "+side, j)
		if rng.Intn(2) == 0 {
			pkg := map[string]string{"ours": "strings", "theirs": "bytes"}[side]
			text = fmt.Sprintf("func %s() int { return len(%s.TrimSpace(nil)) }\n", name, pkg)
			if pkg == "strings" {
				text = fmt.Sprintf("func %s() int { return len(%s.TrimSpace(\"\")) }\n", name, pkg)
			}
			if len(imports) == 0 {
				imports = append(imports, pkg)
			}
		}
		at := rng.Intn(len(out) + 1)
		out = append(out[:at], append([]genDecl{{key: "func " + name, text: text}}, out[at:]...)...)
		final["func "+name] = text
	}
	return out, imports, final, deleted
}

// TestDeclMergeProperties: disjoint edits always merge, the result always
// parses and is gofmt'ed and never loses a declaration either side had;
// overlapping edits never merge.
func TestDeclMergeProperties(t *testing.T) {
	for seed := int64(0); seed < 400; seed++ {
		rng := rand.New(rand.NewSource(seed))
		n := 2 + rng.Intn(8)
		var base []genDecl
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("F%d", i)
			base = append(base, genDecl{key: "func " + name, text: funcText(name, "is base", i)})
		}
		oursKeys, theirsKeys := map[string]bool{}, map[string]bool{}
		for _, d := range base {
			switch rng.Intn(3) {
			case 0:
				oursKeys[d.key] = true
			case 1:
				theirsKeys[d.key] = true
			}
		}
		od, oi, ofinal, odel := sideEdit(rng, base, oursKeys, "ours")
		td, ti, tfinal, tdel := sideEdit(rng, base, theirsKeys, "theirs")
		bs, os, ts := render(base, nil), render(od, oi), render(td, ti)

		out, ok := mergeGoDecls([]byte(bs), []byte(os), []byte(ts))
		if !ok {
			t.Fatalf("seed %d: disjoint edits did not merge\nbase:\n%s\nours:\n%s\ntheirs:\n%s", seed, bs, os, ts)
		}
		assertGofmt(t, out)
		got, err := splitGo(out)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{}
		for _, d := range base {
			if !odel[d.key] && !tdel[d.key] {
				want[d.key] = d.text
			}
		}
		for k, v := range ofinal {
			want[k] = v
		}
		for k, v := range tfinal {
			want[k] = v
		}
		var gotKeys, wantKeys []string
		gotKeys = append(gotKeys, got.keys...)
		for k := range want {
			wantKeys = append(wantKeys, k)
		}
		sort.Strings(gotKeys)
		sort.Strings(wantKeys)
		if strings.Join(gotKeys, "|") != strings.Join(wantKeys, "|") {
			t.Fatalf("seed %d: declarations %v, want %v\n%s", seed, gotKeys, wantKeys, out)
		}
		for k, text := range want {
			if strings.TrimSpace(got.chunks[k]) != strings.TrimSpace(text) {
				t.Fatalf("seed %d: %s = %q, want %q", seed, k, got.chunks[k], text)
			}
		}
		for _, imp := range append(oi, ti...) {
			if !strings.Contains(string(out), `"`+imp+`"`) {
				t.Fatalf("seed %d: import %s lost:\n%s", seed, imp, out)
			}
		}

		// Overlap: both sides change one base declaration differently.
		k := base[rng.Intn(len(base))]
		name := strings.TrimPrefix(k.key, "func ")
		ov := func(decls []genDecl, side string) string {
			c := append([]genDecl(nil), decls...)
			found := false
			for i := range c {
				if c[i].key == k.key {
					c[i].text = funcText(name, "overlapped by "+side, 7)
					found = true
				}
			}
			if !found {
				c = append(c, genDecl{key: k.key, text: funcText(name, "re-added by "+side, 7)})
			}
			return render(c, nil)
		}
		if _, ok := mergeGoDecls([]byte(bs), []byte(ov(od, "ours")), []byte(ov(td, "theirs"))); ok {
			t.Fatalf("seed %d: overlapping edits of %s merged", seed, k.key)
		}
	}
}

// Renaming one function two ways, or renaming it on one side and deleting
// it on the other, is a conflict, though every key differs.
func TestDeclMergeRenames(t *testing.T) {
	base := "package p\n\nfunc Login(ctx Context) {\n\tcheck(ctx)\n}\n\nfunc Logout() {\n}\n"
	signIn := strings.Replace(base, "func Login(", "func SignIn(", 1)
	logIn := strings.Replace(base, "func Login(", "func LogIn(", 1)
	deleted := "package p\n\nfunc Logout() {\n}\n"
	for name, sides := range map[string][2]string{"two renames": {signIn, logIn}, "rename and delete": {signIn, deleted}} {
		if _, ok := mergeGoDecls([]byte(base), []byte(sides[0]), []byte(sides[1])); ok {
			t.Errorf("%s: must conflict", name)
		}
	}
	// The same rename on both sides, or both deleting it, merges.
	logout := strings.Replace(signIn, "func Logout() {\n}", "func Logout() {\n\tclear()\n}", 1)
	if got := mustDeclMerge(t, base, signIn, logout); !strings.Contains(got, "func SignIn(") || !strings.Contains(got, "clear()") || strings.Contains(got, "Login") {
		t.Fatalf("got\n%s", got)
	}
	if got := mustDeclMerge(t, base, deleted, strings.Replace(deleted, "{\n}", "{\n\tclear()\n}", 1)); strings.Contains(got, "Login") {
		t.Fatalf("got\n%s", got)
	}
}
