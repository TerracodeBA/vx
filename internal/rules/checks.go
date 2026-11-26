package rules

// Company-defined checks (#47): [[check]] tables of vx.toml say what must
// be proven about a change before it lands or ships. vx understands their
// structure (when, which paths, who runs and who may report, what they
// produce, what they block), never their meaning: agents, CI and people
// do the checking and report results (evidence) bound to a change and
// its exact tree.
//
//	[[check]]
//	name     = "browser-e2e"
//	when     = "ship"                 # land | ship | manual
//	paths    = ["web/**"]             # only when these change
//	runner   = "agent:browser"        # who picks it up: agent:KIND, "ci" or "human"
//	reporter = "independent"          # self | independent | ci | human
//	inputs   = ["diff", "changed_files", "preview_url"]
//	outputs  = { required = ["steps.json"], artifacts = ["*.gif", "*.png"] }
//	after    = ["unit"]
//	blocks   = "ship"                 # land | ship | nothing
//	timeout  = "30m"
//
//	[[check]]
//	name   = "unit"
//	run    = "go test ./..."          # run by the landing agent (vx land --run)
//	blocks = "land"

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// When is the stage at which a check is expected to run.
type When string

// Stages.
const (
	WhenLand   When = "land"   // before the change lands
	WhenShip   When = "ship"   // before the effort ships
	WhenManual When = "manual" // only when someone asks for it
)

// Blocks is what a check without a passing result stops.
type Blocks string

// What a check blocks.
const (
	BlocksLand    Blocks = "land"
	BlocksShip    Blocks = "ship"
	BlocksNothing Blocks = "nothing"
)

// Reporter says whose result counts for a check. The rules of each are in
// the service (Service.reporterCounts).
type Reporter string

// Reporters.
const (
	ReporterSelf        Reporter = "self"        // anyone with write, the author included
	ReporterIndependent Reporter = "independent" // not the author, nor another agent of the author's person
	ReporterCI          Reporter = "ci"          // a CI service token
	ReporterHuman       Reporter = "human"       // a person, never an agent
)

// Origin says where a check definition comes from.
type Origin string

// Origins.
const (
	OriginCheck    Origin = "check"    // a [[check]] table of vx.toml
	OriginRequired Origin = "required" // [checks] required, as a run check
	OriginPolicy   Origin = "policy"   // a gated or human [[policy]]
	OriginOrg      Origin = "org"      // an organization rule (set by org admins)
)

