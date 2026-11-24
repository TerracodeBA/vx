package engine

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
)

const (
	// WarningSignature: one side changed the parameters or results of a
	// function or method, and the other side added or changed calls that
	// still use the old shape.
	WarningSignature WarningKind = "signature"
	// WarningRemoved: one side removed or renamed a top-level name that
	// code the other side added or changed still uses.
	WarningRemoved WarningKind = "removed"
	// WarningMergedByDecl: a Go file whose lines conflicted was merged by
	// declaration (ship plans, rebases and forecasts report it this way;
	// lands and syncs list the files).
	WarningMergedByDecl WarningKind = "merged-by-declaration"
)

// MergedByDeclWarnings turns files merged by declaration into warnings,
// sorted by path, for the surfaces that have no list of merged files.
func MergedByDeclWarnings(paths []string) []Warning {
	seen := map[string]bool{}
	var out []Warning
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, Warning{Path: p, Kind: WarningMergedByDecl, Detail: fmt.Sprintf("%s merged by declaration: the lines conflicted, but both sides changed different declarations; worth a look", p)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// GoSides are the trees of a three-way merge and its result, for
// CheckGoSides. The names finish "changed in ..." and "added by ...", e.g.
// "this land" and "the effort".
type GoSides struct {
	Base, Ours, Theirs, Merged hash.Hash
	OursName, TheirsName       string
}

// CheckGoSides looks for semantic collisions between the two sides of a
// merge in Go code, where one side's change and the other side's new code
// meet:
//
//   - signature: a function or method whose parameters or results one side
//     changed, while the other side added or changed calls that still have
//     the old shape, in the same package or in a package that imports it
//     within the tree;
//   - removed: a top-level name one side removed or renamed (it is gone
//     from the merged package) that code the other side added or changed
//     still uses.
//
// It is textual, without a type checker, and tuned to stay quiet when in
// doubt (docs/agents.md lists the heuristics):
//
//   - Only packages one side changed are searched for changed
//     declarations, and only files the other side changed for uses; a use
//     counts when it is on a line the other side contributed to the merged
//     file (a line of the merged file that the first side's version of the
//     file does not have).
//   - Signatures are compared by their type parameters and parameter and
//     result types as written (names do not matter), and the change must
//     have survived the merge.
//   - A call to a changed function is reported when its argument count
//     fits the old signature and not the new one, when the result count
//     changed and the call's value is used, or, when only parameter types
//     changed, when an argument at a changed position is not a literal.
//   - Methods are matched by name on any receiver, only for calls in the
//     method's own package or a package that imports it, and only when the
//     argument count stops fitting; removed methods are not reported.
//   - A name in the same package counts only where the parser leaves it
//     unresolved (not a local variable, parameter or field); one in another
//     package only as PKG.Name with PKG naming the import.
//
// Files that do not parse (conflict markers) are skipped; build
// constraints are not evaluated.
func CheckGoSides(ctx context.Context, s blob.Store, sd GoSides) ([]Warning, error) {
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
	ours, theirs := goChanges(oc), goChanges(tc)
	if len(ours) == 0 || len(theirs) == 0 {
		return nil, nil
	}
	c := &semCheck{ctx: ctx, s: s, sd: sd, mods: map[string]string{}}
	var out []Warning
	for _, dir := range []struct {
		side, other           hash.Hash
		changes, otherChanges []object.Change
		sideName, otherName   string
	}{
		{sd.Ours, sd.Theirs, ours, theirs, sd.OursName, sd.TheirsName},
		{sd.Theirs, sd.Ours, theirs, ours, sd.TheirsName, sd.OursName},
	} {
		targets, err := c.targets(dir.side, dir.changes)
		if err != nil {
			return nil, err
		}
		if len(targets) == 0 {
			continue
		}
		ws, err := c.uses(targets, dir.side, dir.otherChanges, dir.sideName, dir.otherName)
		if err != nil {
			return nil, err
		}
		out = append(out, ws...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Detail < out[j].Detail
	})
	return out, nil
}

// goChanges keeps the changes of .go files the go tool would build.
func goChanges(cs []object.Change) []object.Change {
	var out []object.Change
	for _, c := range cs {
		dir := path.Dir(c.Path)
		if dir == "." {
			dir = ""
		}
		if strings.HasSuffix(c.Path, ".go") && !ignoredGoDir(dir) {
			out = append(out, c)
		}
	}
	return out
}

type semCheck struct {
	ctx  context.Context
	s    blob.Store
	sd   GoSides
	mods map[string]string // dir -> import path ("" when unknown)
}

// sig is a function's shape as written.
type sig struct {
	typeParams string
	params     []string // types, one per parameter
	results    []string
	variadic   bool
	display    string // "(ctx context.Context, user string)"
	resultText string // "(string, error)", "error", ""
}

func (a sig) equal(b sig) bool {
	return a.typeParams == b.typeParams && strings.Join(a.params, ",") == strings.Join(b.params, ",") &&
		strings.Join(a.results, ",") == strings.Join(b.results, ",") && a.variadic == b.variadic
}

// fits reports whether a call with n arguments (and ... when spread) can
// match the parameters.
func (a sig) fits(n int, spread bool) bool {
	if spread {
		return a.variadic && n == len(a.params)
	}
	if a.variadic {
		return n >= len(a.params)-1
	}
	return n == len(a.params)
}

// pkgDecl is a top-level declaration of a package, for CheckGoSides.
type pkgDecl struct {
	kind string // func, method, type, var, const
	file string // path in the tree
	sig  *sig   // funcs and methods
}

// semTarget is a declaration one side changed or removed.
type semTarget struct {
	dir, pkg   string // where it is declared
	name       string // F, or T.M for methods
	kind       string
	removed    bool
	old, new   *sig
	file       string // declaring file: in the side for changes, in base for removals
	renamedTo  string // a new declaration of the same kind and shape, for removals
	methodName string
}

// targets lists the declarations side changed the signature of, or
// removed, in the packages it changed, as they stand in the merged tree.
func (c *semCheck) targets(side hash.Hash, changes []object.Change) ([]semTarget, error) {
	dirs := map[string]bool{}
	for _, ch := range changes {
		dirs[path.Dir(ch.Path)] = true
	}
	order := make([]string, 0, len(dirs))
	for d := range dirs {
		order = append(order, d)
	}
	sort.Strings(order)
	var out []semTarget
	for _, dir := range order {
		base, err := c.pkgDecls(c.sd.Base, dir)
		if err != nil {
			return nil, err
		}
		if len(base) == 0 {
			continue
		}
		mine, err := c.pkgDecls(side, dir)
		if err != nil {
			return nil, err
		}
		merged, err := c.pkgDecls(c.sd.Merged, dir)
		if err != nil {
			return nil, err
		}
		pkgs := make([]string, 0, len(base))
		for p := range base {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		for _, pkg := range pkgs {
			bd, sdd, md := base[pkg], mine[pkg], merged[pkg]
			if sdd == nil || md == nil {
				continue // the whole package went away or was renamed: too broad to report
			}
			names := make([]string, 0, len(bd))
			for n := range bd {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				b := bd[n]
				now, inSide := sdd[n]
				m, inMerged := md[n]
				switch {
				case !inSide && !inMerged:
					if b.kind == "method" {
						continue
					}
					t := semTarget{dir: dir, pkg: pkg, name: n, kind: b.kind, removed: true, file: b.file, old: b.sig}
					t.renamedTo = renameOf(b, bd, sdd)
					out = append(out, t)
				case inSide && inMerged && b.sig != nil && now.sig != nil && m.sig != nil && !b.sig.equal(*now.sig) && m.sig.equal(*now.sig):
					t := semTarget{dir: dir, pkg: pkg, name: n, kind: b.kind, file: now.file, old: b.sig, new: now.sig}
					if b.kind == "method" {
						t.methodName = n[strings.LastIndexByte(n, '.')+1:]
					}
					out = append(out, t)
				}
			}
		}
	}
	return out, nil
}

// renameOf finds the one declaration side added with the same kind and,
// for functions, the same shape as the removed b: a likely rename.
func renameOf(b pkgDecl, base, side map[string]pkgDecl) string {
	found := ""
	for n, d := range side {
		if _, old := base[n]; old || d.kind != b.kind {
			continue
		}
		if (b.sig == nil) != (d.sig == nil) || (b.sig != nil && !b.sig.equal(*d.sig)) {
			continue
		}
		if found != "" {
			return ""
		}
		found = n
	}
	return found
}

// pkgDecls parses the .go files of dir in root: package name -> name ->
// declaration. Files that do not parse are skipped.
func (c *semCheck) pkgDecls(root hash.Hash, dir string) (map[string]map[string]pkgDecl, error) {
	files, err := c.goFiles(root, dir)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]pkgDecl{}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, files[name], parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		pkg := f.Name.Name
		if out[pkg] == nil {
			out[pkg] = map[string]pkgDecl{}
		}
		p := path.Join(dir, name)
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "_" || (d.Recv == nil && d.Name.Name == "init") {
					continue
				}
				key, kind := d.Name.Name, "func"
				if d.Recv != nil {
					recv := recvTypeName(d.Recv)
					if recv == "" {
						continue
					}
					key, kind = recv+"."+d.Name.Name, "method"
				}
				sg := funcSig(fset, d.Type)
				out[pkg][key] = pkgDecl{kind: kind, file: p, sig: &sg}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						if spec.Name.Name != "_" {
							out[pkg][spec.Name.Name] = pkgDecl{kind: "type", file: p}
						}
					case *ast.ValueSpec:
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						for _, n := range spec.Names {
							if n.Name != "_" {
								out[pkg][n.Name] = pkgDecl{kind: kind, file: p}
							}
						}
					}
				}
			}
		}
	}
	return out, nil
}

