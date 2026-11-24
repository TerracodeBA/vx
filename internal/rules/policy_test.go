package rules

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const policyToml = `[checks]
required = ["go vet ./...", "go test ./..."]

[[policy]]
paths = ["docs/**"]
mode  = "fast"

[[policy]]
paths   = ["internal/api/**", "proto/**"]
mode    = "gated"
require = ["buf lint", "go test ./..."]

[[policy]]
paths = ["internal/store/migrations/**"]
mode  = "human"

[[policy]]
paths     = ["internal/store/migrations/**", "deploy/**"]
mode      = "human"
approvers = ["user:alice", "team:acme/db", "user:alice"]
`

func TestParsePolicies(t *testing.T) {
	r, err := Parse([]byte(policyToml))
	if err != nil {
		t.Fatal(err)
	}
	want := []Policy{
		{Paths: []string{"docs/**"}, Mode: ModeFast, Line: 4},
		{Paths: []string{"internal/api/**", "proto/**"}, Mode: ModeGated, Require: []string{"buf lint", "go test ./..."}, Line: 8},
		{Paths: []string{"internal/store/migrations/**"}, Mode: ModeHuman, Require: []string{"go vet ./...", "go test ./..."}, Line: 13},
		{Paths: []string{"internal/store/migrations/**", "deploy/**"}, Mode: ModeHuman, Require: []string{"go vet ./...", "go test ./..."},
			Approvers: []string{"user:alice", "team:acme/db"}, Line: 17},
	}
	if !reflect.DeepEqual(r.Policies, want) {
		t.Fatalf("policies:\n got %+v\nwant %+v", r.Policies, want)
	}
	if r.IsEmpty() {
		t.Fatal("rules with policies are not empty")
	}
	if got := r.Policies[2].Describe(); got != `[[policy]] paths = ["internal/store/migrations/**"] (vx.toml line 13)` {
		t.Fatalf("Describe = %s", got)
	}
	// A human policy may require nothing.
	r, err = Parse([]byte("[[policy]]\npaths = [\"a/**\"]\nmode = \"human\"\nrequire = []\n"))
	if err != nil || len(r.Policies[0].Require) != 0 {
		t.Fatalf("human without checks: %+v %v", r.Policies, err)
	}
}

func TestParsePolicyErrors(t *testing.T) {
	cases := []struct {
		name, src string
		line      int
		want      string
	}{
		{"missing paths", "[[policy]]\nmode = \"fast\"\n", 1, `[[policy]] #1: paths is missing: the files this policy covers, e.g. paths = ["internal/store/migrations/**"]`},
		{"paths not a list", "[[policy]]\npaths = \"a/**\"\nmode = \"fast\"\n", 2, "policy.paths must be a list, not a string"},
		{"bad glob", "[[policy]]\npaths = [\"a/**\", \"b/\"]\nmode = \"fast\"\n", 2, `[[policy]] "a/**": glob "b/" ends with /: globs match files, write "b/**" for everything under it`},
		{"missing mode", "[[policy]]\npaths = [\"a/**\"]\n", 1, `[[policy]] "a/**": mode is missing: "fast", "gated" or "human"`},
		{"unknown mode", "[[policy]]\npaths = [\"a/**\"]\nmode = \"slow\"\n", 3, `[[policy]] "a/**": mode "slow" is not one of "fast", "gated" or "human"`},
		{"unknown key", "[[policy]]\npaths = [\"a/**\"]\nmode = \"fast\"\nreviewers = [\"user:bob\"]\n", 4, `unknown key "reviewers" in [[policy]] (known: paths, mode, require, approvers)`},
		{"require on fast", "[[policy]]\npaths = [\"a/**\"]\nmode = \"fast\"\nrequire = [\"go test ./...\"]\n", 4, `require has no effect with mode = "fast"`},
		{"empty require command", "[[policy]]\npaths = [\"a/**\"]\nmode = \"gated\"\nrequire = [\"go vet ./...\", \"\"]\n", 4, "require has an empty command"},
		{"gated without checks", "[[policy]]\npaths = [\"a/**\"]\nmode = \"gated\"\n", 1, `mode = "gated" needs checks: set require = ["go test ./..."] in the policy, or required in [checks]`},
		{"gated with empty require", "[checks]\nrequired = [\"x\"]\n[[policy]]\npaths = [\"a/**\"]\nmode = \"gated\"\nrequire = []\n", 6, `mode = "gated" with require = [] checks nothing`},
		{"approvers on gated", "[checks]\nrequired = [\"x\"]\n\n[[policy]]\npaths = [\"a/**\"]\nmode = \"gated\"\napprovers = [\"user:bob\"]\n", 7, `approvers only apply to mode = "human", not "gated"`},
		{"bad approver", "[[policy]]\npaths = [\"a/**\"]\nmode = \"human\"\napprovers = [\"bob\"]\n", 4, `approver "bob" must be "user:HANDLE" or "team:ORG/TEAM"`},
		{"bad team", "[[policy]]\npaths = [\"a/**\"]\nmode = \"human\"\napprovers = [\"team:db\"]\n", 4, `approver "team:db": write a team as team:ORG/TEAM, e.g. team:acme/db`},
		{"bad handle", "[[policy]]\npaths = [\"a/**\"]\nmode = \"human\"\napprovers = [\"user:Bob Smith\"]\n", 4, `approver "user:Bob Smith": "Bob Smith" is not a valid handle`},
		{"table instead of array", "[policy]\npaths = [\"a/**\"]\n", 1, "policy must be a list, not a table: write each rule as a [[policy]] table"},
		{"second policy", "[[policy]]\npaths = [\"a/**\"]\nmode = \"fast\"\n\n[[policy]]\npaths = [\"b/**\"]\nmode = \"Human\"\n", 7, `[[policy]] "b/**": mode "Human" is not one of`},
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
		})
	}
}