// Outputs are what a check's report produces: Required names a passing
// report must include (an artifact name, the data file's name, or a
// top-level key of its data), Artifacts the globs artifact names must
// match (empty: any name).
type Outputs struct {
	Required  []string `json:"required,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// CheckDef is one check: a [[check]] table, an organization rule, or a
// check made from the older [checks] required and [[policy]] tables. Its
// JSON form is how organization rules are stored.
type CheckDef struct {
	Name     string   `json:"name"`
	When     When     `json:"when"`
	Paths    []string `json:"paths,omitempty"`  // globs; empty: every change
	Runner   string   `json:"runner,omitempty"` // "agent:KIND", "ci", "human"; empty for run checks
	Reporter Reporter `json:"reporter"`
	// AllowSamePerson lets another agent of the author's person count as
	// independent (allow_same_person = true; the company allows it).
	AllowSamePerson bool     `json:"allow_same_person,omitempty"`
	Inputs          []string `json:"inputs,omitempty"`
	Outputs         Outputs  `json:"outputs,omitempty"`
	After           []string `json:"after,omitempty"`
	Blocks          Blocks   `json:"blocks"`
	// Timeout bounds a run by a runner; zero: no limit set.
	Timeout time.Duration `json:"timeout,omitempty"`
	// Run is the command of a run check: the landing agent runs it (vx
	// land --run) and vx records the output.
	Run    string `json:"run,omitempty"`
	Origin Origin `json:"origin,omitempty"`
	// Line is the line of the table in vx.toml; 0 when not known.
	Line int `json:"line,omitempty"`
}

// IsRun reports whether the check is a command vx runs and records.
func (d CheckDef) IsRun() bool { return d.Run != "" }

// Applies reports whether a change of paths needs the check: always
// without Paths, else when one of them matches a glob.
func (d CheckDef) Applies(paths []string) bool {
	if len(d.Paths) == 0 {
		return true
	}
	for _, g := range d.Paths {
		c, err := CompileGlob(g)
		if err != nil {
			continue
		}
		for _, p := range paths {
			if c.Match(p) {
				return true
			}
		}
	}
	return false
}

// Describe names the check for people: `check "unit" (vx.toml line 12)`,
// `required check "go vet ./..." (vx.toml [checks])`.
func (d CheckDef) Describe() string {
	switch d.Origin {
	case OriginRequired:
		return fmt.Sprintf("required check `%s` (%s [checks])", d.Run, File)
	case OriginPolicy:
		return fmt.Sprintf("check %q of a landing policy (%s [[policy]])", d.Name, File)
	case OriginOrg:
		return fmt.Sprintf("check %q (organization rule)", d.Name)
	}
	if d.Line > 0 {
		return fmt.Sprintf("check %q (%s line %d)", d.Name, File, d.Line)
	}
	return fmt.Sprintf("check %q (%s)", d.Name, File)
}

// ArtifactAllowed reports whether an artifact called name may be attached
// to a result of the check.
func (d CheckDef) ArtifactAllowed(name string) bool {
	if len(d.Outputs.Artifacts) == 0 {
		return true
	}
	for _, g := range d.Outputs.Artifacts {
		if c, err := CompileGlob(g); err == nil && c.Match(name) {
			return true
		}
	}
	return false
}

// Check returns the [[check]] called name.
func (r Rules) Check(name string) (CheckDef, bool) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return CheckDef{}, false
}

// RunCheck returns the [[check]] whose run command is command (after
// trimming spaces).
func (r Rules) RunCheck(command string) (CheckDef, bool) {
	command = strings.TrimSpace(command)
	for _, c := range r.Checks {
		if c.Run != "" && c.Run == command {
			return c, true
		}
	}
	return CheckDef{}, false
}

// PolicyReviewCheck is the name of the check a human [[policy]] stands
// for: a person approves the change through the review flow.
const PolicyReviewCheck = "review"

// Effective is every check of the rules: the [[check]] tables, then the
// older tables as checks so one list says what a change needs:
//
//   - [checks] required: run checks named by their command that block
//     nothing (vx land marks them not verified when vx did not run them),
//     unless a [[check]] already runs the same command;
//   - the checks a gated or human [[policy]] requires: run checks that
//     block land for the policy's paths (Service.landPolicy enforces them,
//     with its own messages);
//   - a human [[policy]]: a check "review" a person reports by approving
//     the change (vx review approve), blocking land for the policy's paths.
func (r Rules) Effective() []CheckDef {
	out := append([]CheckDef(nil), r.Checks...)
	ran := map[string]bool{}
	for _, c := range r.Checks {
		if c.Run != "" {
			ran[c.Run] = true
		}
	}
	for _, cmd := range r.RequiredChecks {
		if ran[cmd] {
			continue
		}
		out = append(out, CheckDef{Name: cmd, When: WhenLand, Reporter: ReporterSelf, Blocks: BlocksNothing, Run: cmd, Origin: OriginRequired})
	}
	for _, p := range r.Policies {
		if p.Mode != ModeGated && p.Mode != ModeHuman {
			continue
		}
		for _, cmd := range p.Require {
			out = append(out, CheckDef{Name: cmd, When: WhenLand, Paths: p.Paths, Reporter: ReporterSelf, Blocks: BlocksLand, Run: cmd, Origin: OriginPolicy, Line: p.Line})
		}
		if p.Mode == ModeHuman {
			out = append(out, CheckDef{Name: PolicyReviewCheck, When: WhenLand, Paths: p.Paths, Runner: "human", Reporter: ReporterHuman, Blocks: BlocksLand, Origin: OriginPolicy, Line: p.Line})
		}
	}
	return out
}

type tomlOutputs struct {
	Required  []string `toml:"required"`
	Artifacts []string `toml:"artifacts"`
}

type tomlCheck struct {
	Name     string      `toml:"name"`
	When     string      `toml:"when"`
	Paths    []string    `toml:"paths"`
	Runner   string      `toml:"runner"`
	Reporter string      `toml:"reporter"`
	AllowSP  bool        `toml:"allow_same_person"`
	Inputs   []string    `toml:"inputs"`
	Outputs  tomlOutputs `toml:"outputs"`
	After    []string    `toml:"after"`
	Blocks   string      `toml:"blocks"`
	Timeout  string      `toml:"timeout"`
	Run      string      `toml:"run"`
}

// checkKeys are the keys of a [[check]] table this vx knows.
var checkKeys = []string{"name", "when", "paths", "runner", "reporter", "allow_same_person", "inputs", "outputs", "after", "blocks", "timeout", "run"}

var runnerAgent = regexp.MustCompile(`^agent:[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// parseChecks checks the [[check]] tables (skip: the indices a newer vx's
// key switched off) and returns them with their problems.
func parseChecks(cs []tomlCheck, loc locator, skip map[int]bool) ([]CheckDef, []Problem) {
	var out []CheckDef
	var probs []Problem
	names := map[string]bool{}
	for i, tc := range cs {
		if skip[i] {
			continue
		}
		line := func(key string) int { return loc.at("check", i, key) }
		label := fmt.Sprintf("[[check]] #%d", i+1)
		name := strings.TrimSpace(tc.Name)
		if name != "" {
			label = fmt.Sprintf("[[check]] %q", name)
		}
		d := CheckDef{Name: name, Origin: OriginCheck, Line: line(""), Run: strings.TrimSpace(tc.Run), Runner: strings.TrimSpace(tc.Runner)}
		switch {
		case name == "":
			probs = append(probs, Problem{line(""), label + `: name is missing (e.g. name = "unit"); vx check report NAME uses it`})
		case !ruleName.MatchString(name):
			probs = append(probs, Problem{line("name"), label + ": name may use letters, digits, '-', '_' and '.' only"})
		case names[name]:
			probs = append(probs, Problem{line("name"), label + ": a [[check]] of that name is already defined"})
		}
		names[name] = true

		switch w := When(strings.TrimSpace(tc.When)); w {
		case "":
			d.When = WhenLand
		case WhenLand, WhenShip, WhenManual:
			d.When = w
		default:
			probs = append(probs, Problem{line("when"), fmt.Sprintf(`%s: when %q is not one of "land", "ship" or "manual"`, label, tc.When)})
		}
		switch b := Blocks(strings.TrimSpace(tc.Blocks)); b {
		case "":
			switch d.When {
			case WhenShip:
				d.Blocks = BlocksShip
			case WhenManual:
				d.Blocks = BlocksNothing
			default:
				d.Blocks = BlocksLand
			}
		case BlocksLand, BlocksShip, BlocksNothing:
			d.Blocks = b
		default:
			probs = append(probs, Problem{line("blocks"), fmt.Sprintf(`%s: blocks %q is not one of "land", "ship" or "nothing"`, label, tc.Blocks)})
		}
		if d.When == WhenShip && d.Blocks == BlocksLand {
			probs = append(probs, Problem{line("blocks"), label + `: a check that runs at ship (when = "ship") cannot block land; use blocks = "ship", or when = "land"`})
		}

		switch {
		case d.Runner == "", d.Runner == "ci", d.Runner == "human", runnerAgent.MatchString(d.Runner):
		default:
			probs = append(probs, Problem{line("runner"), fmt.Sprintf(`%s: runner %q is not "ci", "human" or "agent:KIND" (e.g. "agent:browser")`, label, tc.Runner)})
		}
		if d.Run == "" && d.Runner == "" {
			probs = append(probs, Problem{line(""), label + `: say how it runs: run = "COMMAND" (the landing agent runs it with vx land --run) or runner = "agent:KIND", "ci" or "human" (who picks it up and reports)`})
		}
		switch rp := Reporter(strings.TrimSpace(tc.Reporter)); rp {
		case "":
			switch d.Runner {
			case "ci":
				d.Reporter = ReporterCI
			case "human":
				d.Reporter = ReporterHuman
			default:
				d.Reporter = ReporterSelf
			}
		case ReporterSelf, ReporterIndependent, ReporterCI, ReporterHuman:
			d.Reporter = rp
		default:
			probs = append(probs, Problem{line("reporter"), fmt.Sprintf(`%s: reporter %q is not one of "self", "independent", "ci" or "human"`, label, tc.Reporter)})
		}

		for _, g := range tc.Paths {
			g = strings.TrimSpace(g)
			if _, err := CompileGlob(g); err != nil {
				probs = append(probs, Problem{line("paths"), label + ": paths: " + err.Error()})
				continue
			}
			d.Paths = append(d.Paths, g)
		}
		for _, in := range tc.Inputs {
			d.Inputs = append(d.Inputs, strings.TrimSpace(in)) // checked by ValidatePipeline
		}
		for _, o := range tc.Outputs.Required {
			o = strings.TrimSpace(o)
			if o == "" || strings.ContainsAny(o, "/\\") {
				probs = append(probs, Problem{line("outputs"), fmt.Sprintf("%s: outputs.required %q must be a file name (e.g. \"steps.json\") or a data key", label, o)})
				continue
			}
			d.Outputs.Required = append(d.Outputs.Required, o)
		}
		for _, g := range tc.Outputs.Artifacts {
			g = strings.TrimSpace(g)
			if _, err := CompileGlob(g); err != nil || strings.Contains(g, "/") {
				msg := "artifact names have no directories"
				if err != nil {
					msg = err.Error()
				}
				probs = append(probs, Problem{line("outputs"), fmt.Sprintf("%s: outputs.artifacts %q: %s", label, g, msg)})
				continue
			}
			d.Outputs.Artifacts = append(d.Outputs.Artifacts, g)
		}
		for _, a := range tc.After {
			d.After = append(d.After, strings.TrimSpace(a)) // checked by ValidatePipeline
		}
		if tc.AllowSP && d.Reporter != ReporterIndependent {
			probs = append(probs, Problem{line("allow_same_person"), label + `: allow_same_person only applies to reporter = "independent"`})
		}
		d.AllowSamePerson = tc.AllowSP
		if t := strings.TrimSpace(tc.Timeout); t != "" {
			dur, err := time.ParseDuration(t)
			switch {
			case err != nil:
				probs = append(probs, Problem{line("timeout"), fmt.Sprintf(`%s: timeout %q is not a duration (e.g. "30m", "1h30m", "90s")`, label, tc.Timeout)})
			case dur <= 0:
				probs = append(probs, Problem{line("timeout"), fmt.Sprintf(`%s: timeout %q must be more than zero`, label, tc.Timeout)})
			default:
				d.Timeout = dur
			}
		}
		out = append(out, d)
	}
	// after and inputs: no cycles, inputs from checks that run before.
	// after may name checks this file does not define: organization
	// checks, or a check a newer vx's key switched off; the server warns
	// about names that are neither.
	for _, p := range ValidatePipeline(StepsOf(out), func(string) bool { return true }) {
		probs = append(probs, Problem{loc.at("check", indexOf(cs, p.Check), "after"), p.String()})
	}
	return out, probs
}

func indexOf(cs []tomlCheck, name string) int {
	for i, c := range cs {
		if strings.TrimSpace(c.Name) == name {
			return i
		}
	}
	return 0
}

// StepsOf is what pipelines need of checks (pipeline.go).
func StepsOf(defs []CheckDef) []Step {
	out := make([]Step, len(defs))
	for i, d := range defs {
		out[i] = Step{Name: d.Name, After: d.After, Inputs: d.Inputs}
	}
	return out
}

// ValidateChecks checks a set of checks that apply together, for example
// a repository's [[check]] tables and its organization's rules: names are
// unique, and after and inputs pass ValidatePipeline (every after names a
// check of the set). The problems name the check and its origin.
func ValidateChecks(defs ...[]CheckDef) error {
	var all []CheckDef
	seen := map[string]CheckDef{}
	var probs []Problem
	for _, set := range defs {
		for _, d := range set {
			if prev, ok := seen[d.Name]; ok {
				probs = append(probs, Problem{d.Line, fmt.Sprintf("%s: %s already defines a check of that name", d.Describe(), prev.Describe())})
				continue
			}
			seen[d.Name] = d
			all = append(all, d)
		}
	}
	for _, p := range ValidatePipeline(StepsOf(all), nil) {
		probs = append(probs, Problem{seen[p.Check].Line, p.String()})
	}
	if len(probs) > 0 {
		return &Error{Problems: probs}
	}
	return nil
}

// ParseChecks parses TOML that holds only [[check]] tables, with the same
// rules as vx.toml (organization rules use it). Unknown keys are errors
// here: an organization's rules are written for this server.
func ParseChecks(data []byte) ([]CheckDef, error) {
	var f struct {
		Check []tomlCheck `toml:"check"`
	}
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, &Error{Problems: []Problem{{Line: pe.Position.Line, Msg: pe.Message}}}
		}
		return nil, &Error{Problems: []Problem{typeProblem(err)}}
	}
	loc := locate(data)
	var probs []Problem
	for _, k := range md.Undecoded() {
		probs = append(probs, Problem{Line: loc.find(strings.Join([]string(k[:len(k)-1]), "."), k[len(k)-1]),
			Msg: fmt.Sprintf("unknown key %q (known in [[check]]: %s)", k.String(), strings.Join(checkKeys, ", "))})
	}
	defs, ps := parseChecks(f.Check, loc, nil)
	probs = append(probs, ps...)
	if len(probs) > 0 {
		sort.SliceStable(probs, func(i, j int) bool { return probs[i].Line < probs[j].Line })
		return nil, &Error{Problems: probs}
	}
	return defs, nil
}

// checkLabel names the idx-th [[check]] for notices about keys a newer vx
// added.
func checkLabel(cs []tomlCheck, idx int) (string, bool) {
	if idx < len(cs) && strings.TrimSpace(cs[idx].Name) != "" {
		return fmt.Sprintf("[[check]] %q", strings.TrimSpace(cs[idx].Name)), true
	}
	return "", false
}

// FormatTimeout writes a timeout as vx.toml would ("30m", "1h30m", "90s").
func FormatTimeout(d time.Duration) string {
	if d == 0 {
		return ""
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
