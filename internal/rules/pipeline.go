package rules

// Pipelines: a [[check]] with after = ["unit"] runs only once unit passed
// on the same tree, and its inputs may name what an earlier check reported:
//
//	inputs = ["build.preview_url", "build.artifact:app.zip"]
//
// "build.preview_url" is the data field preview_url of build's result,
// "build.artifact:app.zip" its artifact app.zip. A name without a dot
// ("diff", "changed_files") is something vx provides itself.
//
// The functions here are pure: they see check names, their after lists
// and inputs, and the results on one tree, never the store. The server's
// status, gates, job queue and web pages all read the same answers.

import (
	"fmt"
	"sort"
	"strings"
)

// Step is what a pipeline needs of a check: its name, the checks it runs
// after and its inputs.
type Step struct {
	Name   string
	After  []string
	Inputs []string
}

// InputRef is one entry of a check's inputs.
type InputRef struct {
	Raw string
	// Check is the earlier check whose output this is; empty for an input
	// vx provides (diff, changed_files, ...).
	Check string
	// Field is the data field of Check's result (build.preview_url).
	Field string
	// Artifact is the artifact name of Check's result
	// (build.artifact:app.zip).
	Artifact string
}

// IsOutput reports whether the input is an earlier check's output.
func (r InputRef) IsOutput() bool { return r.Check != "" }

// ParseInput reads one inputs entry: NAME (provided by vx),
// CHECK.FIELD (a data field of CHECK's result) or CHECK.artifact:NAME (an
// artifact of CHECK's result).
func ParseInput(s string) (InputRef, error) {
	s = strings.TrimSpace(s)
	ref := InputRef{Raw: s}
	if s == "" {
		return ref, fmt.Errorf("an input is empty")
	}
	check, rest, ok := strings.Cut(s, ".")
	if !ok {
		if !ruleName.MatchString(s) {
			return ref, fmt.Errorf("input %q: use NAME, CHECK.FIELD or CHECK.artifact:FILE", s)
		}
		return ref, nil
	}
	if check == "" || !ruleName.MatchString(check) {
		return ref, fmt.Errorf("input %q: %q is not a check name", s, check)
	}
	ref.Check = check
	if name, ok := strings.CutPrefix(rest, "artifact:"); ok {
		if name == "" || strings.ContainsAny(name, "/\\") {
			return ref, fmt.Errorf("input %q: name the artifact file after artifact:, e.g. %s.artifact:app.zip", s, check)
		}
		ref.Artifact = name
		return ref, nil
	}
	if rest == "" || !ruleName.MatchString(rest) || strings.Contains(rest, ":") {
		return ref, fmt.Errorf("input %q: after the check name give a data field (%s.preview_url) or an artifact (%s.artifact:app.zip)", s, check, check)
	}
	ref.Field = rest
	return ref, nil
}

// PipelineProblem is one thing wrong with the after and inputs of a set of
// checks.
type PipelineProblem struct {
	Check string // the check the problem is on
	Msg   string
}

func (p PipelineProblem) String() string { return fmt.Sprintf("[[check]] %q: %s", p.Check, p.Msg) }