// The strictest matching policy applies; checks of every gated or human
// policy add up; approvers lists are kept per policy.
func TestDecide(t *testing.T) {
	r, err := Parse([]byte(policyToml))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		paths     []string
		mode      Mode
		hit       []string
		require   []string
		approvers [][]string
	}{
		{"no policy", []string{"README.md"}, ModeFast, nil, nil, nil},
		{"fast policy", []string{"docs/a.md"}, ModeFast, []string{"docs/a.md"}, nil, nil},
		{"gated", []string{"docs/a.md", "proto/vx.proto"}, ModeGated, []string{"proto/vx.proto"}, []string{"buf lint", "go test ./..."}, nil},
		{"human beats gated", []string{"proto/vx.proto", "internal/store/migrations/021_x.sql"}, ModeHuman,
			[]string{"internal/store/migrations/021_x.sql"}, []string{"buf lint", "go test ./...", "go vet ./..."}, [][]string{{"user:alice", "team:acme/db"}}},
		{"one human policy", []string{"deploy/x.yaml"}, ModeHuman, []string{"deploy/x.yaml"}, []string{"go vet ./...", "go test ./..."}, [][]string{{"user:alice", "team:acme/db"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := r.Decide(c.paths)
			if d.Mode != c.mode || !reflect.DeepEqual(d.Paths, c.hit) || !reflect.DeepEqual(d.Require, c.require) || !reflect.DeepEqual(d.Approvers, c.approvers) {
				t.Fatalf("decision %+v", d)
			}
		})
	}
	d := r.Decide([]string{"internal/store/migrations/021_x.sql"})
	if len(d.Policies) != 2 {
		t.Fatalf("both human policies should be named: %q", d.Policies)
	}
	if got := d.LandCommand(); got != `vx land --run 'go vet ./...' --run 'go test ./...'` {
		t.Fatalf("LandCommand = %s", got)
	}
}

// A change cannot weaken its own policy: the head's and the snapshot's
// decisions combine to the strictest.
func TestCombine(t *testing.T) {
	head := Decision{Mode: ModeHuman, Paths: []string{"m/1.sql"}, Require: []string{"a"}, Approvers: [][]string{{"user:alice"}}, Policies: []string{"p1"}}
	snap := Decision{Mode: ModeGated, Paths: []string{"api/x.go"}, Require: []string{"b"}}
	d := Combine(head, snap)
	if d.Mode != ModeHuman || !reflect.DeepEqual(d.Paths, []string{"m/1.sql"}) || !reflect.DeepEqual(d.Require, []string{"a", "b"}) || len(d.Approvers) != 1 {
		t.Fatalf("combine: %+v", d)
	}
	d = Combine(Decision{Mode: ModeFast}, head, Decision{Mode: ModeHuman, Paths: []string{"m/2.sql"}, Approvers: [][]string{{"user:alice"}, {"team:acme/db"}}})
	if !reflect.DeepEqual(d.Paths, []string{"m/1.sql", "m/2.sql"}) || len(d.Approvers) != 2 {
		t.Fatalf("combine equal modes: %+v", d)
	}
	if d := Combine(); d.Mode != ModeFast {
		t.Fatalf("empty combine: %+v", d)
	}
}

func TestMissingAndQuote(t *testing.T) {
	d := Decision{Mode: ModeGated, Require: []string{`test -z "$(gofmt -l .)"`, "go vet ./...", "echo 'x'"}}
	got := d.Missing([]string{" go vet ./... "}, []string{"echo 'x'"})
	want := []MissingCheck{{Command: `test -z "$(gofmt -l .)"`}, {Command: "echo 'x'", Failed: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %+v", got)
	}
	if c := d.LandCommand(); c != `vx land --run 'test -z "$(gofmt -l .)"' --run 'go vet ./...' --run 'echo '"'"'x'"'"''` {
		t.Fatalf("LandCommand = %s", c)
	}
}
