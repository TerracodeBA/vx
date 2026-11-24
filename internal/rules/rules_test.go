package rules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

func tree(t *testing.T, s blob.Store, files map[string]string) hash.Hash {
	t.Helper()
	ctx := context.Background()
	m := object.Manifest{}
	for p, c := range files {
		h, err := s.Put(ctx, hash.KindBlob, []byte(c))
		if err != nil {
			t.Fatal(err)
		}
		m[p] = object.File{Hash: h, Mode: object.ModeFile}
	}
	h, err := object.WriteManifest(ctx, s, m)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestParseRules(t *testing.T) {
	r, err := Parse([]byte(`
# comment
[[unique]]
name = "migration"
glob = "internal/store/migrations/*.sql"
key  = '^(\d+)_'

[[unique]]
name = "proto-field"
glob = "proto/**/*.proto"
key  = '^(\w+)\.proto$'

[[generated]]
glob    = "gen/**"
command = "buf generate"

[[generated]]
glob = "**/*_string.go"

[checks]
required = ["go vet ./...", "  go test ./...  ", "go vet ./..."]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := Rules{
		Unique: []Unique{
			{Name: "migration", Glob: "internal/store/migrations/*.sql", Key: `^(\d+)_`},
			{Name: "proto-field", Glob: "proto/**/*.proto", Key: `^(\w+)\.proto$`},
		},
		Generated:      []Generated{{Glob: "gen/**", Command: "buf generate"}, {Glob: "**/*_string.go"}},
		RequiredChecks: []string{"go vet ./...", "go test ./..."},
	}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("got %+v\nwant %+v", r, want)
	}
	if u, ok := r.UniqueRule("migration"); !ok || u.Glob != "internal/store/migrations/*.sql" {
		t.Fatalf("UniqueRule: %+v %v", u, ok)
	}
	empty, err := Parse(nil)
	if err != nil || !empty.IsEmpty() {
		t.Fatalf("empty file: %+v %v", empty, err)
	}
}

// The repository's own vx.toml is the first real case.
func TestParseRulesRepoFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := r.UniqueRule("migration"); !ok || u.Glob != "internal/store/migrations/*.sql" {
		t.Fatalf("migration rule: %+v", r.Unique)
	}
	if len(r.Generated) != 1 || r.Generated[0].Command != "buf generate" {
		t.Fatalf("generated: %+v", r.Generated)
	}
	if len(r.RequiredChecks) != 3 || r.RequiredChecks[1] != "go vet ./..." {
		t.Fatalf("required checks: %q", r.RequiredChecks)
	}
}

func TestParseRulesErrors(t *testing.T) {
	cases := []struct {
		name, src string
		line      int
		want      string
	}{
		{"syntax", "[[unique]]\nname = \"m\nglob = \"x\"\n", 2, "vx.toml line 2:"},
		{"unknown top-level key", "versoin = 2\n", 1, `unknown table or key "versoin" (known: [[unique]], [[generated]], [checks])`},
		{"unknown table", "\n[merge]\nstrategy = \"ours\"\n", 2, `unknown table or key "merge"`},
		{"unknown key in unique", "[[unique]]\nname = \"m\"\nglob = \"a/*.sql\"\nkey = '^(\\d+)'\n\n[[unique]]\nname = \"n\"\nglob = \"b/*.sql\"\nkey = '^(\\d+)'\npattern = \"x\"\n", 10, `unknown key "pattern" in [[unique]] (known: name, glob, key)`},
		{"unknown key in generated", "[[generated]]\nglob = \"gen/**\"\ncmd = \"make\"\n", 3, `unknown key "cmd" in [[generated]] (known: glob, command)`},
		{"unknown key in checks", "[checks]\noptional = [\"x\"]\n", 2, `unknown key "optional" in [checks] (known: required)`},
		{"missing name", "[[unique]]\nglob = \"a/*.sql\"\nkey = '^(\\d+)'\n", 1, "name is missing"},
		{"bad name", "[[unique]]\nname = \"my rule\"\nglob = \"a/*.sql\"\nkey = '^(\\d+)'\n", 2, "name may use letters"},
		{"duplicate name", "[[unique]]\nname = \"m\"\nglob = \"a/*.sql\"\nkey = '^(\\d+)'\n[[unique]]\nname = \"m\"\nglob = \"b/*.sql\"\nkey = '^(\\d+)'\n", 6, "already defined"},
		{"missing glob", "[[unique]]\nname = \"m\"\nkey = '^(\\d+)'\n", 1, "glob is missing"},
		{"bad glob", "[[unique]]\nname = \"m\"\nglob = \"a/**x/*.sql\"\nkey = '^(\\d+)'\n", 3, "** must be a whole path segment"},
		{"missing key", "[[unique]]\nname = \"m\"\nglob = \"a/*.sql\"\n", 1, "key is missing"},
		{"bad regexp", "[[unique]]\nname = \"m\"\nglob = \"a/*.sql\"\nkey = '^(\\d+_'\n", 4, "is not a valid regular expression"},
		{"no group", "[[unique]]\nname = \"m\"\nglob = \"a/*.sql\"\nkey = '^\\d+_'\n", 4, "has no group"},
		{"generated bad glob", "[[generated]]\nglob = \"/gen/**\"\n", 2, "starts with /"},
		{"generated missing glob", "[[generated]]\ncommand = \"make\"\n", 1, "glob is missing"},
		{"empty required", "[checks]\nrequired = [\"go vet ./...\", \" \"]\n", 2, "empty command"},
		{"wrong type", "[checks]\nrequired = \"go vet ./...\"\n", 2, "checks.required must be a list, not a string"},
		{"table instead of array", "[unique]\nname = \"m\"\n", 1, "unique must be a list, not a table: write each rule as a [[unique]] table"},
		{"number name", "[[unique]]\nname = 8\n", 0, "unique.name must be a string, not a number"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
			var re *Error
			if !errors.As(err, &re) {
				t.Fatalf("not a *rules.Error: %T", err)
			}
			if c.line > 0 && re.Problems[0].Line != c.line {
				t.Fatalf("line %d, want %d: %v", re.Problems[0].Line, c.line, err)
			}
			if !strings.HasPrefix(err.Error(), "vx.toml") {
				t.Fatalf("error does not name the file: %q", err)
			}
		})
	}
	// Several problems are all reported, in line order.
	_, err := Parse([]byte("[[unique]]\nname = \"m\"\nglob = \"\"\nkey = '^\\d'\nextra = 1\n"))
	var re *Error
	if !errors.As(err, &re) || len(re.Problems) != 3 {
		t.Fatalf("want 3 problems, got %v", err)
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		glob  string
		dir   string
		match []string
		miss  []string
	}{
		{"internal/store/migrations/*.sql", "internal/store/migrations",
			[]string{"internal/store/migrations/008_orgs.sql", "internal/store/migrations/.sql"},
			[]string{"internal/store/migrations/x/008.sql", "internal/store/migrations/008.sql.bak", "internal/store/008.sql"}},
		{"gen/**", "gen", []string{"gen/x.go", "gen/vx/v1/vx.pb.go"}, []string{"gen", "general/x.go", "x/gen/y.go"}},
		{"**/*_string.go", "", []string{"kind_string.go", "a/b/kind_string.go"}, []string{"a/kind_string.go.txt", "a/string.go"}},
		{"api/**/types.go", "api", []string{"api/types.go", "api/v1/types.go", "api/v1/x/types.go"}, []string{"api/v1/types.go/x", "xapi/types.go"}},
		{"**", "", []string{"a", "a/b/c"}, nil},
		{"vx.toml", "", []string{"vx.toml"}, []string{"a/vx.toml", "vxxtoml"}},
		{"docs/?.md", "docs", []string{"docs/a.md"}, []string{"docs/ab.md", "docs//.md"}},
		{"a/*/b/*.go", "a", []string{"a/x/b/y.go"}, []string{"a/b/y.go", "a/x/y/b/z.go"}},
		{"a.b/(x)+.go", "a.b", []string{"a.b/(x)+.go"}, []string{"aXb/(x)+.go", "a.b/xx.go"}},
	}
	for _, c := range cases {
		g, err := CompileGlob(c.glob)
		if err != nil {
			t.Fatalf("%s: %v", c.glob, err)
		}
		if g.Dir() != c.dir {
			t.Errorf("%s: dir %q, want %q", c.glob, g.Dir(), c.dir)
		}
		for _, p := range c.match {
			if !g.Match(p) {
				t.Errorf("%s should match %s", c.glob, p)
			}
		}
		for _, p := range c.miss {
			if g.Match(p) {
				t.Errorf("%s should not match %s", c.glob, p)
			}
		}
	}
	for _, bad := range []string{"", "/gen/**", "gen/", `gen\x`, "gen/[ab].go", "gen/{a,b}", "a//b", "./a", "a/../b", "a**", "**b/c"} {
		if _, err := CompileGlob(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if !MatchGlob("gen/**", "gen/a") || MatchGlob("/bad", "bad") {
		t.Error("MatchGlob")
	}
}

var migrationRule = Rules{Unique: []Unique{{Name: "migration", Glob: "internal/store/migrations/*.sql", Key: `^(\d+)_`}}}

// The clash of 2026-11-24: two changes each added migration 008.
func TestCheckRulesMigration008(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMem()
	files := map[string]string{"vx.toml": "", "README.md": "x"}
	for _, m := range []string{"001_init", "002_git_objects", "003_shipments", "004_changeset_warnings", "005_board", "006_claims", "007_tasks", "008_effort_rebases"} {
		files["internal/store/migrations/"+m+".sql"] = m
	}
	base := tree(t, s, files)
	files["internal/store/migrations/008_orgs.sql"] = "create table orgs ();"
	next := tree(t, s, files)

	ws, err := Check(ctx, s, migrationRule, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("warnings: %+v", ws)
	}
	w := ws[0]
	if w.Detail != "migration 008 is used by 008_effort_rebases.sql and 008_orgs.sql (rule migration in vx.toml)" {
		t.Fatalf("detail: %q", w.Detail)
	}
	if w.Kind != WarningRule || w.Name != "migration" || w.Decl != "008" || w.Path != "internal/store/migrations/008_orgs.sql" ||
		!reflect.DeepEqual(w.Files, []string{"internal/store/migrations/008_effort_rebases.sql", "internal/store/migrations/008_orgs.sql"}) {
		t.Fatalf("warning: %+v", w)
	}

	// The fix (renaming to 009) clears it.
	delete(files, "internal/store/migrations/008_orgs.sql")
	files["internal/store/migrations/009_orgs.sql"] = "create table orgs ();"
	fixed := tree(t, s, files)
	if ws, err := Check(ctx, s, migrationRule, next, fixed); err != nil || len(ws) != 0 {
		t.Fatalf("after the rename: %+v %v", ws, err)
	}
}

// A clash is reported only when a changed file is involved: old ones are
// not repeated on every land.
func TestCheckRulesOnlyChangedFiles(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMem()
	files := map[string]string{
		"internal/store/migrations/001_a.sql":  "a",
		"internal/store/migrations/001_b.sql":  "b", // old clash
		"internal/store/migrations/002_c.sql":  "c",
		"internal/store/migrations/notes.txt":  "n",
		"internal/store/migrations/README.sql": "no key",
	}
	base := tree(t, s, files)
	files["main.go"] = "package main"
	other := tree(t, s, files)
	if ws, err := Check(ctx, s, migrationRule, base, other); err != nil || len(ws) != 0 {
		t.Fatalf("unrelated change: %+v %v", ws, err)
	}
	files["internal/store/migrations/003_d.sql"] = "d"
	added := tree(t, s, files)
	if ws, err := Check(ctx, s, migrationRule, other, added); err != nil || len(ws) != 0 {
		t.Fatalf("new free number: %+v %v", ws, err)
	}
	// Editing one of the old clashing files reports that clash.
	files["internal/store/migrations/001_b.sql"] = "b2"
	edited := tree(t, s, files)
	ws, err := Check(ctx, s, migrationRule, added, edited)
	if err != nil || len(ws) != 1 || ws[0].Decl != "001" || ws[0].Path != "internal/store/migrations/001_b.sql" {
		t.Fatalf("edited clash: %+v %v", ws, err)
	}
	// Three files on one key, base zero: everything counts as new.
	files["internal/store/migrations/002_e.sql"] = "e"
	files["internal/store/migrations/002_f.sql"] = "f"
	all := tree(t, s, files)
	ws, err = Check(ctx, s, migrationRule, hash.Zero, all)
	if err != nil || len(ws) != 2 {
		t.Fatalf("zero base: %+v %v", ws, err)
	}
	if ws[1].Detail != "migration 002 is used by 002_c.sql, 002_e.sql and 002_f.sql (rule migration in vx.toml)" {
		t.Fatalf("three files: %q", ws[1].Detail)
	}
	// Across directories, full paths are shown.
	r := Rules{Unique: []Unique{{Name: "page", Glob: "web/**/*.html", Key: `^(\w+)\.html$`}}}
	b := tree(t, s, map[string]string{"web/a/index.html": "1"})
	n := tree(t, s, map[string]string{"web/a/index.html": "1", "web/b/index.html": "2"})
	ws, err = Check(ctx, s, r, b, n)
	if err != nil || len(ws) != 1 || ws[0].Detail != "page index is used by web/a/index.html and web/b/index.html (rule page in vx.toml)" {
		t.Fatalf("across dirs: %+v %v", ws, err)
	}
}

func TestRulesAt(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMem()
	none := tree(t, s, map[string]string{"a.go": "package a"})
	if r, err := At(ctx, s, none); err != nil || !r.IsEmpty() || r.Source != "" {
		t.Fatalf("no vx.toml: %+v %v", r, err)
	}
	with := tree(t, s, map[string]string{"vx.toml": "[checks]\nrequired = [\"go vet ./...\"]\n"})
	r, err := At(ctx, s, with)
	if err != nil || len(r.RequiredChecks) != 1 || r.Source != "vx.toml at tree "+with.Short() {
		t.Fatalf("with vx.toml: %+v %v", r, err)
	}
	bad := tree(t, s, map[string]string{"vx.toml": "[checks]\nrequird = []\n"})
	if _, err := At(ctx, s, bad); err == nil || !strings.Contains(err.Error(), "vx.toml line 2: unknown key \"requird\"") {
		t.Fatalf("broken vx.toml: %v", err)
	}
	nested := tree(t, s, map[string]string{"sub/vx.toml": "x = 1"})
	if r, err := At(ctx, s, nested); err != nil || !r.IsEmpty() {
		t.Fatalf("vx.toml not at the root: %+v %v", r, err)
	}
}

func TestGeneratedMatcher(t *testing.T) {
	r := Rules{Generated: []Generated{{Glob: "gen/**", Command: "buf generate"}, {Glob: "**/*.pb.go"}, {Glob: "/bad"}}}
	m := r.GeneratedMatcher()
	if cmd, ok := m("gen/vx/v1/vx.pb.go"); !ok || cmd != "buf generate" {
		t.Fatalf("gen: %q %v", cmd, ok)
	}
	if cmd, ok := m("x/y.pb.go"); !ok || cmd != "" {
		t.Fatalf("pb.go: %q %v", cmd, ok)
	}
	if _, ok := m("internal/x.go"); ok {
		t.Fatal("not generated")
	}
}