// goFiles reads the .go files directly in dir of root.
func (c *semCheck) goFiles(root hash.Hash, dir string) (map[string][]byte, error) {
	treeHash := root
	if dir != "." && dir != "" {
		e, ok, err := object.Lookup(c.ctx, c.s, root, dir)
		if err != nil {
			return nil, err
		}
		if !ok || e.Mode != object.ModeDir {
			return nil, nil
		}
		treeHash = e.Hash
	}
	t, err := object.GetTree(c.ctx, c.s, treeHash)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range t.Entries {
		if (e.Mode != object.ModeFile && e.Mode != object.ModeExec) || !strings.HasSuffix(e.Name, ".go") {
			continue
		}
		data, err := blob.GetKind(c.ctx, c.s, hash.KindBlob, e.Hash)
		if err != nil {
			return nil, err
		}
		files[e.Name] = data
	}
	return files, nil
}

func funcSig(fset *token.FileSet, ft *ast.FuncType) sig {
	text := func(n ast.Node) string {
		var b bytes.Buffer
		_ = printer.Fprint(&b, fset, n)
		return b.String()
	}
	// FieldList is not a node go/printer prints on its own.
	fields := func(fl *ast.FieldList) string {
		var parts []string
		for _, f := range fl.List {
			var names []string
			for _, id := range f.Names {
				names = append(names, id.Name)
			}
			t := text(f.Type)
			if len(names) > 0 {
				t = strings.Join(names, ", ") + " " + t
			}
			parts = append(parts, t)
		}
		return strings.Join(parts, ", ")
	}
	var s sig
	if ft.TypeParams != nil {
		s.typeParams = "[" + fields(ft.TypeParams) + "]"
	}
	var shown []string
	if ft.Params != nil {
		for _, f := range ft.Params.List {
			t := text(f.Type)
			if _, ok := f.Type.(*ast.Ellipsis); ok {
				s.variadic = true
			}
			n := len(f.Names)
			if n == 0 {
				n = 1
				shown = append(shown, t)
			} else {
				var names []string
				for _, id := range f.Names {
					names = append(names, id.Name)
				}
				shown = append(shown, strings.Join(names, ", ")+" "+t)
			}
			for i := 0; i < n; i++ {
				s.params = append(s.params, t)
			}
		}
	}
	s.display = "(" + strings.Join(shown, ", ") + ")"
	if ft.Results != nil {
		for _, f := range ft.Results.List {
			t := text(f.Type)
			n := len(f.Names)
			if n == 0 {
				n = 1
			}
			for i := 0; i < n; i++ {
				s.results = append(s.results, t)
			}
		}
		s.resultText = "(" + fields(ft.Results) + ")"
		if len(ft.Results.List) == 1 && len(ft.Results.List[0].Names) == 0 {
			s.resultText = text(ft.Results.List[0].Type)
		}
	}
	return s
}

