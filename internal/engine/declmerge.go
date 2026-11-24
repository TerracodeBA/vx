package engine

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
	"strings"

	"github.com/TerracodeBA/vx/internal/merge"
)

// Merging Go files by declaration.
//
// When the line merge of a .go file conflicts, mergeGoDecls retries one
// level up: it cuts base, ours and theirs into top-level declarations and
// merges those. A file is
//
//	header   build constraints, package doc and the package clause
//	imports  the import declarations
//	decls    one chunk per top-level declaration: the comments and blank
//	         lines before it, the declaration, and its line comment
//	tail     comments after the last declaration
//
// A declaration is keyed like CheckGo keys it (func name, Recv.Method, and
// the names of a type, var or const declaration), so a chunk only one side
// changed, added or deleted takes that side. The merge gives up, and the
// caller keeps the text conflict, when both sides changed the same
// declaration (or the header, or the tail) differently, when one side
// deleted a declaration the other changed, when a file does not parse, or
// when the result does not parse or loses a declaration.
//
// Imports are united: an import stays unless one side removed it, both
// sides' additions are kept, and an import one side removed comes back
// when the merged code still uses its package name. Files that import "C"
// are only merged when at most one side changed the imports, because the
// cgo preamble must stay right above its import.
//
// Declarations keep ours' order; a declaration only theirs added goes right
// after the declaration that precedes it in theirs (theirs first, as keep
// both does with lines). The result is gofmt'ed.

// goParts is a Go file cut into the pieces mergeGoDecls merges.
type goParts struct {
	pkg     string
	header  string
	imports string // raw text of the import declarations, with comments between them
	specs   []goImport
	keys    []string            // declaration keys in file order
	chunks  map[string]string   // key -> chunk text
	names   map[string][]string // key -> the names it declares (the key itself for blank decls and init funcs)
	tail    string
	fset    *token.FileSet
}

// goImport is one import spec.
type goImport struct {
	name, path string // name is empty when the import has none
	doc        string // the comment above the spec, raw
	comment    string // the line comment, raw
}

func (i goImport) key() string { return i.name + " " + i.path }

// local is the name the import is used by in code: its explicit name, or a
// guess from the path for unnamed imports (last element, without a "go-"
// prefix or a ".vN" suffix; for ".../vN" the element before).
func (i goImport) local() string {
	if i.name != "" {
		return i.name
	}
	parts := strings.Split(i.path, "/")
	last := parts[len(parts)-1]
	if len(parts) > 1 && isMajorVersion(last) {
		last = parts[len(parts)-2]
	}
	if dot := strings.Index(last, ".v"); dot > 0 && isMajorVersion(last[dot+1:]) {
		last = last[:dot]
	}
	last = strings.TrimPrefix(last, "go-")
	last = strings.TrimSuffix(last, "-go")
	last = strings.TrimSuffix(last, ".go")
	return strings.ReplaceAll(last, "-", "")
}

func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	_, err := strconv.Atoi(s[1:])
	return err == nil
}

// text renders the spec as a line of an import block.
func (i goImport) text() string {
	var b strings.Builder
	if i.doc != "" {
		for _, l := range strings.Split(i.doc, "\n") {
			b.WriteString("\t" + strings.TrimSpace(l) + "\n")
		}
	}
	b.WriteString("\t")
	if i.name != "" {
		b.WriteString(i.name + " ")
	}
	b.WriteString(strconv.Quote(i.path))
	if i.comment != "" {
		b.WriteString(" " + i.comment)
	}
	b.WriteString("\n")
	return b.String()
}

