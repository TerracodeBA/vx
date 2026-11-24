package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/merge"
)

func checkGo(t *testing.T, files map[string]string, changed ...string) []Warning {
	t.Helper()
	f := newFixture(t)
	root := f.tree(files)
	ws, err := CheckGo(f.ctx, f.s, root, changed)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func details(ws []Warning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Detail)
	}
	return out
}

func wantDetails(t *testing.T, ws []Warning, want ...string) {
	t.Helper()
	if got := details(ws); !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings:\n  got  %q\n  want %q", got, want)
	}
}

const pkgHeader = "package httpapi\n\n"

func TestCheckGoDuplicateFuncAcrossFiles(t *testing.T) {
	ws := checkGo(t, map[string]string{
		"internal/httpapi/limit.go":  pkgHeader + "func clientIP() string { return \"\" }\n",
		"internal/httpapi/audit.go":  pkgHeader + "func clientIP() string { return \"x\" }\n",
		"internal/httpapi/server.go": pkgHeader + "func serve() {}\n",
	}, "internal/httpapi/audit.go")
	wantDetails(t, ws, "duplicate func clientIP in internal/httpapi (audit.go, limit.go)")
	w := ws[0]
	if w.Kind != WarningDuplicate || w.Path != "internal/httpapi" || w.Name != "clientIP" || w.Decl != "func" ||
		!reflect.DeepEqual(w.Files, []string{"audit.go", "limit.go"}) {
		t.Fatalf("warning = %+v", w)
	}
}

func TestCheckGoDuplicateKinds(t *testing.T) {
	ws := checkGo(t, map[string]string{
		"p/a.go": "package p\n\ntype T struct{}\nvar X, _ = 1, 2\nconst C = 1\nfunc F() {}\nfunc init() {}\nfunc _() {}\n",
		"p/b.go": "package p\n\ntype T int\nvar X = 3\nconst (\n\tC = 2\n)\nvar F = 1\nfunc init() {}\nfunc _() {}\nvar _ = 4\n",
	}, "p/b.go")
	wantDetails(t, ws,
		"duplicate const C in p (a.go, b.go)",
		"duplicate declaration F in p (a.go, b.go)",
		"duplicate type T in p (a.go, b.go)",
		"duplicate var X in p (a.go, b.go)",
	)
}

func TestCheckGoMethods(t *testing.T) {
	files := map[string]string{
		"p/a.go": "package p\n\ntype S struct{}\ntype U struct{}\ntype G[K any] struct{}\n\n" +
			"func (s *S) Close() {}\nfunc (U) Close() {}\nfunc Close() {}\nfunc (g *G[K]) Add() {}\n",
		"p/b.go": "package p\n\nfunc (s S) Close() {}\nfunc (g G[K]) Add() {}\nfunc (u *U) Open() {}\n",
	}
	// Method vs function, and methods on different receivers, are fine;
	// methods on the same receiver type (pointer or not) are duplicates.
	wantDetails(t, checkGo(t, files, "p/b.go"),
		"duplicate method G.Add in p (a.go, b.go)",
		"duplicate method S.Close in p (a.go, b.go)",
	)
}

func TestCheckGoSameFile(t *testing.T) {
	// Two changes each added the func to the same file, in different places.
	ws := checkGo(t, map[string]string{
		"main.go": "package main\n\nfunc helper() {}\n\nfunc main() {}\n\nfunc helper() {}\n",
	}, "main.go")
	wantDetails(t, ws, "duplicate func helper in . (main.go)")
}

func TestCheckGoTestPackageSplit(t *testing.T) {
	files := map[string]string{
		"p/p.go":           "package p\n\nfunc helper() {}\n",
		"p/ext_test.go":    "package p_test\n\nfunc helper() {}\n",
		"p/ext2_test.go":   "package p_test\n\nfunc other() {}\n",
		"p/inner_test.go":  "package p\n\nfunc testOnly() {}\n",
		"p/inner2_test.go": "package p\n\nfunc testOnly() {}\n",
	}
	// helper in p and p_test are different packages.
	wantDetails(t, checkGo(t, files, "p/ext_test.go", "p/p.go"))
	// An internal test file is part of package p.
	wantDetails(t, checkGo(t, files, "p/inner2_test.go"), "duplicate func testOnly in p (inner2_test.go, inner_test.go)")

	files["p/ext2_test.go"] = "package p_test\n\nfunc helper() {}\n"
	wantDetails(t, checkGo(t, files, "p/ext2_test.go"), "duplicate func helper in p (package p_test) (ext2_test.go, ext_test.go)")
}