// importPath returns the import path of dir in the merged tree, from the
// nearest go.mod above it; "" when there is none.
func (c *semCheck) importPath(dir string) (string, error) {
	if p, ok := c.mods[dir]; ok {
		return p, nil
	}
	for d := dir; ; d = path.Dir(d) {
		mod := "go.mod"
		if d != "." && d != "" {
			mod = d + "/go.mod"
		}
		e, ok, err := object.Lookup(c.ctx, c.s, c.sd.Merged, mod)
		if err != nil {
			return "", err
		}
		if ok && e.Mode != object.ModeDir {
			data, err := blob.GetKind(c.ctx, c.s, hash.KindBlob, e.Hash)
			if err != nil {
				return "", err
			}
			p := ""
			if m := moduleLine(data); m != "" {
				p = m
				if rel := strings.TrimPrefix(strings.TrimPrefix(dir, d), "/"); dir != d && rel != "" {
					p = m + "/" + rel
				}
			}
			c.mods[dir] = p
			return p, nil
		}
		if d == "." || d == "" || d == "/" {
			break
		}
	}
	c.mods[dir] = ""
	return "", nil
}

func moduleLine(data []byte) string {
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(l, "module"); ok && (strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "\t")) {
			m := strings.TrimSpace(rest)
			if u, err := strconv.Unquote(m); err == nil {
				m = u
			}
			return m
		}
	}
	return ""
}

