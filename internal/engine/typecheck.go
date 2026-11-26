package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

const (
	// WarningUnreachable: after a merge that combined both sides in a Go
	// package, a statement follows a return, panic, branch or endless loop
	// (for example one side's early return before the other side's new
	// code), and neither side had that.
	WarningUnreachable WarningKind = "unreachable"
	// WarningUndefined: after such a merge, code uses a name the package
	// does not declare, and neither side had that.
	WarningUndefined WarningKind = "undefined"
	// WarningUnusedImport: after such a merge, a file imports a package it
	// no longer uses, and neither side had that.
	WarningUnusedImport WarningKind = "unused-import"
)

// Bounds that keep CheckGoMerge fast: packages beyond them are not checked.
const (
	maxMergeCheckPackages = 12
	maxMergeCheckBytes    = 4 << 20 // Go source per package and tree
)

// CheckGoMerge type-checks the Go packages that both sides of a merge
// changed (a .go file of the same directory on each side), in process with
// go/types and without the go tool, and reports what the merged tree has
// that neither side had:
//
//   - unreachable code (a statement after a return, panic, goto, break,
//     continue or endless loop in the same block);
//   - undefined names (fields and methods are not checked: a type that
//     embeds one from another package would look like it lacks them);
//   - imports a file no longer uses.
//
// A problem counts when the merged package has it more often than either
// side's version of the package (compared by file, kind and message, not
// line, since a merge moves lines), so problems that predate the merge are
// never reported. Imports are not resolved (each is an empty package of the
// name its path suggests): names from other packages are not checked, and
// a package with a dot import is not checked for undefined names. Build constraints are evaluated for linux/amd64 as in
// CheckGo. Packages whose files do not parse (conflict markers) are
// skipped, and at most maxMergeCheckPackages packages of at most
// maxMergeCheckBytes of source are checked.
func CheckGoMerge(ctx context.Context, s blob.Store, sd GoSides) ([]Warning, error) {
	if sd.Merged.IsZero() || sd.Base == sd.Ours || sd.Base == sd.Theirs {
		return nil, nil
	}
	oc, err := object.DiffTrees(ctx, s, sd.Base, sd.Ours)
	if err != nil {
		return nil, err
	}
	tc, err := object.DiffTrees(ctx, s, sd.Base, sd.Theirs)
	if err != nil {
		return nil, err
	}
	dirsOf := func(cs []object.Change) map[string]bool {
		out := map[string]bool{}
		for _, c := range goChanges(cs) {
			d := path.Dir(c.Path)
			if d == "." {
				d = ""
			}
			out[d] = true
		}
		return out
	}
	od, td := dirsOf(oc), dirsOf(tc)
	var dirs []string
	for d := range od {
		if td[d] {
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	if len(dirs) > maxMergeCheckPackages {
		dirs = dirs[:maxMergeCheckPackages]
	}
	var out []Warning
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		merged, ok, err := goDirProblems(ctx, s, sd.Merged, dir)
		if err != nil {
			return nil, err
		}
		if !ok || len(merged) == 0 {
			continue
		}
		ours, okO, err := goDirProblems(ctx, s, sd.Ours, dir)
		if err != nil {
			return nil, err
		}
		theirs, okT, err := goDirProblems(ctx, s, sd.Theirs, dir)
		if err != nil {
			return nil, err
		}
		if !okO || !okT {
			continue // a side does not parse: no baseline to compare with
		}
		before := map[string]int{}
		for _, side := range [][]goProblem{ours, theirs} {
			n := map[string]int{}
			for _, p := range side {
				n[p.key()]++
			}
			for k, c := range n {
				before[k] = max(before[k], c)
			}
		}
		for _, p := range merged {
			k := p.key()
			if before[k] > 0 {
				before[k]--
				continue
			}
			out = append(out, p.warning())
		}
	}
	return out, nil
}

// goProblem is one problem in one file of a package.
type goProblem struct {
	file string // path in the tree
	line int
	kind WarningKind
	name string // undefined: the name; unused-import: the import path; unreachable: the function
	msg  string // without position
}

func (p goProblem) key() string { return p.file + "\x00" + string(p.kind) + "\x00" + p.msg }

func (p goProblem) warning() Warning {
	return Warning{
		Path: p.file, Kind: p.kind, Name: p.name, Files: []string{p.file},
		Detail: fmt.Sprintf("%s:%d: %s after the merge (neither side had it; both changed this package)", p.file, p.line, p.msg),
	}
}

var (
	unusedImport = regexp.MustCompile(`^("[^"]+") imported (?:as \S+ )?and not used$`)
)

// goDirProblems type-checks the Go packages of dir in tree. ok is false
// when the directory is gone, too big, or a file does not parse.
func goDirProblems(ctx context.Context, s blob.Store, tree hash.Hash, dir string) ([]goProblem, bool, error) {
	treeHash := tree
	if dir != "" {
		e, ok, err := object.Lookup(ctx, s, tree, dir)
		if err != nil {
			return nil, false, err
		}
		if !ok || e.Mode != object.ModeDir {
			return nil, false, nil
		}
		treeHash = e.Hash
	}
	t, err := object.GetTree(ctx, s, treeHash)
	if err != nil {
		return nil, false, err
	}
	files := map[string][]byte{}
	var names []string
	size := 0
	for _, e := range t.Entries {
		if (e.Mode != object.ModeFile && e.Mode != object.ModeExec) || !strings.HasSuffix(e.Name, ".go") {
			continue
		}
		data, err := blob.GetKind(ctx, s, hash.KindBlob, e.Hash)
		if err != nil {
			return nil, false, err
		}
		if size += len(data); size > maxMergeCheckBytes {
			return nil, false, nil
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
	fset := token.NewFileSet()
	pkgs := map[string][]*ast.File{}
	var order []string
	for _, name := range names {
		if ok, err := bc.MatchFile(dir, name); err != nil || !ok {
			continue
		}
		f, err := parser.ParseFile(fset, path.Join(dir, name), files[name], parser.SkipObjectResolution)
		if err != nil {
			return nil, false, nil
		}
		pkg := f.Name.Name
		if _, seen := pkgs[pkg]; !seen {
			order = append(order, pkg)
		}
		pkgs[pkg] = append(pkgs[pkg], f)
	}
	sort.Strings(order)
	var out []goProblem
	for _, pkg := range order {
		out = append(out, checkGoPackage(fset, pkg, pkgs[pkg])...)
	}
	return out, true, nil
}

// emptyImports resolves every import to an empty package of the name the
// import path suggests, so go/types still sees which imports a file uses
// (an import that fails to resolve is never reported as unused) while
// uses of their members (undefined: pkg.Name) are left out by the caller.
type emptyImports map[string]*types.Package

func (m emptyImports) Import(p string) (*types.Package, error) {
	if pkg, ok := m[p]; ok {
		return pkg, nil
	}
	pkg := types.NewPackage(p, importName(p))
	pkg.MarkComplete()
	m[p] = pkg
	return pkg, nil
}

var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

// importName guesses the package name of an import path the way the go
// tool's conventions suggest: the last element, without a major version
// (/v2, .v3), a go- prefix or a -go suffix, and with other characters
// that cannot be in a name dropped.
func importName(p string) string {
	parts := strings.Split(p, "/")
	name := parts[len(parts)-1]
	if majorVersion.MatchString(name) && len(parts) > 1 {
		name = parts[len(parts)-2]
	}
	if i := strings.Index(name, ".v"); i > 0 && majorVersion.MatchString(name[i+1:]) {
		name = name[:i]
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "go-"), "-go")
	var b strings.Builder
	for _, r := range name {
		if r == '_' || unicode.IsLetter(r) || (unicode.IsDigit(r) && b.Len() > 0) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "pkg"
	}
	return b.String()
}

func checkGoPackage(fset *token.FileSet, name string, files []*ast.File) []goProblem {
	dotImport := false
	for _, f := range files {
		for _, im := range f.Imports {
			if im.Name != nil && im.Name.Name == "." {
				dotImport = true
			}
		}
	}
	var out []goProblem
	conf := types.Config{
		Importer:    emptyImports{},
		FakeImportC: true,
		Error: func(err error) {
			var te types.Error
			if !errors.As(err, &te) {
				return
			}
			pos := fset.Position(te.Pos)
			p := goProblem{file: pos.Filename, line: pos.Line}
			switch {
			case strings.HasPrefix(te.Msg, "undefined: "):
				name := strings.TrimPrefix(te.Msg, "undefined: ")
				if dotImport || strings.Contains(name, ".") {
					return // a member of an imported package: not resolved
				}
				p.kind, p.name, p.msg = WarningUndefined, name, te.Msg
			case unusedImport.MatchString(te.Msg):
				p.kind, p.name, p.msg = WarningUnusedImport, strings.Trim(unusedImport.FindStringSubmatch(te.Msg)[1], `"`), te.Msg
			default:
				return
			}
			out = append(out, p)
		},
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	_, _ = conf.Check(name, fset, files, info) // errors go to conf.Error
	for _, f := range files {
		out = append(out, unreachableIn(fset, f, info)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

// unreachableIn finds the first unreachable statement of every statement
// list in f's functions, as go vet's unreachable check does: a statement
// after a terminating one (return, goto, panic, a block or if/else, for,
// switch or select that cannot end normally) or after break, continue or
// fallthrough. Labeled statements may be jumped to and so count as
// reachable.
func unreachableIn(fset *token.FileSet, f *ast.File, info *types.Info) []goProblem {
	var out []goProblem
	file := fset.Position(f.Pos()).Filename
	var fn string
	var visit func(n ast.Node) bool
	list := func(stmts []ast.Stmt) {
		for i, st := range stmts {
			if i == len(stmts)-1 || !deadAfter(st, info) {
				continue
			}
			next := stmts[i+1]
			if _, ok := next.(*ast.EmptyStmt); ok {
				continue
			}
			if _, ok := next.(*ast.LabeledStmt); ok {
				continue
			}
			out = append(out, goProblem{file: file, line: fset.Position(next.Pos()).Line, kind: WarningUnreachable, name: fn,
				msg: "unreachable code in " + fn})
			break
		}
	}
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BlockStmt:
			list(n.List)
		case *ast.CaseClause:
			list(n.Body)
		case *ast.CommClause:
			list(n.Body)
		}
		return true
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fn = fd.Name.Name
		if r := recvTypeName(fd.Recv); r != "" {
			fn = r + "." + fn
		}
		ast.Inspect(fd.Body, visit) // function literals inside count as fn's
	}
	return out
}

// deadAfter reports whether control never reaches the statement after st
// in the same list.
func deadAfter(st ast.Stmt, info *types.Info) bool {
	if b, ok := st.(*ast.BranchStmt); ok {
		return b.Tok == token.BREAK || b.Tok == token.CONTINUE || b.Tok == token.GOTO || b.Tok == token.FALLTHROUGH
	}
	return terminating(st, info, "")
}

// terminating is the Go spec's terminating statement, for st with the
// given label ("" for none).
func terminating(st ast.Stmt, info *types.Info, label string) bool {
	switch s := st.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return s.Tok == token.GOTO || s.Tok == token.FALLTHROUGH
	case *ast.ExprStmt:
		call, ok := s.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := ast.Unparen(call.Fun).(*ast.Ident)
		if !ok || id.Name != "panic" {
			return false
		}
		_, builtin := info.Uses[id].(*types.Builtin)
		return builtin
	case *ast.BlockStmt:
		return len(s.List) > 0 && terminating(s.List[len(s.List)-1], info, "")
	case *ast.IfStmt:
		return s.Else != nil && terminating(s.Body, info, "") && terminating(s.Else, info, "")
	case *ast.LabeledStmt:
		return terminating(s.Stmt, info, s.Label.Name)
	case *ast.ForStmt:
		return s.Cond == nil && !hasBreak(s.Body, label, true)
	case *ast.SwitchStmt:
		return clausesTerminate(s.Body, info, label)
	case *ast.TypeSwitchStmt:
		return clausesTerminate(s.Body, info, label)
	case *ast.SelectStmt:
		for _, c := range s.Body.List {
			cc := c.(*ast.CommClause)
			if hasBreakList(cc.Body, label, true) || !endsTerminating(cc.Body, info) {
				return false
			}
		}
		return true
	}
	return false
}

// clausesTerminate: a switch with a default whose every clause ends in a
// terminating statement (or fallthrough) and has no break for it.
func clausesTerminate(body *ast.BlockStmt, info *types.Info, label string) bool {
	hasDefault := false
	for _, c := range body.List {
		cc := c.(*ast.CaseClause)
		if cc.List == nil {
			hasDefault = true
		}
		if hasBreakList(cc.Body, label, true) || !endsTerminating(cc.Body, info) {
			return false
		}
	}
	return hasDefault
}

func endsTerminating(list []ast.Stmt, info *types.Info) bool {
	return len(list) > 0 && terminating(list[len(list)-1], info, "")
}

func hasBreakList(list []ast.Stmt, label string, implicit bool) bool {
	// In a block, so a loop at the top of the list counts as nested.
	return hasBreak(&ast.BlockStmt{List: list}, label, implicit)
}

// hasBreak reports whether st contains a break that ends the enclosing
// statement: one with its label, or (implicit) an unlabeled one not inside
// a nested for, switch or select.
func hasBreak(st ast.Node, label string, implicit bool) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.BranchStmt:
			if n.Tok == token.BREAK && ((n.Label == nil && implicit) || (n.Label != nil && n.Label.Name == label && label != "")) {
				found = true
			}
		case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			if n != st {
				if label != "" {
					found = hasBreak(n.(ast.Stmt), label, false) || found
				}
				return false
			}
		}
		return true
	})
	return found
}

// WithoutCovered returns typed (CheckGoMerge warnings) without the
// undefined names that a removed warning of CheckGoSides already explains
// (same file and name), so one problem is reported once, by the warning
// that says more.
func WithoutCovered(sides, typed []Warning) []Warning {
	covered := map[string]bool{}
	for _, w := range sides {
		if w.Kind != WarningRemoved {
			continue
		}
		name := w.Name
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		covered[w.Path+"\x00"+name] = true
	}
	out := typed[:0:0]
	for _, w := range typed {
		if w.Kind == WarningUndefined && covered[w.Path+"\x00"+w.Name] {
			continue
		}
		out = append(out, w)
	}
	return out
}