// splitGo cuts src into goParts. It fails on files that do not parse and
// on layouts it cannot cut along lines (two declarations on one line).
func splitGo(src []byte) (*goParts, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	tf := fset.File(f.Pos())
	off := func(p token.Pos) int { return tf.Offset(p) }
	lineEnd := func(o int) int {
		if i := bytes.IndexByte(src[o:], '\n'); i >= 0 {
			return o + i + 1
		}
		return len(src)
	}
	start := func(d ast.Decl) int {
		var doc *ast.CommentGroup
		switch d := d.(type) {
		case *ast.FuncDecl:
			doc = d.Doc
		case *ast.GenDecl:
			doc = d.Doc
		}
		if doc != nil {
			return off(doc.Pos())
		}
		return off(d.Pos())
	}
	p := &goParts{pkg: f.Name.Name, chunks: map[string]string{}, names: map[string][]string{}, fset: fset}
	pos := lineEnd(off(f.Name.End()))
	if len(f.Decls) > 0 && start(f.Decls[0]) < pos {
		return nil, fmt.Errorf("declaration on the package line")
	}
	p.header = string(src[:pos])

	i := 0
	importStart := pos
	for ; i < len(f.Decls); i++ {
		g, ok := f.Decls[i].(*ast.GenDecl)
		if !ok || g.Tok != token.IMPORT {
			break
		}
		end := lineEnd(off(g.End()))
		if i+1 < len(f.Decls) && start(f.Decls[i+1]) < end {
			return nil, fmt.Errorf("two declarations on one line")
		}
		for _, s := range g.Specs {
			s := s.(*ast.ImportSpec)
			path, err := strconv.Unquote(s.Path.Value)
			if err != nil {
				return nil, err
			}
			imp := goImport{path: path}
			if s.Name != nil {
				imp.name = s.Name.Name
			}
			if s.Doc != nil {
				imp.doc = string(src[off(s.Doc.Pos()):off(s.Doc.End())])
			} else if g.Lparen == token.NoPos && g.Doc != nil {
				imp.doc = string(src[off(g.Doc.Pos()):off(g.Doc.End())])
			}
			if s.Comment != nil {
				imp.comment = string(src[off(s.Comment.Pos()):off(s.Comment.End())])
			}
			p.specs = append(p.specs, imp)
		}
		pos = end
	}
	p.imports = string(src[importStart:pos])

	seen := map[string]int{}
	for ; i < len(f.Decls); i++ {
		d := f.Decls[i]
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			return nil, fmt.Errorf("import after a declaration")
		}
		end := lineEnd(off(d.End()))
		if i+1 < len(f.Decls) && start(f.Decls[i+1]) < end {
			return nil, fmt.Errorf("two declarations on one line")
		}
		chunk := string(src[pos:end])
		k := declKey(d, func(n ast.Node) string { return string(src[off(n.Pos()):off(n.End())]) })
		if k == "" {
			return nil, fmt.Errorf("declaration without a key")
		}
		if n := seen[k]; n > 0 {
			seen[k]++
			k = fmt.Sprintf("%s#%d", k, n)
		} else {
			seen[k] = 1
		}
		p.keys = append(p.keys, k)
		p.chunks[k] = chunk
		p.names[k] = declNames(d, k)
		pos = end
	}
	p.tail = string(src[pos:])
	return p, nil
}

// declKey names a top-level declaration: "func F", "method T.M", "type T",
// "var a,b", "const A,B". Declarations of blank names are keyed by their
// text, since they can repeat; init funcs repeat too and get an ordinal
// from splitGo.
func declKey(d ast.Decl, text func(ast.Node) string) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			return "func " + d.Name.Name
		}
		recv := recvTypeName(d.Recv)
		if recv == "" {
			return ""
		}
		if d.Name.Name == "_" {
			return "method " + recv + "._" + blankSep + text(d)
		}
		return "method " + recv + "." + d.Name.Name
	case *ast.GenDecl:
		var names []string
		blank := true
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
				blank = blank && s.Name.Name == "_"
			case *ast.ValueSpec:
				for _, n := range s.Names {
					names = append(names, n.Name)
					blank = blank && n.Name == "_"
				}
			}
		}
		k := d.Tok.String() + " " + strings.Join(names, ",")
		if blank {
			k += blankSep + text(d)
		}
		return k
	}
	return ""
}

