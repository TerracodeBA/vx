package rules

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Mode is how a landing policy lands changes to its paths.
type Mode string

// Landing modes, from the least to the most strict.
const (
	// ModeFast lands at once and checks afterwards: what vx always did.
	ModeFast Mode = "fast"
	// ModeGated lands only when every required check ran by vx (vx land
	// --run) and passed in this land, on exactly what lands.
	ModeGated Mode = "gated"
	// ModeHuman is gated, and then a person approves before the change
	// lands: until then it waits as a pending review and the effort head
	// does not move.
	ModeHuman Mode = "human"
)

// Modes lists the modes in vx.toml spelling, least strict first.
var Modes = []Mode{ModeFast, ModeGated, ModeHuman}

func (m Mode) rank() int {
	switch m {
	case ModeGated:
		return 1
	case ModeHuman:
		return 2
	}
	return 0
}

// Stricter reports whether m is stricter than o.
func (m Mode) Stricter(o Mode) bool { return m.rank() > o.rank() }

// Policy is one [[policy]] of vx.toml: how changes to Paths land.
type Policy struct {
	Paths []string // globs
	Mode  Mode
	// Require are the checks a gated or human land needs, run by vx and
	// passing: the policy's own require, or [checks] required when the
	// policy does not set it.
	Require []string
	// Approvers limit who approves a human land: "user:HANDLE" or
	// "team:ORG/TEAM". Empty: anyone with write on the repository.
	Approvers []string
	Line      int // line of the [[policy]] header in vx.toml; 0 when not known
}

// Describe names the policy for people:
//
//	[[policy]] paths = ["internal/store/migrations/**"] (vx.toml line 12)
func (p Policy) Describe() string {
	qs := make([]string, len(p.Paths))
	for i, g := range p.Paths {
		qs[i] = fmt.Sprintf("%q", g)
	}
	s := "[[policy]] paths = [" + strings.Join(qs, ", ") + "]"
	if p.Line > 0 {
		return fmt.Sprintf("%s (%s line %d)", s, File, p.Line)
	}
	return s + " (" + File + ")"
}

// Matches reports whether path is under one of the policy's globs.
func (p Policy) Matches(path string) bool {
	for _, g := range p.Paths {
		if c, err := CompileGlob(g); err == nil && c.Match(path) {
			return true
		}
	}
	return false
}

// Decision is the policy that applies to a land: the strictest of the
// policies whose paths match a file the land changes.
type Decision struct {
	Mode Mode // ModeFast when no policy matches
	// Paths are the changed files under the policies of Mode, sorted: the
	// files that make this land gated or human.
	Paths []string
	// Require are the checks of every matching gated or human policy, in
	// vx.toml order without repeats.
	Require []string
	// Approvers has one list per matching human policy that names
	// approvers: a person approves only when they are in every list.
	Approvers [][]string
	// Policies describe the policies of Mode that matched (Policy.Describe).
	Policies []string
}

// Decide returns the policy for a land that changes paths.
func (r Rules) Decide(paths []string) Decision {
	d := Decision{Mode: ModeFast}
	for _, p := range r.Policies {
		var hit []string
		for _, path := range paths {
			if p.Matches(path) {
				hit = append(hit, path)
			}
		}
		if len(hit) == 0 {
			continue
		}
		d = Combine(d, Decision{Mode: p.Mode, Paths: hit, Require: p.Require, Approvers: approverLists(p), Policies: []string{p.Describe()}})
	}
	return d
}

func approverLists(p Policy) [][]string {
	if p.Mode != ModeHuman || len(p.Approvers) == 0 {
		return nil
	}
	return [][]string{p.Approvers}
}

// Combine returns the strictest of decisions, for example the policies of
// the effort head's vx.toml and of the snapshot's: a change can make its
// own policy stricter but never weaker. The checks of every gated or
// human decision add up; the paths, approvers and policies are those of
// the strictest mode.
func Combine(ds ...Decision) Decision {
	out := Decision{Mode: ModeFast}
	for _, d := range ds {
		if d.Mode == "" {
			d.Mode = ModeFast
		}
		if d.Mode.Stricter(out.Mode) {
			out = Decision{Mode: d.Mode, Require: out.Require}
		}
		if d.Mode != ModeFast {
			out.Require = union(out.Require, d.Require)
		}
		if d.Mode != out.Mode {
			continue
		}
		out.Paths = union(out.Paths, d.Paths)
		out.Policies = union(out.Policies, d.Policies)
		for _, l := range d.Approvers {
			if !hasList(out.Approvers, l) {
				out.Approvers = append(out.Approvers, l)
			}
		}
	}
	sort.Strings(out.Paths)
	return out
}

func union(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	out := append([]string(nil), a...)
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func hasList(ls [][]string, l []string) bool {
	for _, x := range ls {
		if strings.Join(x, "\x00") == strings.Join(l, "\x00") {
			return true
		}
	}
	return false
}

// Approver is a parsed approvers entry.
type Approver struct {
	Kind string // "user" or "team"
	Name string // a handle, or ORG/TEAM
}

var approverName = map[string]*regexp.Regexp{
	"user": regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,38}$`),
	"team": regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,38}/[A-Za-z0-9][A-Za-z0-9._-]*$`),
}

// ParseApprover parses "user:HANDLE" or "team:ORG/TEAM".
func ParseApprover(s string) (Approver, error) {
	kind, name, ok := strings.Cut(strings.TrimSpace(s), ":")
	re := approverName[kind]
	if !ok || re == nil {
		return Approver{}, fmt.Errorf("approver %q must be \"user:HANDLE\" or \"team:ORG/TEAM\"", s)
	}
	if !re.MatchString(name) {
		if kind == "team" {
			return Approver{}, fmt.Errorf("approver %q: write a team as team:ORG/TEAM, e.g. team:acme/db", s)
		}
		return Approver{}, fmt.Errorf("approver %q: %q is not a valid handle (lowercase letters, digits, '.', '_', '-')", s, name)
	}
	return Approver{Kind: kind, Name: name}, nil
}

// Missing returns the checks of d that did not run by vx and pass:
// passed are the commands vx ran that passed (compared after trimming
// spaces), failed those it ran that failed. Each entry says which.
func (d Decision) Missing(passed, failed []string) []MissingCheck {
	ok := map[string]bool{}
	for _, c := range passed {
		ok[strings.TrimSpace(c)] = true
	}
	bad := map[string]bool{}
	for _, c := range failed {
		bad[strings.TrimSpace(c)] = true
	}
	var out []MissingCheck
	for _, c := range d.Require {
		switch {
		case ok[c]:
		case bad[c]:
			out = append(out, MissingCheck{Command: c, Failed: true})
		default:
			out = append(out, MissingCheck{Command: c})
		}
	}
	return out
}

// MissingCheck is a required check a gated land lacks.
type MissingCheck struct {
	Command string
	Failed  bool // vx ran it and it failed; otherwise vx did not run it
}

// LandCommand is the vx land that runs every check of d, quoted for sh:
//
//	vx land --run 'go vet ./...' --run 'go test ./...'
func (d Decision) LandCommand() string {
	var b strings.Builder
	b.WriteString("vx land")
	for _, c := range d.Require {
		b.WriteString(" --run ")
		b.WriteString(ShellQuote(c))
	}
	return b.String()
}

// ShellQuote quotes s as one sh word, in single quotes.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