// uses finds code the other side contributed that uses targets the old
// way. side is the tree whose changes made the targets.
func (c *semCheck) uses(targets []semTarget, side hash.Hash, otherChanges []object.Change, sideName, otherName string) ([]Warning, error) {
	var out []Warning
	for _, ch := range otherChanges {
		if ch.Op == object.OpDelete {
			continue
		}
		e, ok, err := object.Lookup(c.ctx, c.s, c.sd.Merged, ch.Path)
		if err != nil {
			return nil, err
		}
		if !ok || e.Mode == object.ModeDir || e.Mode == object.ModeSymlink {
			continue
		}
		merged, err := blob.GetKind(c.ctx, c.s, hash.KindBlob, e.Hash)
		if err != nil {
			return nil, err
		}
		var mine []byte
		if se, ok, err := object.Lookup(c.ctx, c.s, side, ch.Path); err != nil {
			return nil, err
		} else if ok && se.Mode != object.ModeDir {
			if mine, err = blob.GetKind(c.ctx, c.s, hash.KindBlob, se.Hash); err != nil {
				return nil, err
			}
		}
		added := addedLines(mine, merged)
		if len(added) == 0 {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, ch.Path, merged, 0)
		if err != nil {
			continue
		}
		ws, err := c.fileUses(fset, f, ch.Path, merged, added, targets, sideName, otherName)
		if err != nil {
			return nil, err
		}
		out = append(out, ws...)
	}
	return out, nil
}