// declNames lists the names a declaration declares, for telling apart
// edits to different declarations from edits that only look different
// because a var or const group was extended. Blank declarations and init
// funcs declare their key.
func declNames(d ast.Decl, key string) []string {
	var out []string
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil && d.Name.Name != "init" && d.Name.Name != "_" {
			out = append(out, d.Name.Name)
		} else if d.Recv != nil && d.Name.Name != "_" {
			out = append(out, recvTypeName(d.Recv)+"."+d.Name.Name)
		}
	case *ast.GenDecl:
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				if s.Name.Name != "_" {
					out = append(out, s.Name.Name)
				}
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.Name != "_" {
						out = append(out, n.Name)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		out = []string{key}
	}
	return out
}

// renames maps base declarations side removed to a declaration side
// added that looks like the same one under a new name: same kind, and at
// least half of the lines equal once the names are blanked out.
func renames(b, side *goParts) map[string]string {
	out := map[string]string{}
	for _, k := range b.keys {
		if _, ok := side.chunks[k]; ok {
			continue
		}
		kind, _, _ := strings.Cut(k, " ")
		best, score := "", 0.5
		for _, a := range side.keys {
			if _, old := b.chunks[a]; old || !strings.HasPrefix(a, kind+" ") {
				continue
			}
			if sc := similarity(blankNames(b.chunks[k], b.names[k]), blankNames(side.chunks[a], side.names[a])); sc >= score {
				best, score = a, sc
			}
		}
		if best != "" {
			out[k] = best
		}
	}
	return out
}

func blankNames(text string, names []string) string {
	for _, n := range names {
		if i := strings.LastIndexByte(n, '.'); i >= 0 {
			n = n[i+1:]
		}
		text = strings.ReplaceAll(text, n, "\x00")
	}
	return text
}

// similarity is the share of non-blank lines two texts have in common.
func similarity(a, b string) float64 {
	nonBlank := func(s string) []string {
		var out []string
		for _, l := range merge.SplitLines([]byte(s)) {
			if strings.TrimSpace(l) != "" {
				out = append(out, strings.TrimSpace(l)+"\n")
			}
		}
		return out
	}
	la, lb := nonBlank(a), nonBlank(b)
	n := max(len(la), len(lb))
	if n == 0 {
		return 0
	}
	same := 0
	for _, e := range merge.Diff(la, lb) {
		if e.Kind == merge.Equal {
			same += e.A1 - e.A0
		}
	}
	return float64(same) / float64(n)
}

// pick3 merges one piece that is not split further.
func pick3(base, ours, theirs string) (string, bool) {
	switch {
	case ours == base:
		return theirs, true
	case theirs == base, ours == theirs:
		return ours, true
	}
	return "", false
}

