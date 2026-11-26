package rules

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const checksToml = `[[check]]
name     = "browser-e2e"
when     = "ship"
paths    = ["web/**"]
runner   = "agent:browser"
reporter = "independent"
inputs   = ["diff", "changed_files", "preview_url"]
outputs  = { required = ["steps.json"], artifacts = ["*.gif", "*.png"] }
after    = ["unit"]
timeout  = "30m"

[[check]]
name   = "unit"
when   = "land"
run    = "go test ./..."
blocks = "land"

[[check]]
name   = "signoff"
runner = "human"
when   = "manual"
`

// The example of #47 parses with its defaults: ship checks block ship,
// manual ones nothing, a human runner means a human reporter.
func TestParseChecks(t *testing.T) {
	r, err := Parse([]byte(checksToml))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 3 || len(r.Ignored) != 0 {
		t.Fatalf("checks = %+v ignored %v", r.Checks, r.Ignored)
	}
	e2e := r.Checks[0]
	want := CheckDef{
		Name: "browser-e2e", When: WhenShip, Paths: []string{"web/**"}, Runner: "agent:browser", Reporter: ReporterIndependent,
		Inputs: []string{"diff", "changed_files", "preview_url"}, Outputs: Outputs{Required: []string{"steps.json"}, Artifacts: []string{"*.gif", "*.png"}},
		After: []string{"unit"}, Blocks: BlocksShip, Timeout: 30 * time.Minute, Origin: OriginCheck, Line: 1,
	}
	if !reflect.DeepEqual(e2e, want) {
		t.Fatalf("browser-e2e =\n%+v\nwant\n%+v", e2e, want)
	}
	if u := r.Checks[1]; u.Run != "go test ./..." || u.Blocks != BlocksLand || u.Reporter != ReporterSelf || u.Line != 12 || !u.IsRun() {
		t.Fatalf("unit = %+v", u)
	}
	if s := r.Checks[2]; s.Blocks != BlocksNothing || s.Reporter != ReporterHuman || s.When != WhenManual {
		t.Fatalf("signoff = %+v", s)
	}
	if !e2e.Applies([]string{"web/app.js"}) || e2e.Applies([]string{"api/x.go"}) || !r.Checks[1].Applies([]string{"anything"}) {
		t.Fatal("paths decide whether a check applies; no paths: always")
	}
	if !e2e.ArtifactAllowed("flow.gif") || e2e.ArtifactAllowed("notes.txt") || !r.Checks[1].ArtifactAllowed("x.bin") {
		t.Fatal("outputs.artifacts limit artifact names")
	}
	if got := r.Checks[1].Describe(); got != `check "unit" (vx.toml line 12)` {
		t.Fatalf("describe = %q", got)
	}
	if u, ok := r.RunCheck(" go test ./... "); !ok || u.Name != "unit" {
		t.Fatalf("RunCheck = %+v %v", u, ok)
	}
	if got := FormatTimeout(e2e.Timeout); got != "30m" {
		t.Fatalf("timeout = %q", got)
	}
	for d, want := range map[time.Duration]string{90 * time.Second: "1m30s", time.Hour: "1h", 10 * time.Second: "10s", 90 * time.Minute: "1h30m"} {
		if got := FormatTimeout(d); got != want {
			t.Errorf("FormatTimeout(%v) = %q, want %q", d, got, want)
		}
	}
	// The JSON form (organization rules store it) round-trips.
	data, err := json.Marshal(e2e)
	if err != nil {
		t.Fatal(err)
	}
	var back CheckDef
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back, e2e) {
		t.Fatalf("json round trip: %s -> %+v %v", data, back, err)
	}
	steps := StepsOf(r.Checks)
	if len(steps) != 3 || steps[0].Name != "browser-e2e" || strings.Join(steps[0].After, ",") != "unit" {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestParseCheckErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{"[[check]]\nrun = \"x\"\n", `vx.toml line 1: [[check]] #1: name is missing`},
		{"[[check]]\nname = \"a b\"\nrun = \"x\"\n", `vx.toml line 2: [[check]] "a b": name may use letters, digits`},
		{"[[check]]\nname = \"a\"\nrun = \"x\"\n[[check]]\nname = \"a\"\nrun = \"y\"\n", `vx.toml line 5: [[check]] "a": a [[check]] of that name is already defined`},
		{"[[check]]\nname = \"a\"\nwhen = \"merge\"\nrun = \"x\"\n", `vx.toml line 3: [[check]] "a": when "merge" is not one of "land", "ship" or "manual"`},
		{"[[check]]\nname = \"a\"\nblocks = \"all\"\nrun = \"x\"\n", `vx.toml line 3: [[check]] "a": blocks "all" is not one of "land", "ship" or "nothing"`},
		{"[[check]]\nname = \"a\"\nwhen = \"ship\"\nblocks = \"land\"\nrunner = \"ci\"\n", `vx.toml line 4: [[check]] "a": a check that runs at ship (when = "ship") cannot block land`},
		{"[[check]]\nname = \"a\"\nreporter = \"anyone\"\nrun = \"x\"\n", `vx.toml line 3: [[check]] "a": reporter "anyone" is not one of "self", "independent", "ci" or "human"`},
		{"[[check]]\nname = \"a\"\nrunner = \"robot\"\n", `vx.toml line 3: [[check]] "a": runner "robot" is not "ci", "human" or "agent:KIND"`},
		{"[[check]]\nname = \"a\"\n", `vx.toml line 1: [[check]] "a": say how it runs: run = "COMMAND"`},
		{"[[check]]\nname = \"a\"\nrun = \"x\"\npaths = [\"/web\"]\n", `vx.toml line 4: [[check]] "a": paths: glob "/web" starts with /`},
		{"[[check]]\nname = \"a\"\nrun = \"x\"\ntimeout = \"soon\"\n", `vx.toml line 4: [[check]] "a": timeout "soon" is not a duration`},
		{"[[check]]\nname = \"a\"\nreporter = \"self\"\nallow_same_person = true\nrun = \"x\"\n", `vx.toml line 4: [[check]] "a": allow_same_person only applies to reporter = "independent"`},
		{"[[check]]\nname = \"a\"\nrun = \"x\"\ninputs = [\"b.url\"]\n", `[[check]] "a": input "b.url" needs after = ["b"] so b has run first`},
		{"[[check]]\nname = \"a\"\nrun = \"x\"\nafter = [\"a\"]\n", `vx.toml line 4: [[check]] "a": after names the check itself`},
		{"[[check]]\nname = \"a\"\nrun = \"x\"\nafter = [\"b\"]\n[[check]]\nname = \"b\"\nrun = \"y\"\nafter = [\"a\"]\n", `after makes a cycle: a -> b -> a; a check cannot wait for itself`},
		{"[[check]]\nname = \"a\"\nrunner = \"ci\"\noutputs = { artifacts = [\"shots/*.png\"] }\n", `outputs.artifacts "shots/*.png": artifact names have no directories`},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q) = %v, want %q", c.src, err, c.want)
		}
	}
}

