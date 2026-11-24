package engine

import (
	"strings"
	"testing"
)

// semantic merges base with ours and theirs (path -> content; a missing
// path is unchanged, "" deletes it) and runs CheckGoSides on the result.
func semantic(t *testing.T, base, ours, theirs map[string]string) []Warning {
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
	ws, err := CheckGoSides(f.ctx, f.s, GoSides{Base: b, Ours: o, Theirs: th, Merged: r.Tree, OursName: "this land", TheirsName: "the effort"})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func semDetails(ws []Warning) string {
	var out []string
	for _, w := range ws {
		out = append(out, string(w.Kind)+": "+w.Detail)
	}
	return strings.Join(out, "\n")
}

var authBase = map[string]string{
	"go.mod": "module example.com/app\n\ngo 1.22\n",
	"auth/auth.go": `package auth

// Token is a session token.
type Token string

// Login signs a user in.
func Login(user string) (Token, error) {
	return Token(user), nil
}

// Store keeps sessions.
type Store struct{}

// Get returns a session.
func (s *Store) Get(id string) Token { return Token(id) }

// SetLimit sets the limit.
func SetLimit(n int) {}
`,
	"api/api.go": `package api

import "example.com/app/auth"

func Handle(user string) {
	_, _ = auth.Login(user)
}
`,
}

const loginCtx = `package auth

import "context"

// Token is a session token.
type Token string

// Login signs a user in.
func Login(ctx context.Context, user string) (Token, error) {
	return Token(user), nil
}

// Store keeps sessions.
type Store struct{}

// Get returns a session.
func (s *Store) Get(id string) Token { return Token(id) }

// SetLimit sets the limit.
func SetLimit(n int) {}
`

const apiCtx = `package api

import (
	"context"

	"example.com/app/auth"
)

func Handle(user string) {
	_, _ = auth.Login(context.Background(), user)
}
`

func TestSemanticSignatureAcrossPackages(t *testing.T) {
	ws := semantic(t, authBase,
		map[string]string{"auth/auth.go": loginCtx, "api/api.go": apiCtx},
		map[string]string{"audit/audit.go": `package audit

import "example.com/app/auth"

// Record logs a user in for the audit trail.
func Record(user string) error {
	_, err := auth.Login(user)
	return err
}
`})
	want := "signature: auth.Login now takes (ctx context.Context, user string) (changed in this land); audit/audit.go:7 calls auth.Login(user) (added by the effort)"
	if semDetails(ws) != want {
		t.Fatalf("got\n%s\nwant\n%s", semDetails(ws), want)
	}
	w := ws[0]
	if w.Path != "audit/audit.go" || w.Name != "auth.Login" || w.Decl != "func" || strings.Join(w.Files, ",") != "auth/auth.go,audit/audit.go" {
		t.Fatalf("warning %+v", w)
	}

	// The other way round: the effort changed the signature, this land
	// added the call.
	ws = semantic(t, authBase,
		map[string]string{"audit/audit.go": "package audit\n\nimport a \"example.com/app/auth\"\n\nfunc Record(u string) { a.Login(u) }\n"},
		map[string]string{"auth/auth.go": loginCtx, "api/api.go": apiCtx})
	want = "signature: auth.Login now takes (ctx context.Context, user string) (changed in the effort); audit/audit.go:5 calls a.Login(u) (added by this land)"
	if semDetails(ws) != want {
		t.Fatalf("got\n%s\nwant\n%s", semDetails(ws), want)
	}
}

func TestSemanticSignatureSamePackage(t *testing.T) {
	ws := semantic(t, authBase,
		map[string]string{"auth/auth.go": loginCtx, "api/api.go": apiCtx},
		map[string]string{"auth/session.go": "package auth\n\nfunc Refresh(u string) {\n\tLogin(u)\n}\n"})
	if len(ws) != 1 || ws[0].Kind != WarningSignature || !strings.Contains(ws[0].Detail, "auth/session.go:4 calls Login(u)") {
		t.Fatalf("got\n%s", semDetails(ws))
	}
}

func TestSemanticRemoved(t *testing.T) {
	renamed := strings.ReplaceAll(authBase["auth/auth.go"], "Token", "SessionToken")
	ws := semantic(t, authBase,
		map[string]string{"auth/auth.go": renamed},
		map[string]string{"audit/audit.go": "package audit\n\nimport \"example.com/app/auth\"\n\nvar Last auth.Token\n"})
	want := "removed: type auth.Token was removed (renamed to auth.SessionToken?) (in this land); audit/audit.go:5 still uses it: var Last auth.Token (added by the effort)"
	if semDetails(ws) != want {
		t.Fatalf("got\n%s\nwant\n%s", semDetails(ws), want)
	}
	// Same package, used as the X of a selector.
	ws = semantic(t, map[string]string{"p/a.go": "package p\n\nvar cfg struct{ Port int }\n\nfunc A() {}\n"},
		map[string]string{"p/a.go": "package p\n\nfunc A() {}\n"},
		map[string]string{"p/b.go": "package p\n\nfunc B() int { return cfg.Port }\n"})
	if len(ws) != 1 || ws[0].Kind != WarningRemoved || ws[0].Name != "p.cfg" {
		t.Fatalf("got\n%s", semDetails(ws))
	}
}

func TestSemanticMethod(t *testing.T) {
	changed := strings.Replace(authBase["auth/auth.go"], "func (s *Store) Get(id string) Token", "func (s *Store) Get(id string, fresh bool) Token", 1)
	ws := semantic(t, authBase,
		map[string]string{"auth/auth.go": changed},
		map[string]string{"api/cache.go": "package api\n\nimport \"example.com/app/auth\"\n\nfunc Cached(s *auth.Store) auth.Token { return s.Get(\"x\") }\n"})
	if len(ws) != 1 || ws[0].Decl != "method" || !strings.Contains(ws[0].Detail, "auth.Store.Get now takes (id string, fresh bool)") {
		t.Fatalf("got\n%s", semDetails(ws))
	}
	// A file that does not import the package is not searched for method
	// calls: m.Get there is some other Get.
	ws = semantic(t, authBase,
		map[string]string{"auth/auth.go": changed},
		map[string]string{"cache/cache.go": "package cache\n\ntype M map[string]int\n\nfunc (m M) Get(k string) int { return m[k] }\n\nfunc F(m M) int { return m.Get(\"x\") }\n"})
	if len(ws) != 0 {
		t.Fatalf("got\n%s", semDetails(ws))
	}
}

func TestSemanticResultsAndTypes(t *testing.T) {
	changed := strings.Replace(authBase["auth/auth.go"], "func SetLimit(n int) {}", "func SetLimit(n int64) {}", 1)
	changed = strings.Replace(changed, "func (s *Store) Get(id string) Token { return Token(id) }", "func (s *Store) Get(id string) Token { return Token(id) }\n\nfunc Flush() {}", 1)
	ws := semantic(t, authBase,
		map[string]string{"auth/auth.go": changed},
		map[string]string{"api/limits.go": "package api\n\nimport \"example.com/app/auth\"\n\nfunc Limits(n int) {\n\tauth.SetLimit(5)\n\tauth.SetLimit(n)\n}\n"})
	if len(ws) != 1 || !strings.Contains(ws[0].Detail, "api/limits.go:7 calls auth.SetLimit(n)") || !strings.Contains(ws[0].Detail, "now takes (n int64)") {
		t.Fatalf("got\n%s", semDetails(ws))
	}
	// Results: a call whose value is used breaks, a bare call does not.
	one := strings.Replace(authBase["auth/auth.go"], "// SetLimit sets the limit.", "// Version is the version.\nfunc Version() string { return \"1\" }\n\n// SetLimit sets the limit.", 1)
	two := strings.Replace(one, "func Version() string { return \"1\" }", "func Version() (string, error) { return \"1\", nil }", 1)
	base := map[string]string{"go.mod": authBase["go.mod"], "auth/auth.go": one}
	ws = semantic(t, base,
		map[string]string{"auth/auth.go": two},
		map[string]string{"api/v.go": "package api\n\nimport \"example.com/app/auth\"\n\nfunc V() string {\n\tauth.Version()\n\treturn auth.Version()\n}\n"})
	if len(ws) != 1 || !strings.Contains(ws[0].Detail, "auth.Version now returns (string, error)") || !strings.Contains(ws[0].Detail, "api/v.go:7") {
		t.Fatalf("got\n%s", semDetails(ws))
	}
}

// The false-positive traps: shadowing, fields, keys, the new shape, other
// packages, code that was already there, names that still exist.
func TestSemanticQuietCases(t *testing.T) {
	ours := map[string]string{"auth/auth.go": loginCtx, "api/api.go": apiCtx}
	cases := map[string]map[string]string{
		"local shadows the name":         {"auth/s.go": "package auth\n\nfunc R() {\n\tLogin := func(string) {}\n\tLogin(\"x\")\n}\n"},
		"param shadows the package":      {"api/p.go": "package api\n\ntype C struct{}\n\nfunc (C) Login(string) {}\n\nfunc R(auth C) { auth.Login(\"x\") }\n"},
		"new shape":                      {"audit/a.go": "package audit\n\nimport (\n\t\"context\"\n\n\t\"example.com/app/auth\"\n)\n\nfunc R() { auth.Login(context.TODO(), \"x\") }\n"},
		"other package of the same name": {"audit/a.go": "package audit\n\nimport \"example.com/app/other/auth\"\n\nfunc R() { auth.Login(\"x\") }\n"},
		"field and key named like it":    {"audit/a.go": "package audit\n\ntype S struct{ Login func(string) }\n\nfunc R(s S) { s.Login(\"x\"); _ = S{Login: nil} }\n"},
		"unrelated edit":                 {"api/other.go": "package api\n\nfunc Other() {}\n"},
	}
	for name, theirs := range cases {
		if ws := semantic(t, authBase, ours, theirs); len(ws) != 0 {
			t.Errorf("%s: got\n%s", name, semDetails(ws))
		}
	}
	// A call that was already there is not the other side's: the land that
	// changed the signature left it broken itself.
	if ws := semantic(t, authBase, map[string]string{"auth/auth.go": loginCtx}, map[string]string{"api/other.go": "package api\n\nfunc Other() {}\n"}); len(ws) != 0 {
		t.Errorf("old call: got\n%s", semDetails(ws))
	}
	// A name removed in one file but declared in another of the package.
	moved := map[string]string{
		"auth/auth.go":  strings.Replace(authBase["auth/auth.go"], "// Token is a session token.\ntype Token string\n", "", 1),
		"auth/token.go": "package auth\n\n// Token is a session token.\ntype Token string\n",
	}
	if ws := semantic(t, authBase, moved, map[string]string{"audit/a.go": "package audit\n\nimport \"example.com/app/auth\"\n\nvar T auth.Token\n"}); len(ws) != 0 {
		t.Errorf("moved: got\n%s", semDetails(ws))
	}
	// Each side alone: nothing meets.
	if ws := semantic(t, authBase, ours, map[string]string{"README.md": "x\n"}); len(ws) != 0 {
		t.Errorf("one side: got\n%s", semDetails(ws))
	}
}

func TestSemanticGenericsAndVariadic(t *testing.T) {
	base := map[string]string{
		"go.mod":    "module example.com/app\n",
		"xs/xs.go":  "package xs\n\nfunc Keys[K comparable, V any](m map[K]V) []K { return nil }\n\nfunc Join(sep string, parts ...string) string { return \"\" }\n",
		"xs/doc.go": "package xs\n",
	}
	ours := map[string]string{"xs/xs.go": "package xs\n\nfunc Keys[K comparable, V any](m map[K]V, sorted bool) []K { return nil }\n\nfunc Join(parts ...string) string { return \"\" }\n"}
	theirs := map[string]string{"use/use.go": "package use\n\nimport \"example.com/app/xs\"\n\nfunc U(m map[string]int, ps []string) {\n\t_ = xs.Keys[string, int](m)\n\t_ = xs.Join(\",\", ps...)\n\t_ = xs.Join(\",\", \"a\", \"b\")\n}\n"}
	ws := semantic(t, base, ours, theirs)
	got := semDetails(ws)
	if len(ws) != 2 || !strings.Contains(got, "use/use.go:6 calls xs.Keys[string, int](m)") || !strings.Contains(got, "use/use.go:7 calls xs.Join(\",\", ps...)") {
		t.Fatalf("got\n%s", got)
	}
}