// addedLines returns the 1-based line numbers of merged that mine does not
// have: what the other side contributed.
func addedLines(mine, merged []byte) map[int]bool {
	out := map[int]bool{}
	for _, e := range merge.Diff(merge.SplitLines(mine), merge.SplitLines(merged)) {
		if e.Kind == merge.Insert {
			for l := e.B0; l < e.B1; l++ {
				out[l+1] = true
			}
		}
	}
	return out
}

func (c *semCheck) fileUses(fset *token.FileSet, f *ast.File, file string, src []byte, added map[int]bool, targets []semTarget, sideName, otherName string) ([]Warning, error) {
	dir := path.Dir(file)
	// What this file calls each target's package: "" for its own package.
	type ref struct {
		t     semTarget
		local string
	}
	imports := map[string]string{} // import path -> local name
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := ""
		if imp.Name != nil {
			local = imp.Name.Name
		}
		imports[p] = local
	}
	var refs []ref
	for _, t := range targets {
		if t.dir == dir && t.pkg == f.Name.Name {
			refs = append(refs, ref{t: t})
			continue
		}
		ip, err := c.importPath(t.dir)
		if err != nil {
			return nil, err
		}
		local, ok := imports[ip]
		if ip == "" || !ok || local == "_" || local == "." {
			continue
		}
		if local == "" {
			local = t.pkg
		}
		refs = append(refs, ref{t: t, local: local})
	}
	if len(refs) == 0 {
		return nil, nil
	}
	unresolved := map[*ast.Ident]bool{}
	for _, id := range f.Unresolved {
		unresolved[id] = true
	}
	line := func(n ast.Node) int { return fset.Position(n.Pos()).Line }
	snippet := func(n ast.Node) string {
		s := string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > 60 {
			s = s[:57] + "..."
		}
		return s
	}
	// matches reports whether expr names the target: Name in its own
	// package (unresolved, or this file's own top-level func), PKG.Name
	// elsewhere.
	matches := func(r ref, expr ast.Expr) bool {
		switch e := expr.(type) {
		case *ast.Ident:
			if r.local != "" || e.Name != r.t.name {
				return false
			}
			return unresolved[e] || (e.Obj != nil && e.Obj.Kind == ast.Fun && r.t.kind == "func" && !r.t.removed)
		case *ast.SelectorExpr:
			x, ok := e.X.(*ast.Ident)
			return ok && r.local != "" && x.Name == r.local && unresolved[x] && e.Sel.Name == r.t.name
		}
		return false
	}
	shown := func(t semTarget) string { return t.pkg + "." + t.name }
	seen := map[string]bool{}
	var out []Warning
	add := func(r ref, n ast.Node, kind WarningKind, detail string) {
		k := fmt.Sprintf("%s|%s|%d", shown(r.t), kind, line(n))
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, Warning{Path: file, Kind: kind, Name: shown(r.t), Decl: r.t.kind, Files: []string{r.t.file, file}, Detail: detail})
	}

	// Calls, for signature changes. valueUsed tracks calls whose results
	// are used (not a bare statement, go or defer).
	bare := map[*ast.CallExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.ExprStmt:
			if call, ok := s.X.(*ast.CallExpr); ok {
				bare[call] = true
			}
		case *ast.GoStmt:
			bare[s.Call] = true
		case *ast.DeferStmt:
			bare[s.Call] = true
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if !added[line(n)] {
				return true
			}
			fun := ast.Unparen(n.Fun)
			switch ix := fun.(type) {
			case *ast.IndexExpr:
				fun = ix.X
			case *ast.IndexListExpr:
				fun = ix.X
			}
			for _, r := range refs {
				if r.t.removed || r.t.new == nil {
					continue
				}
				var hit bool
				if r.t.kind == "method" {
					sel, ok := fun.(*ast.SelectorExpr)
					hit = ok && sel.Sel.Name == r.t.methodName && !isImportName(sel.X, unresolved, imports)
					if hit && !(r.t.old.fits(len(n.Args), n.Ellipsis.IsValid()) && !r.t.new.fits(len(n.Args), n.Ellipsis.IsValid())) {
						hit = false
					}
				} else if matches(r, fun) {
					hit = callBreaks(*r.t.old, *r.t.new, n, bare[n])
				}
				if hit {
					add(r, n, WarningSignature, fmt.Sprintf("%s now %s (changed in %s); %s:%d calls %s (added by %s)",
						shown(r.t), shapeText(*r.t.old, *r.t.new), sideName, file, line(n), snippet(n), otherName))
				}
			}
		case *ast.Ident, *ast.SelectorExpr:
			if !added[line(n)] {
				return true
			}
			for _, r := range refs {
				if !r.t.removed {
					continue
				}
				if expr, ok := n.(ast.Expr); ok && matches(r, expr) {
					what := "removed or renamed"
					if r.t.renamedTo != "" {
						what = "removed (renamed to " + r.t.pkg + "." + r.t.renamedTo + "?)"
					}
					add(r, n, WarningRemoved, fmt.Sprintf("%s %s was %s (in %s); %s:%d still uses it: %s (added by %s)",
						r.t.kind, shown(r.t), what, sideName, file, line(n), snippetLine(src, fset, n), otherName))
				}
			}
		}
		return true
	})
	return out, nil
}

