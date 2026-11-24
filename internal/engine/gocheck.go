package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

// WarningKind names a problem found in a merged tree that does not stop a land.
type WarningKind string

const (
	// WarningDuplicate: one Go package declares the same top-level name twice,
	// typically because two changes each added it in a different file and the
	// text merged cleanly.
	WarningDuplicate WarningKind = "duplicate"
	// WarningParse: a changed Go file does not parse.
	WarningParse WarningKind = "parse"
	// WarningCheck: a check could not run; Detail says why.
	WarningCheck WarningKind = "check"
)

// Warning is a semantic problem in a tree: the text merged, but the result
// will probably not build.
type Warning struct {
	Path   string      // package directory for duplicates ("." for the root), file path for parse errors
	Kind   WarningKind //
	Name   string      // duplicate: the name, "Recv.Name" for methods
	Decl   string      // duplicate: func, method, type, var, const, or "declaration" when kinds differ
	Files  []string    // duplicate: base names of the declaring files, sorted and unique
	Detail string      // human-readable summary
}

// goCheckContext decides which files belong to a package. It is fixed to
// linux/amd64 with cgo and no extra tags, so results do not depend on the
// server's platform.
var goCheckContext = func() build.Context {
	c := build.Default
	c.GOOS = "linux"
	c.GOARCH = "amd64"
	c.CgoEnabled = true
	c.BuildTags = nil
	c.ToolTags = nil
	return c
}()

// CheckGo looks for duplicate top-level declarations in the Go packages of
// root that contain one of the changed paths. Every .go file of such a
// directory is parsed, not only the changed ones, and packages are told
// apart by name, so package x and its external test package x_test in the
// same directory are checked separately. A duplicate is reported only when
// at least one of its declarations is in a changed file, so problems that
// predate the change are not repeated on every land.
//
// Changed .go files that do not parse get a parse warning; other files that
// do not parse are skipped.
//
// Limitations: build constraints (//go:build lines and _GOOS/_GOARCH file
// name suffixes) are evaluated for linux/amd64 with cgo and no custom tags.
// Files excluded on that platform are not checked, so a duplicate between
// two files for another platform, or one behind a custom tag, is missed.
// Only redeclarations are detected: an import name that collides with a
// package-level name, or a method that collides with a field, is not.
func CheckGo(ctx context.Context, s blob.Store, root hash.Hash, changed []string) ([]Warning, error) {
	dirs := map[string]map[string]bool{} // dir -> changed base names
	for _, p := range changed {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		dir, name := path.Split(p)
		dir = strings.TrimSuffix(dir, "/")
		if ignoredGoDir(dir) {
			continue
		}
		if dirs[dir] == nil {
			dirs[dir] = map[string]bool{}
		}
		dirs[dir][name] = true
	}
	order := make([]string, 0, len(dirs))
	for d := range dirs {
		order = append(order, d)
	}
	sort.Strings(order)

	var out []Warning
	for _, dir := range order {
		w, err := checkGoDir(ctx, s, root, dir, dirs[dir])
		if err != nil {
			return nil, err
		}
		out = append(out, w...)
	}
	return out, nil
}

// ignoredGoDir reports whether the go tool ignores dir: testdata, and
// directories whose name starts with "." or "_".
func ignoredGoDir(dir string) bool {
	if dir == "" {
		return false
	}
	for _, part := range strings.Split(dir, "/") {
		if part == "testdata" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return true
		}
	}
	return false
}

type goDecl struct {
	key  string // name, or "Recv.Name" for methods
	kind string // func, method, type, var, const
	file string
}