// ValidatePipeline checks the after and inputs of steps: no check runs
// after itself or after a check that (through others) runs after it, and
// an input CHECK.FIELD names a check this one runs after, directly or
// through others. Names in after that are not in steps are problems unless
// known says they exist elsewhere (organization checks, for a vx.toml);
// known may be nil.
func ValidatePipeline(steps []Step, known func(name string) bool) []PipelineProblem {
	byName := make(map[string]Step, len(steps))
	for _, s := range steps {
		if _, dup := byName[s.Name]; !dup {
			byName[s.Name] = s
		}
	}
	exists := func(n string) bool {
		_, ok := byName[n]
		return ok || (known != nil && known(n))
	}
	var probs []PipelineProblem
	for _, s := range steps {
		seen := map[string]bool{}
		for _, a := range s.After {
			switch {
			case a == s.Name:
				probs = append(probs, PipelineProblem{s.Name, "after names the check itself"})
			case seen[a]:
				probs = append(probs, PipelineProblem{s.Name, fmt.Sprintf("after names %q twice", a)})
			case !exists(a):
				probs = append(probs, PipelineProblem{s.Name, fmt.Sprintf("after names %q, which is not a check here", a)})
			}
			seen[a] = true
		}
	}
	for _, cyc := range cycles(steps, byName) {
		probs = append(probs, PipelineProblem{cyc[0], "after makes a cycle: " + strings.Join(append(cyc, cyc[0]), " -> ") + "; a check cannot wait for itself"})
	}
	for _, s := range steps {
		up := upstream(s.Name, byName)
		for _, in := range s.Inputs {
			ref, err := ParseInput(in)
			if err != nil {
				probs = append(probs, PipelineProblem{s.Name, err.Error()})
				continue
			}
			if !ref.IsOutput() {
				continue
			}
			if ref.Check == s.Name {
				probs = append(probs, PipelineProblem{s.Name, fmt.Sprintf("input %q is the check's own output", in)})
				continue
			}
			if _, local := byName[ref.Check]; !local && known != nil && known(ref.Check) {
				if !contains(s.After, ref.Check) && !up[ref.Check] {
					probs = append(probs, PipelineProblem{s.Name, fmt.Sprintf("input %q needs after = [%q] so %s has run first", in, ref.Check, ref.Check)})
				}
				continue
			}
			if !up[ref.Check] {
				if exists(ref.Check) {
					probs = append(probs, PipelineProblem{s.Name, fmt.Sprintf("input %q needs after = [%q] so %s has run first", in, ref.Check, ref.Check)})
				} else {
					probs = append(probs, PipelineProblem{s.Name, fmt.Sprintf("input %q names %q, which is not a check here", in, ref.Check)})
				}
			}
		}
	}
	return probs
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// upstream is every check name runs after, directly or through others.
func upstream(name string, byName map[string]Step) map[string]bool {
	out := map[string]bool{}
	var walk func(n string)
	walk = func(n string) {
		for _, a := range byName[n].After {
			if !out[a] {
				out[a] = true
				walk(a)
			}
		}
	}
	walk(name)
	delete(out, name)
	return out
}

// cycles finds the cycles of after (each once, starting at its first check
// in file order).
func cycles(steps []Step, byName map[string]Step) [][]string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var out [][]string
	reported := map[string]bool{}
	var visit func(n string)
	visit = func(n string) {
		color[n] = grey
		stack = append(stack, n)
		for _, a := range byName[n].After {
			if _, ok := byName[a]; !ok || a == n {
				continue
			}
			switch color[a] {
			case white:
				visit(a)
			case grey:
				i := len(stack) - 1
				for i >= 0 && stack[i] != a {
					i--
				}
				cyc := append([]string(nil), stack[i:]...)
				// Report the cycle once, from the check written first.
				first := 0
				for j, c := range cyc {
					if order(steps, c) < order(steps, cyc[first]) {
						first = j
					}
				}
				cyc = append(cyc[first:], cyc[:first]...)
				key := strings.Join(sortedCopy(cyc), "\x00")
				if !reported[key] {
					reported[key] = true
					// Read in the direction of waiting: a waits for b.
					out = append(out, cyc)
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}
	for _, s := range steps {
		if color[s.Name] == white {
			visit(s.Name)
		}
	}
	return out
}

func order(steps []Step, name string) int {
	for i, s := range steps {
		if s.Name == name {
			return i
		}
	}
	return len(steps)
}

func sortedCopy(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// Result states of a check on one tree, as pipelines see them.
const (
	ResultPass    = "pass"
	ResultFail    = "fail"
	ResultNeutral = "neutral"
)

// Pipeline states of a check on one tree.
const (
	StatePassed  = "passed"  // a passing result
	StateFailed  = "failed"  // a failing result
	StateNeutral = "neutral" // a neutral result: done, neither pass nor fail
	StateReady   = "ready"   // every check it runs after is done; waits for its result
	StateWaiting = "waiting" // some check it runs after has no result yet
	StateSkipped = "skipped" // a check it runs after failed or was skipped: it will not run on this tree
	StateNotHere = "not needed"
)

// StepState is one check's place in the pipeline of one tree.
type StepState struct {
	Name  string
	Depth int // 0 for a check that runs after nothing; else 1 + the deepest check it runs after
	// Parent is the check it is shown under (its deepest after), empty at
	// depth 0; After are all of them.
	Parent string
	After  []string
	State  string
	// Reason says why a check waits or was skipped, for people:
	//
	//	waits for unit
	//	skipped: unit failed
	//	skipped: build was skipped (unit failed)
	Reason string
	cause  string // for a skipped check: the failure that started it ("unit failed")
}

// Pipeline places every step on one tree: results maps a check name to its
// result there (pass, fail or neutral; missing: no result yet), applies
// reports whether a check applies to the change at all (nil: every one
// does). A check that does not apply counts as done for the checks that run
// after it. Neutral counts as done too: only a failure stops a pipeline.
//
// The states come in dependency order (a check after every check it runs
// after, otherwise in the order of steps), each under its deepest parent,
// so printing them indented by Depth draws the pipeline. Names in after
// that are not steps are ignored (ValidatePipeline reports them); a cycle
// leaves its checks waiting.
func Pipeline(steps []Step, results map[string]string, applies func(name string) bool) []StepState {
	byName := make(map[string]Step, len(steps))
	for _, s := range steps {
		if _, dup := byName[s.Name]; !dup {
			byName[s.Name] = s
		}
	}
	st := map[string]*StepState{}
	var resolve func(n string, path map[string]bool) *StepState
	resolve = func(n string, path map[string]bool) *StepState {
		if x, ok := st[n]; ok {
			return x
		}
		s := byName[n]
		x := &StepState{Name: n}
		if path[n] { // a cycle: waits forever, ValidatePipeline refuses it
			x.State, x.Reason = StateWaiting, "after makes a cycle"
			return x
		}
		path[n] = true
		defer delete(path, n)
		var waitFor, failed, skipped []string
		cause := ""
		for _, a := range s.After {
			if _, ok := byName[a]; !ok || a == n {
				continue
			}
			x.After = append(x.After, a)
			p := resolve(a, path)
			if p.Depth+1 > x.Depth {
				x.Depth, x.Parent = p.Depth+1, a
			}
			switch p.State {
			case StateFailed:
				failed = append(failed, a)
			case StateSkipped:
				skipped = append(skipped, a)
				if cause == "" {
					cause = p.cause
				}
			case StatePassed, StateNeutral, StateNotHere:
			default:
				waitFor = append(waitFor, a)
			}
		}
		r, has := results[n]
		switch {
		case applies != nil && !applies(n):
			x.State, x.Reason = StateNotHere, "does not apply to this change"
		case has && r == ResultPass:
			x.State = StatePassed
		case has && r == ResultFail:
			x.State = StateFailed
		case has:
			x.State = StateNeutral
		case len(failed) > 0:
			x.cause = joinAnd(failed) + " failed"
			x.State, x.Reason = StateSkipped, "skipped: "+x.cause
		case len(skipped) > 0:
			x.cause = cause
			x.State, x.Reason = StateSkipped, "skipped: "+joinAnd(skipped)+" was skipped ("+cause+")"
		case len(waitFor) > 0:
			x.State, x.Reason = StateWaiting, "waits for "+joinAnd(waitFor)
		default:
			x.State = StateReady
		}
		st[n] = x
		return x
	}
	for _, s := range steps {
		resolve(s.Name, map[string]bool{})
	}
	// Order: depth-first from the roots, children in steps order under
	// their parent, so each check prints below the one it is shown under.
	children := map[string][]string{}
	var roots []string
	done := map[string]bool{}
	for _, s := range steps {
		if done[s.Name] {
			continue
		}
		done[s.Name] = true
		if p := st[s.Name].Parent; p != "" {
			children[p] = append(children[p], s.Name)
		} else {
			roots = append(roots, s.Name)
		}
	}
	var out []StepState
	emitted := map[string]bool{}
	var emit func(n string)
	emit = func(n string) {
		if emitted[n] {
			return
		}
		emitted[n] = true
		out = append(out, *st[n])
		for _, c := range children[n] {
			emit(c)
		}
	}
	for _, r := range roots {
		emit(r)
	}
	for _, s := range steps { // cycles have no root
		emit(s.Name)
	}
	return out
}

// PipelineReady lists the checks of steps that may run now on a tree:
// those without a result whose every after is done (see Pipeline). On a
// new tree, without results, these are the checks that run after nothing
// (or only after checks that do not apply).
func PipelineReady(steps []Step, results map[string]string, applies func(name string) bool) []string {
	var out []string
	for _, s := range Pipeline(steps, results, applies) {
		if s.State == StateReady {
			out = append(out, s.Name)
		}
	}
	return out
}

// PipelineSkipped lists the checks of steps that will not run on a tree
// because a check they run after failed, with the reason.
func PipelineSkipped(steps []Step, results map[string]string, applies func(name string) bool) map[string]string {
	out := map[string]string{}
	for _, s := range Pipeline(steps, results, applies) {
		if s.State == StateSkipped {
			out[s.Name] = s.Reason
		}
	}
	return out
}

// Output is one named output of a check's result, for the inputs of the
// checks after it.
type Output struct {
	Data      map[string]string // data fields
	Artifacts map[string]string // artifact file name -> where to get it (a URL or a blob reference)
}

// ResolveInputs gives the values of a step's inputs that name earlier
// checks' outputs, keyed by the input as written ("build.preview_url"),
// from outputs (check name -> its result's outputs on this tree). Inputs
// vx provides itself are left to the caller. missing lists the inputs
// whose check has no such field or artifact.
func ResolveInputs(s Step, outputs map[string]Output) (values map[string]string, missing []string) {
	values = map[string]string{}
	for _, in := range s.Inputs {
		ref, err := ParseInput(in)
		if err != nil || !ref.IsOutput() {
			continue
		}
		o := outputs[ref.Check]
		var v string
		var ok bool
		if ref.Artifact != "" {
			v, ok = o.Artifacts[ref.Artifact]
		} else {
			v, ok = o.Data[ref.Field]
		}
		if ok {
			values[ref.Raw] = v
		} else {
			missing = append(missing, ref.Raw)
		}
	}
	return values, missing
}