func TestCheckGoParseErrors(t *testing.T) {
	files := map[string]string{
		"p/a.go":      "package p\n\nfunc clientIP() {}\n",
		"p/b.go":      "package p\n\nfunc clientIP() {}\n",
		"p/broken.go": "package p\n\nfunc broken( {\n",
		"p/old.go":    "package p\n\nthis is not go\n",
	}
	ws := checkGo(t, files, "p/broken.go", "p/b.go")
	wantDetails(t, ws,
		"p/broken.go does not parse: line 3: expected ')', found '{'",
		"duplicate func clientIP in p (a.go, b.go)",
	)
	if ws[0].Kind != WarningParse || ws[0].Path != "p/broken.go" {
		t.Fatalf("parse warning = %+v", ws[0])
	}
}

func TestCheckGoOnlyChangedDuplicates(t *testing.T) {
	files := map[string]string{
		"p/a.go": "package p\n\nfunc old() {}\n",
		"p/b.go": "package p\n\nfunc old() {}\n",
		"p/c.go": "package p\n\nfunc fresh() {}\n",
	}
	// The duplicate predates the change to c.go: not reported again.
	wantDetails(t, checkGo(t, files, "p/c.go"))
	// Non-Go files, deleted directories and ignored directories are skipped.
	wantDetails(t, checkGo(t, map[string]string{
		"p/testdata/a.go": "package p\n\nfunc x() {}\n",
		"p/testdata/b.go": "package p\n\nfunc x() {}\n",
		"_tools/a.go":     "package p\n\nfunc x() {}\n",
		"_tools/b.go":     "package p\n\nfunc x() {}\n",
		"README.md":       "readme",
	}, "p/testdata/b.go", "_tools/b.go", "README.md", "gone/x.go"))
}

func TestCheckGoBuildConstraints(t *testing.T) {
	files := map[string]string{
		"p/a_linux.go":   "package p\n\nfunc osName() string { return \"linux\" }\n",
		"p/a_windows.go": "package p\n\nfunc osName() string { return \"windows\" }\n",
		"p/a_other.go":   "//go:build !linux && !windows\n\npackage p\n\nfunc osName() string { return \"other\" }\n",
		"p/gen.go":       "//go:build ignore\n\npackage main\n\nfunc main() {}\n",
		"p/gen2.go":      "//go:build ignore\n\npackage main\n\nfunc main() {}\n",
	}
	wantDetails(t, checkGo(t, files, "p/a_linux.go", "p/a_windows.go", "p/a_other.go", "p/gen2.go"))

	files["p/b.go"] = "package p\n\nfunc osName() string { return \"\" }\n"
	wantDetails(t, checkGo(t, files, "p/b.go"), "duplicate func osName in p (a_linux.go, b.go)")
}

// The case from issue #14: two changes add the same func in different
// files, the text merge is clean, and the package stops compiling.
func TestCheckGoAfterCleanMerge(t *testing.T) {
	f := newFixture(t)
	base := f.tree(map[string]string{"api/server.go": pkgHeader + "func serve() {}\n"})
	ours := f.tree(map[string]string{
		"api/server.go": pkgHeader + "func serve() {}\n",
		"api/limit.go":  pkgHeader + "func clientIP() string { return \"\" }\n",
	})
	theirs := f.tree(map[string]string{
		"api/server.go": pkgHeader + "func serve() {}\n",
		"api/audit.go":  pkgHeader + "func clientIP() string { return \"\" }\n",
	})
	mr, err := MergeTrees(f.ctx, f.s, base, ours, theirs, merge.Labels{})
	if err != nil || !mr.Clean() {
		t.Fatalf("merge: %v %+v", err, mr.Conflicts)
	}
	ws, err := CheckGo(f.ctx, f.s, mr.Tree, []string{"api/limit.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || !strings.Contains(ws[0].Detail, "duplicate func clientIP in api (audit.go, limit.go)") {
		t.Fatalf("warnings = %q", details(ws))
	}
}
