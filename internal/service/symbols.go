package service

// Symbol claims: a claim on "pkg.Func" counts against a land only when the
// land changes the lines of that symbol. Go files are compared declaration
// by declaration (go/ast, before and after the land); in other languages a
// symbol stands for the whole file named like its package part.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"strings"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// goFile is a parsed Go file: its package name and the source text of each
// top-level declaration, by name ("Func", "Type", "Type.Method", "Var").
type goFile struct {
	pkg   string
	decls map[string]string
}

// parseGoDecls parses src. ok is false when it is not valid Go.
func parseGoDecls(src []byte) (goFile, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return goFile{}, false
	}
	out := goFile{pkg: f.Name.Name, decls: map[string]string{}}
	text := func(from, to token.Pos) string {
		a, b := fset.Position(from).Offset, fset.Position(to).Offset
		if a < 0 || b > len(src) || a > b {
			return ""
		}
		return string(src[a:b])
	}
	start := func(doc *ast.CommentGroup, pos token.Pos) token.Pos {
		if doc != nil {
			return doc.Pos()
		}
		return pos
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = recvName(d.Recv.List[0].Type) + "." + name
			}
			out.decls[name] += text(start(d.Doc, d.Pos()), d.End())
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				// A lone spec takes the whole declaration (keyword, doc);
				// specs in a group take their own lines.
				from, to := start(d.Doc, d.Pos()), d.End()
				var names []string
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					names = []string{sp.Name.Name}
					if d.Lparen.IsValid() {
						from, to = start(sp.Doc, sp.Pos()), sp.End()
					}
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						names = append(names, n.Name)
					}
					if d.Lparen.IsValid() {
						from, to = start(sp.Doc, sp.Pos()), sp.End()
					}
				}
				for _, n := range names {
					out.decls[n] += text(from, to)
				}
			}
		}
	}
	return out, true
}

// recvName is the type name of a method receiver: T for T, *T, T[K] or *T[K].
func recvName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// declMatches reports whether a declaration name is what symbol (the part
// after the package) names: the same, a member of it ("Type" covers
// "Type.Method"), or what it is a member of ("Type" for "Type.Field").
func declMatches(decl, symbol string) bool {
	return symbol == "" || decl == symbol || strings.HasPrefix(decl, symbol+".") || strings.HasPrefix(symbol, decl+".")
}

// symbolTarget splits a claim target into its package part and the rest
// ("service.Note.Read" is "service" and "Note.Read"). ok is false for
// paths.
func symbolTarget(t string) (pkg, rest string, ok bool) {
	if looksLikePath(t) {
		return "", "", false
	}
	pkg, rest, _ = strings.Cut(trimSep(t), ".")
	return pkg, rest, pkg != ""
}

// landTouches reports whether the files a land changed touch a claim's
// target: a path target as TargetsMatch says, a symbol target by its lines.
type landTouches struct {
	ctx    context.Context
	blobs  blob.Store
	files  []store.FileChange
	parsed map[hash.Hash]*goFile // nil value: not Go
}

func newLandTouches(ctx context.Context, blobs blob.Store, files []store.FileChange) *landTouches {
	return &landTouches{ctx: ctx, blobs: blobs, files: files, parsed: map[hash.Hash]*goFile{}}
}

// goFileAt parses a version of a file; nil when there is none or it is not Go.
func (lt *landTouches) goFileAt(f object.File) (*goFile, error) {
	if f.Hash.IsZero() {
		return nil, nil
	}
	if g, ok := lt.parsed[f.Hash]; ok {
		return g, nil
	}
	data, err := blob.GetKind(lt.ctx, lt.blobs, hash.KindBlob, f.Hash)
	if err != nil {
		return nil, err
	}
	var g *goFile
	if parsed, ok := parseGoDecls(data); ok {
		g = &parsed
	}
	lt.parsed[f.Hash] = g
	return g, nil
}

// touches reports whether any of targets is touched by the land.
func (lt *landTouches) touches(targets []string) (bool, error) {
	for _, t := range targets {
		for _, f := range lt.files {
			if TargetsMatch(t, f.Path) {
				return true, nil
			}
		}
		pkg, rest, ok := symbolTarget(t)
		if !ok {
			continue
		}
		for _, f := range lt.files {
			hit, err := lt.symbolTouched(f, pkg, rest)
			if err != nil || hit {
				return hit, err
			}
		}
	}
	return false, nil
}

// symbolTouched reports whether the change to one file touches symbol
// pkg.rest.
func (lt *landTouches) symbolTouched(f store.FileChange, pkg, rest string) (bool, error) {
	dir := path.Base(path.Dir(f.Path))
	if !strings.HasSuffix(f.Path, ".go") {
		base := path.Base(f.Path)
		if i := strings.IndexByte(base, '.'); i > 0 {
			base = base[:i]
		}
		return base == pkg || dir == pkg, nil
	}
	before, err := lt.goFileAt(f.Old)
	if err != nil {
		return false, err
	}
	after, err := lt.goFileAt(f.New)
	if err != nil {
		return false, err
	}
	inPkg := func(g *goFile) bool { return g != nil && (g.pkg == pkg || strings.TrimSuffix(g.pkg, "_test") == pkg) }
	if !inPkg(before) && !inPkg(after) {
		// Not this package, or a side that does not parse: then the
		// directory decides, and the whole file counts.
		parses := (f.Old.Hash.IsZero() || before != nil) && (f.New.Hash.IsZero() || after != nil)
		return !parses && dir == pkg, nil
	}
	decls := func(g *goFile) map[string]string {
		if g == nil || !inPkg(g) {
			return nil
		}
		return g.decls
	}
	b, a := decls(before), decls(after)
	for name, text := range b {
		if declMatches(name, rest) && a[name] != text {
			return true, nil
		}
	}
	for name := range a {
		if _, had := b[name]; !had && declMatches(name, rest) {
			return true, nil
		}
	}
	return false, nil
}