// isImportName reports whether x names an imported package, so x.M is a
// package function, not a method call.
func isImportName(x ast.Expr, unresolved map[*ast.Ident]bool, imports map[string]string) bool {
	id, ok := x.(*ast.Ident)
	if !ok || !unresolved[id] {
		return false
	}
	for p, local := range imports {
		if local == id.Name || (local == "" && path.Base(p) == id.Name) {
			return true
		}
	}
	return false
}

// callBreaks decides whether a call written against old likely fails
// against new (see CheckGoSides).
func callBreaks(old, new sig, call *ast.CallExpr, bare bool) bool {
	n, spread := len(call.Args), call.Ellipsis.IsValid()
	if old.fits(n, spread) && !new.fits(n, spread) {
		return true
	}
	if len(old.results) != len(new.results) && !bare {
		return true
	}
	if len(old.params) == len(new.params) && old.variadic == new.variadic {
		for i := range old.params {
			if old.params[i] == new.params[i] || i >= n {
				continue
			}
			if !isLiteral(call.Args[i]) {
				return true
			}
		}
	}
	return false
}

func isLiteral(e ast.Expr) bool {
	switch e := ast.Unparen(e).(type) {
	case *ast.BasicLit, *ast.CompositeLit, *ast.FuncLit:
		return true
	case *ast.Ident:
		return e.Name == "nil" || e.Name == "true" || e.Name == "false" || e.Name == "iota"
	case *ast.UnaryExpr:
		return isLiteral(e.X)
	}
	return false
}

// shapeText says what changed: "takes (ctx context.Context, user
// string)", "returns (string, error)", or both.
func shapeText(old, new sig) string {
	var parts []string
	if old.typeParams != new.typeParams || strings.Join(old.params, ",") != strings.Join(new.params, ",") || old.variadic != new.variadic {
		tp := ""
		if new.typeParams != "" {
			tp = new.typeParams
		}
		parts = append(parts, "takes "+tp+new.display)
	}
	if strings.Join(old.results, ",") != strings.Join(new.results, ",") {
		r := new.resultText
		if r == "" {
			r = "nothing"
		}
		parts = append(parts, "returns "+r)
	}
	return strings.Join(parts, " and ")
}

// snippetLine is the source line of n, trimmed, for removed names (the
// name alone says little).
func snippetLine(src []byte, fset *token.FileSet, n ast.Node) string {
	off := fset.Position(n.Pos()).Offset
	start := bytes.LastIndexByte(src[:off], '\n') + 1
	end := bytes.IndexByte(src[off:], '\n')
	if end < 0 {
		end = len(src)
	} else {
		end += off
	}
	s := strings.TrimSpace(string(src[start:end]))
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}