// A [[check]] with a key from a newer vx is switched off alone, with a
// notice; the checks after it keep their after link.
func TestParseChecksNewerKey(t *testing.T) {
	src := `[[check]]
name   = "unit"
run    = "go test ./..."
retry  = 3

[[check]]
name   = "e2e"
runner = "agent:browser"
after  = ["unit"]
`
	r, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 1 || r.Checks[0].Name != "e2e" {
		t.Fatalf("checks = %+v", r.Checks)
	}
	if len(r.Ignored) != 1 || !strings.Contains(r.Ignored[0], `vx.toml line 4: rule [[check]] "unit" uses key "retry", which this vx does not know; the rule is ignored`) {
		t.Fatalf("ignored = %v", r.Ignored)
	}
}

// [checks] required and [[policy]] show as checks too: required ones
// block nothing, a gated policy's block land for its paths, a human
// policy adds the review a person reports by approving.
func TestEffectiveChecks(t *testing.T) {
	r, err := Parse([]byte(`[checks]
required = ["go vet ./...", "go test ./..."]

[[policy]]
paths   = ["api/**"]
mode    = "gated"
require = ["buf lint"]

[[policy]]
paths = ["db/**"]
mode  = "human"

[[check]]
name = "unit"
run  = "go test ./..."
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range r.Effective() {
		got = append(got, string(d.Origin)+":"+d.Name+":"+string(d.Blocks)+":"+strings.Join(d.Paths, ","))
	}
	want := []string{
		"check:unit:land:",
		"required:go vet ./...:nothing:",
		"policy:buf lint:land:api/**",
		"policy:go vet ./...:land:db/**",
		"policy:go test ./...:land:db/**",
		"policy:review:land:db/**",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("effective =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Organization rules parse with the same rules, and a repository's checks
// and the organization's validate together.
func TestParseChecksForOrgRules(t *testing.T) {
	org, err := ParseChecks([]byte("[[check]]\nname = \"security\"\nrunner = \"agent:security\"\nreporter = \"independent\"\n"))
	if err != nil || len(org) != 1 {
		t.Fatalf("%+v %v", org, err)
	}
	org[0].Origin = OriginOrg
	if _, err := ParseChecks([]byte("[[check]]\nname = \"x\"\nrun = \"y\"\nflaky = true\n")); err == nil || !strings.Contains(err.Error(), `unknown key "check.flaky"`) {
		t.Fatalf("unknown keys in organization rules: %v", err)
	}
	repo := []CheckDef{{Name: "e2e", After: []string{"security"}, Origin: OriginCheck, Line: 3}}
	if err := ValidateChecks(repo, org); err != nil {
		t.Fatalf("after across sets: %v", err)
	}
	dup := []CheckDef{{Name: "security", Origin: OriginCheck, Line: 7}}
	if err := ValidateChecks(org, dup); err == nil || !strings.Contains(err.Error(), `check "security" (vx.toml line 7): check "security" (organization rule) already defines a check of that name`) {
		t.Fatalf("duplicate across sets: %v", err)
	}
	if err := ValidateChecks(repo); err == nil || !strings.Contains(err.Error(), `after names "security", which is not a check here`) {
		t.Fatalf("after to nothing: %v", err)
	}
}