// mergeGoDecls merges a Go file by declaration (see the comment at the top
// of this file). ok is false when it cannot, and the text conflict stands.
func mergeGoDecls(base, ours, theirs []byte) (out []byte, ok bool) {
	if IsGenerated(base) || IsGenerated(ours) || IsGenerated(theirs) {
		return nil, false
	}
	b, err := splitGo(base)
	if err != nil {
		return nil, false
	}
	o, err := splitGo(ours)
	if err != nil {
		return nil, false
	}
	t, err := splitGo(theirs)
	if err != nil {
		return nil, false
	}
	if o.pkg != t.pkg {
		return nil, false
	}
	header, ok := pick3(b.header, o.header, t.header)
	if !ok {
		return nil, false
	}
	tail, ok := pick3(b.tail, o.tail, t.tail)
	if !ok {
		return nil, false
	}

	// Declarations: take the side that changed each one. A name both sides
	// touched (changed, deleted or declared anew) must have come out the
	// same on both: extending a var group on one side and deleting it on
	// the other changes two different keys but the same names.
	result := map[string]string{}
	keys := unionKeys(b.keys, o.keys, t.keys)
	touchedO, touchedT := map[string]bool{}, map[string]bool{}
	same := map[string]bool{} // keys both sides left identical
	for _, k := range keys {
		bc, inB := b.chunks[k]
		oc, inO := o.chunks[k]
		tc, inT := t.chunks[k]
		same[k] = inO == inT && oc == tc
		names := append(append(append([]string(nil), b.names[k]...), o.names[k]...), t.names[k]...)
		for _, n := range names {
			if inO != inB || oc != bc {
				touchedO[n] = true
			}
			if inT != inB || tc != bc {
				touchedT[n] = true
			}
		}
	}
	for _, k := range keys {
		if same[k] {
			continue
		}
		for _, n := range append(append(append([]string(nil), b.names[k]...), o.names[k]...), t.names[k]...) {
			if touchedO[n] && touchedT[n] {
				return nil, false
			}
		}
	}
	// A declaration both sides removed is fine, unless a side renamed it:
	// two renames of one function, or a rename and a deletion, conflict.
	ro, rt := renames(b, o), renames(b, t)
	for _, k := range b.keys {
		_, inO := o.chunks[k]
		_, inT := t.chunks[k]
		if inO || inT {
			continue
		}
		a, c := ro[k], rt[k]
		if (a != "" || c != "") && (a != c || o.chunks[a] != t.chunks[c]) {
			return nil, false
		}
	}
	for _, k := range keys {
		bc, inB := b.chunks[k]
		oc, inO := o.chunks[k]
		tc, inT := t.chunks[k]
		oChanged := inO != inB || oc != bc
		tChanged := inT != inB || tc != bc
		switch {
		case oChanged && tChanged:
			if !same[k] {
				return nil, false
			}
			if inO {
				result[k] = oc
			}
		case oChanged:
			if inO {
				result[k] = oc
			}
		case tChanged:
			if inT {
				result[k] = tc
			}
		case inB:
			result[k] = bc
		}
	}
	order := declOrder(o.keys, t.keys, result)
	// A name declared twice does not build.
	declared := map[string]bool{}
	for _, k := range order {
		names := o.names[k]
		if _, ok := o.chunks[k]; !ok || result[k] != o.chunks[k] {
			names = t.names[k]
			if _, ok := t.chunks[k]; !ok || result[k] != t.chunks[k] {
				names = b.names[k]
			}
		}
		for _, n := range names {
			if declared[n] {
				return nil, false
			}
			declared[n] = true
		}
	}

	var body strings.Builder
	for _, k := range order {
		c := strings.TrimLeft(result[k], " \t\r\n")
		if !strings.HasSuffix(c, "\n") {
			c += "\n"
		}
		body.WriteString("\n")
		body.WriteString(c)
	}
	if tl := strings.TrimLeft(tail, " \t\r\n"); tl != "" {
		body.WriteString("\n" + tl)
	}

	imports, ok := mergeImports(b, o, t, body.String())
	if !ok {
		return nil, false
	}
	src := header
	if imports != "" {
		src += "\n" + strings.TrimLeft(imports, " \t\r\n")
	}
	src += body.String()
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return nil, false
	}
	// The result must parse and hold exactly the merged declarations.
	m, err := splitGo(formatted)
	if err != nil || len(m.keys) != len(order) {
		return nil, false
	}
	want := map[string]int{}
	for _, k := range order {
		want[baseKey(k)]++
	}
	for _, k := range m.keys {
		want[baseKey(k)]--
	}
	for _, n := range want {
		if n != 0 {
			return nil, false
		}
	}
	return formatted, true
}

// blankSep separates the kind of a blank declaration's key from its text.
const blankSep = "\x00"

// baseKey drops what gofmt may change in a key: the text of a blank
// declaration and the ordinal of a repeated one.
func baseKey(k string) string {
	k, _, _ = strings.Cut(k, blankSep)
	if i := strings.LastIndexByte(k, '#'); i > 0 {
		if _, err := strconv.Atoi(k[i+1:]); err == nil {
			k = k[:i]
		}
	}
	return k
}