func checkGoDir(ctx context.Context, s blob.Store, root hash.Hash, dir string, changed map[string]bool) ([]Warning, error) {
	treeHash := root
	if dir != "" {
		e, ok, err := object.Lookup(ctx, s, root, dir)
		if err != nil {
			return nil, err
		}
		if !ok || e.Mode != object.ModeDir {
			return nil, nil // the directory was deleted
		}
		treeHash = e.Hash
	}
	t, err := object.GetTree(ctx, s, treeHash)
	if err != nil {
		return nil, err
	}
	shown := dir
	if shown == "" {
		shown = "."
	}

	files := map[string][]byte{}
	var names []string
	for _, e := range t.Entries {
		if e.Mode != object.ModeFile && e.Mode != object.ModeExec {
			continue
		}
		if !strings.HasSuffix(e.Name, ".go") {
			continue
		}
		data, err := blob.GetKind(ctx, s, hash.KindBlob, e.Hash)
		if err != nil {
			return nil, err
		}
		files[e.Name] = data
		names = append(names, e.Name)
	}

	bc := goCheckContext
	bc.JoinPath = func(elem ...string) string { return path.Join(elem...) }
	bc.OpenFile = func(p string) (io.ReadCloser, error) {
		data, ok := files[path.Base(p)]
		if !ok {
			return nil, fmt.Errorf("%s: not found", p)
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}

	var out []Warning
	pkgs := map[string][]goDecl{} // package name -> declarations
	var pkgOrder []string
	fset := token.NewFileSet()
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, files[name], parser.SkipObjectResolution)
		if err != nil {
			if changed[name] {
				p := path.Join(dir, name)
				out = append(out, Warning{
					Path:   p,
					Kind:   WarningParse,
					Detail: fmt.Sprintf("%s does not parse: %s", p, parseErrorText(err)),
				})
			}
			continue
		}
		if ok, err := bc.MatchFile(dir, name); err != nil || !ok {
			continue // excluded by build constraints on linux/amd64
		}
		pkg := f.Name.Name
		if _, seen := pkgs[pkg]; !seen {
			pkgOrder = append(pkgOrder, pkg)
		}
		pkgs[pkg] = append(pkgs[pkg], fileDecls(f, name)...)
	}
	sort.Strings(pkgOrder)
	for _, pkg := range pkgOrder {
		out = append(out, duplicates(shown, pkg, pkgs[pkg], changed)...)
	}
	return out, nil
}

// fileDecls lists the top-level declarations of f that must be unique in
// its package. Blank names and init funcs may repeat and are left out.
func fileDecls(f *ast.File, file string) []goDecl {
	var out []goDecl
	add := func(name, kind string) {
		if name == "_" || name == "" {
			return
		}
		out = append(out, goDecl{key: name, kind: kind, file: file})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				if d.Name.Name != "init" {
					add(d.Name.Name, "func")
				}
				continue
			}
			if d.Name.Name == "_" {
				continue
			}
			if recv := recvTypeName(d.Recv); recv != "" {
				out = append(out, goDecl{key: recv + "." + d.Name.Name, kind: "method", file: file})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					add(spec.Name.Name, "type")
				case *ast.ValueSpec:
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					for _, n := range spec.Names {
						add(n.Name, kind)
					}
				}
			}
		}
	}
	return out
}

// recvTypeName returns the base type name of a method receiver: T for
// T, *T, T[K] and *T[K, V].
func recvTypeName(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	expr := fl.List[0].Type
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
}

func duplicates(dir, pkg string, decls []goDecl, changed map[string]bool) []Warning {
	byKey := map[string][]goDecl{}
	var keys []string
	for _, d := range decls {
		if _, ok := byKey[d.key]; !ok {
			keys = append(keys, d.key)
		}
		byKey[d.key] = append(byKey[d.key], d)
	}
	sort.Strings(keys)
	var out []Warning
	for _, k := range keys {
		ds := byKey[k]
		if len(ds) < 2 {
			continue
		}
		touched := false
		kind := ds[0].kind
		fileSet := map[string]bool{}
		for _, d := range ds {
			touched = touched || changed[d.file]
			if d.kind != kind {
				kind = "declaration"
			}
			fileSet[d.file] = true
		}
		if !touched {
			continue
		}
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)
		where := dir
		if strings.HasSuffix(pkg, "_test") {
			where += " (package " + pkg + ")"
		}
		out = append(out, Warning{
			Path:   dir,
			Kind:   WarningDuplicate,
			Name:   k,
			Decl:   kind,
			Files:  files,
			Detail: fmt.Sprintf("duplicate %s %s in %s (%s)", kind, k, where, strings.Join(files, ", ")),
		})
	}
	return out
}

// parseErrorText returns the first syntax error as "line N: message".
func parseErrorText(err error) string {
	var list scanner.ErrorList
	if errors.As(err, &list) && len(list) > 0 {
		return fmt.Sprintf("line %d: %s", list[0].Pos.Line, list[0].Msg)
	}
	return err.Error()
}
