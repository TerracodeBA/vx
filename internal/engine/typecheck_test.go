package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mergeCheck merges like semantic and runs CheckGoMerge on the result.
func mergeCheck(t *testing.T, base, ours, theirs map[string]string) []Warning {
	t.Helper()
	f := newFixture(t)
	side := func(changes map[string]string) map[string]string {
		out := map[string]string{}
		for p, c := range base {
			out[p] = c
		}
		for p, c := range changes {
			if c == "" {
				delete(out, p)
			} else {
				out[p] = c
			}
		}
		return out
	}
	b, o, th := f.tree(base), f.tree(side(ours)), f.tree(side(theirs))
	r := f.merge(base, side(ours), side(theirs))
	if !r.Clean() {
		t.Fatalf("conflicts: %+v", r.Conflicts)
	}
	ws, err := CheckGoMerge(f.ctx, f.s, GoSides{Base: b, Ours: o, Theirs: th, Merged: r.Tree})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

const muxBase = "package web\n\nfunc register(m *Mux) {\n\tm.Handle(\"a\")\n}\n\ntype Mux struct{}\n\nfunc (m *Mux) Handle(string) {}\n"

// The reported case: both sides add code at the end of a function, one
// of them ending in a return; the merge keeps both, and the other side's
// new code comes after the return.
func TestCheckGoMergeUnreachable(t *testing.T) {
	theirs := strings.Replace(muxBase, "\tm.Handle(\"a\")\n", "\tm.Handle(\"a\")\n\tm.Handle(\"b\")\n\treturn\n", 1)
	ours := strings.Replace(muxBase, "\tm.Handle(\"a\")\n", "\tm.Handle(\"a\")\n\tm.Handle(\"c\")\n", 1)
	ws := mergeCheck(t, map[string]string{"web/mux.go": muxBase}, map[string]string{"web/mux.go": ours}, map[string]string{"web/mux.go": theirs})
	if len(ws) != 1 || ws[0].Kind != WarningUnreachable || ws[0].Path != "web/mux.go" || ws[0].Name != "register" ||
		ws[0].Detail != "web/mux.go:7: unreachable code in register after the merge (neither side had it; both changed this package)" {
		t.Fatalf("warnings:\n%s", semDetails(ws))
	}
}

// One side renames a helper, the other adds a call to it in another file:
// undefined.
func TestCheckGoMergeUndefined(t *testing.T) {
	base := map[string]string{
		"shop/a.go": "package shop\n\nfunc helper() int { return 1 }\n",
		"shop/c.go": "package shop\n\nfunc C() int { return helper() }\n",
	}
	ours := map[string]string{
		"shop/a.go": "package shop\n\nfunc assist() int { return 1 }\n",
		"shop/c.go": "package shop\n\nfunc C() int { return assist() }\n",
	}
	theirs := map[string]string{
		"shop/d.go": "package shop\n\nfunc D() int { return helper() + 1 }\n",
	}
	ws := mergeCheck(t, base, ours, theirs)
	if len(ws) != 1 || ws[0].Kind != WarningUndefined || ws[0].Name != "helper" || ws[0].Path != "shop/d.go" ||
		ws[0].Detail != "shop/d.go:3: undefined: helper after the merge (neither side had it; both changed this package)" {
		t.Fatalf("undefined:\n%s", semDetails(ws))
	}

	// A problem one side already had is not the merge's.
	theirs["shop/c.go"] = "package shop\n\nfunc C() int { return helper() + missing() }\n"
	ours["shop/c.go"] = "package shop\n\nfunc C() int { return missing() }\n"
	theirs["shop/c.go"] = ""
	ws = mergeCheck(t, base, ours, map[string]string{"shop/d.go": "package shop\n\nfunc D() int { return 2 }\n"})
	if len(ws) != 0 {
		t.Fatalf("problem ours had: %s", semDetails(ws))
	}
}

// Each side drops one of two uses of an import: the merge uses it nowhere.
func TestCheckGoMergeUnusedImport(t *testing.T) {
	const head = "package shop\n\nimport \"strings\"\n\n"
	base := map[string]string{"shop/b.go": head + "func U(s string) string { return strings.ToUpper(s) }\n\n// A gap, so the two edits merge.\n\n// More gap.\n\nfunc W(s string) string { return strings.ToLower(s) }\n"}
	ours := map[string]string{"shop/b.go": head + "func U(s string) string { return s }\n\n// A gap, so the two edits merge.\n\n// More gap.\n\nfunc W(s string) string { return strings.ToLower(s) }\n"}
	theirs := map[string]string{"shop/b.go": head + "func U(s string) string { return strings.ToUpper(s) }\n\n// A gap, so the two edits merge.\n\n// More gap.\n\nfunc W(s string) string { return s }\n"}
	ws := mergeCheck(t, base, ours, theirs)
	if len(ws) != 1 || ws[0].Kind != WarningUnusedImport || ws[0].Name != "strings" ||
		ws[0].Detail != `shop/b.go:3: "strings" imported and not used after the merge (neither side had it; both changed this package)` {
		t.Fatalf("unused import:\n%s", semDetails(ws))
	}
}

// Packages only one side changed, problems in imported packages and
// problems in code that does not parse are left alone.
func TestCheckGoMergeQuiet(t *testing.T) {
	base := map[string]string{"a/a.go": "package a\n\nfunc A() {}\n", "b/b.go": "package b\n\nfunc B() {}\n"}
	ws := mergeCheck(t, base,
		map[string]string{"a/a.go": "package a\n\nfunc A() { return; missing() }\n"},
		map[string]string{"b/b.go": "package b\n\nfunc B() { gone() }\n"})
	if len(ws) != 0 {
		t.Fatalf("one side per package: %s", semDetails(ws))
	}
	ws = mergeCheck(t, map[string]string{"a/a.go": "package a\n\nimport \"example.com/x\"\n\nfunc A() { x.Old() }\n"},
		map[string]string{"a/a.go": "package a\n\nimport \"example.com/x\"\n\nfunc A() { x.Old(); x.New() }\n"},
		map[string]string{"a/b.go": "package a\n\nimport . \"example.com/y\"\n\nfunc B() { Dotted() }\n"})
	if len(ws) != 0 {
		t.Fatalf("imported names: %s", semDetails(ws))
	}
}

// The unreachable check follows the spec's terminating statements: loops
// with breaks, switches whose break only ends the switch, labels, goto
// targets and a shadowed panic.
func TestUnreachableRules(t *testing.T) {
	code := `package a

func Loops(n int) int {
	for {
		if n > 3 {
			break
		}
		n++
	}
	for {
		switch n {
		case 1:
			break
		default:
			return n
		}
	}
	return 0
}

func Labels(n int) int {
outer:
	for {
		for {
			break outer
		}
	}
	goto end
end:
	return n
}

func Panics() {
	panic("x")
}

func Shadow(panic func(string)) int {
	panic("x")
	return 1
}

func Literal() func() int {
	return func() int {
		return 1
		println()
		return 2
	}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "a/a.go", code, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range checkGoPackage(fset, "a", []*ast.File{f}) {
		got = append(got, fmt.Sprintf("%d %s %s", p.line, p.kind, p.name))
	}
	want := []string{"18 unreachable Loops", "45 unreachable Literal"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestImportName(t *testing.T) {
	for p, want := range map[string]string{
		"fmt": "fmt", "github.com/jackc/pgx/v5": "pgx", "gopkg.in/yaml.v3": "yaml", "github.com/go-git/go-git/v5": "git",
		"github.com/mattn/go-sqlite3": "sqlite3", "github.com/BurntSushi/toml": "toml", "example.com/sdk-go": "sdk", "example.com/a-b": "ab",
	} {
		if got := importName(p); got != want {
			t.Errorf("importName(%q) = %q, want %q", p, got, want)
		}
	}
}

// A package of a few hundred functions merges and checks in well under a
// second.
func TestCheckGoMergeFast(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n\nimport \"fmt\"\n\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("func F" + strconv.Itoa(i) + "(n int) string {\n\tif n > 0 {\n\t\treturn fmt.Sprint(n)\n\t}\n\treturn \"\"\n}\n\n")
	}
	base := map[string]string{"big/big.go": b.String()}
	start := time.Now()
	mergeCheck(t, base, map[string]string{"big/x.go": "package big\n\nfunc X() {}\n"}, map[string]string{"big/y.go": "package big\n\nfunc Y() {}\n"})
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestWithoutCovered(t *testing.T) {
	sides := []Warning{{Path: "audit.go", Kind: WarningRemoved, Name: "auth.Login"}}
	typed := []Warning{
		{Path: "audit.go", Kind: WarningUndefined, Name: "Login"},
		{Path: "audit.go", Kind: WarningUndefined, Name: "Logout"},
		{Path: "other.go", Kind: WarningUndefined, Name: "Login"},
		{Path: "audit.go", Kind: WarningUnreachable, Name: "Login"},
	}
	got := WithoutCovered(sides, typed)
	if len(got) != 3 || got[0].Name != "Logout" || got[1].Path != "other.go" || got[2].Kind != WarningUnreachable {
		t.Fatalf("got %+v", got)
	}
}