func unionKeys(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, k := range l {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// declOrder orders the merged declarations: ours' order, then each
// declaration only theirs has goes right after the one before it in
// theirs.
func declOrder(ours, theirs []string, result map[string]string) []string {
	var order []string
	in := map[string]bool{}
	for _, k := range ours {
		if _, ok := result[k]; ok {
			order = append(order, k)
			in[k] = true
		}
	}
	last := -1
	for _, k := range theirs {
		if in[k] {
			for i, x := range order {
				if x == k {
					last = i
					break
				}
			}
			continue
		}
		if _, ok := result[k]; !ok {
			continue
		}
		order = append(order, "")
		copy(order[last+2:], order[last+1:])
		order[last+1] = k
		last++
		in[k] = true
	}
	return order
}

// mergeImports merges the import declarations of a file merged by
// declaration; body is the merged code after the imports, used to bring
// back an import one side removed that the code still needs.
func mergeImports(b, o, t *goParts, body string) (string, bool) {
	bset, oset, tset := importSet(b.specs), importSet(o.specs), importSet(t.specs)
	var final []goImport
	have := map[string]bool{}
	for _, list := range [][]goImport{o.specs, t.specs} {
		for _, imp := range list {
			k := imp.key()
			if have[k] {
				continue
			}
			_, inB := bset[k]
			_, inO := oset[k]
			_, inT := tset[k]
			if (inO && inT) || !inB {
				final = append(final, imp)
				have[k] = true
			}
		}
	}
	// An import a side removed comes back if the merged code still uses
	// its package and no other import provides that name.
	used := usedPackageNames(body)
	names := map[string]string{} // local name -> path
	for _, imp := range final {
		if imp.name != "_" && imp.name != "." {
			names[imp.local()] = imp.path
		}
	}
	for _, list := range [][]goImport{b.specs, o.specs, t.specs} {
		for _, imp := range list {
			l := imp.local()
			if have[imp.key()] || imp.name == "_" || imp.name == "." || !used[l] {
				continue
			}
			if _, taken := names[l]; taken {
				continue
			}
			final = append(final, imp)
			have[imp.key()] = true
			names[l] = imp.path
		}
	}
	// Two different packages under one name do not build.
	byName := map[string]string{}
	for _, imp := range final {
		if imp.name == "_" || imp.name == "." {
			continue
		}
		l := imp.local()
		if p, ok := byName[l]; ok && p != imp.path {
			return "", false
		}
		byName[l] = imp.path
	}

	// Keep a side's import text as it is when only that side changed it
	// and nothing came back.
	switch {
	case o.imports == b.imports && sameImports(t.specs, final):
		return t.imports, true
	case (t.imports == b.imports || t.imports == o.imports) && sameImports(o.specs, final):
		return o.imports, true
	}
	for _, imp := range final {
		if imp.path == "C" {
			return "", false // the cgo preamble cannot be rebuilt safely
		}
	}
	if len(final) == 0 {
		return "", true
	}
	var std, other []string
	for _, imp := range final {
		first := strings.SplitN(imp.path, "/", 2)[0]
		if strings.Contains(first, ".") {
			other = append(other, imp.text())
		} else {
			std = append(std, imp.text())
		}
	}
	var sb strings.Builder
	sb.WriteString("import (\n")
	sb.WriteString(strings.Join(std, ""))
	if len(std) > 0 && len(other) > 0 {
		sb.WriteString("\n")
	}
	sb.WriteString(strings.Join(other, ""))
	sb.WriteString(")\n")
	return sb.String(), true
}

func importSet(specs []goImport) map[string]goImport {
	m := make(map[string]goImport, len(specs))
	for _, s := range specs {
		m[s.key()] = s
	}
	return m
}

func sameImports(specs, final []goImport) bool {
	a, b := importSet(specs), importSet(final)
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// usedPackageNames returns the names used as X in X.Sel in Go code that
// are not declared in it: candidates for imported package names.
func usedPackageNames(code string) map[string]bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", "package p\n"+code, 0)
	used := map[string]bool{}
	if err != nil || f == nil {
		return used
	}
	unresolved := map[*ast.Ident]bool{}
	for _, id := range f.Unresolved {
		unresolved[id] = true
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok && unresolved[id] {
				used[id.Name] = true
			}
		}
		return true
	})
	return used
}
