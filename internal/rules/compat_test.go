package rules

import (
	"reflect"
	"strings"
	"testing"
)

// A vx.toml written for a newer vx: one rule uses a key this vx does not
// know, and there is a table it does not know. Only that rule and that
// table are left out, each with a notice; the valid rules next to them
// still apply, and real mistakes in known keys are still errors.
func TestParseRulesFromNewerVX(t *testing.T) {
	src := `[[unique]]
name = "migration"
glob = "internal/store/migrations/*.sql"
key  = '^(\d+)_'

[[unique]]
name    = "proto-field"
glob    = "proto/**/*.proto"
key     = '^(\w+)'
future  = "per-message"

[[generated]]
glob    = "gen/**"
command = "buf generate"
cache   = true

[[generated]]
glob = "**/*_string.go"

[checks]
required = ["go vet ./..."]
parallel = 4

[merge]
strategy = "ours"
`
	r, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unique) != 1 || r.Unique[0].Name != "migration" {
		t.Fatalf("unique = %+v", r.Unique)
	}
	if !reflect.DeepEqual(r.Generated, []Generated{{Glob: "**/*_string.go"}}) {
		t.Fatalf("generated = %+v", r.Generated)
	}
	if !reflect.DeepEqual(r.RequiredChecks, []string{"go vet ./..."}) {
		t.Fatalf("required = %q", r.RequiredChecks)
	}
	want := []string{
		`vx.toml line 10: rule [[unique]] "proto-field" uses key "future", which this vx does not know; the rule is ignored: update vx (known: name, glob, key, content, scope)`,
		`vx.toml line 15: rule [[generated]] "gen/**" uses key "cache", which this vx does not know; the rule is ignored: update vx (known: glob, command)`,
		`vx.toml line 22: [checks] uses key "parallel", which this vx does not know; ignored: update vx (known: required)`,
		`vx.toml line 24: table or key "merge" is unknown to this vx; ignored: update vx if it is new (known: [[unique]], [[generated]], [checks], [automation], [[policy]], [[check]], [files])`,
	}
	if !reflect.DeepEqual(r.Ignored, want) {
		t.Fatalf("ignored:\n%s\nwant\n%s", strings.Join(r.Ignored, "\n"), strings.Join(want, "\n"))
	}

	// An ignored rule is not validated; a mistake in a known key of a
	// valid rule is still an error.
	if _, err := Parse([]byte(src + "\n[[unique]]\nname = \"bad\"\nglob = \"a/*\"\nkey = '^\\d+'\n")); err == nil || !strings.Contains(err.Error(), `[[unique]] "bad": key`) || !strings.Contains(err.Error(), "has no group") {
		t.Fatalf("bad key next to future rules: %v", err)
	}
	if _, err := Parse([]byte("[[unique]]\nname = \"x\"\nglob = \"a/**x\"\nkey = '(\\d+)'\nfuture = 1\n")); err != nil {
		t.Fatalf("an ignored rule is not validated: %v", err)
	}
	// A [[policy]] with an unknown key is ignored too.
	r, err = Parse([]byte("[[policy]]\npaths = [\"a/**\"]\nmode = \"human\"\nreviewers = [\"user:bob\"]\n\n[[policy]]\npaths = [\"b/**\"]\nmode = \"fast\"\n"))
	if err != nil || len(r.Policies) != 1 || r.Policies[0].Paths[0] != "b/**" || len(r.Ignored) != 1 || !strings.Contains(r.Ignored[0], `rule [[policy]] "a/**" uses key "reviewers"`) {
		t.Fatalf("policy: %+v %v", r, err)
	}
}
