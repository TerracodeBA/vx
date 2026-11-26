package rules

import (
	"reflect"
	"strings"
	"testing"
)

func names(ss []StepState) []string {
	var out []string
	for _, s := range ss {
		out = append(out, strings.Repeat("  ", s.Depth)+s.Name)
	}
	return out
}

// A pipeline prints each check under the deepest check it runs after,
// in dependency order, whatever the order of the file.
func TestPipelineOrdering(t *testing.T) {
	steps := []Step{
		{Name: "e2e", After: []string{"deploy", "unit"}},
		{Name: "lint"},
		{Name: "deploy", After: []string{"build"}},
		{Name: "build", After: []string{"unit"}},
		{Name: "unit"},
		{Name: "docs", After: []string{"lint"}},
	}
	got := names(Pipeline(steps, nil, nil))
	want := []string{"lint", "  docs", "unit", "  build", "    deploy", "      e2e"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	ss := Pipeline(steps, nil, nil)
	for _, s := range ss {
		if s.Name == "e2e" && (s.Parent != "deploy" || !reflect.DeepEqual(s.After, []string{"deploy", "unit"})) {
			t.Errorf("e2e: parent %q after %v", s.Parent, s.After)
		}
	}
	if got := PipelineReady(steps, nil, nil); !reflect.DeepEqual(got, []string{"lint", "unit"}) {
		t.Errorf("ready on a new tree: %v", got)
	}
	if got := PipelineReady(steps, map[string]string{"unit": ResultPass}, nil); !reflect.DeepEqual(got, []string{"lint", "build"}) {
		t.Errorf("ready after unit: %v", got)
	}
	// Neutral counts as done; e2e still waits for deploy.
	res := map[string]string{"unit": ResultPass, "build": ResultNeutral}
	for _, s := range Pipeline(steps, res, nil) {
		switch s.Name {
		case "deploy":
			if s.State != StateReady {
				t.Errorf("deploy after a neutral build: %+v", s)
			}
		case "e2e":
			if s.State != StateWaiting || s.Reason != "waits for deploy" {
				t.Errorf("e2e: %+v", s)
			}
		}
	}
	// A check that does not apply to the change does not hold back the
	// checks after it.
	notBuild := func(n string) bool { return n != "build" }
	if got := PipelineReady(steps, map[string]string{"unit": ResultPass}, notBuild); !reflect.DeepEqual(got, []string{"lint", "deploy"}) {
		t.Errorf("ready with build not needed: %v", got)
	}
}

// A failure skips every check after it, with the reason, instead of
// leaving them pending.
func TestPipelineSkipOnFailure(t *testing.T) {
	steps := []Step{
		{Name: "unit"},
		{Name: "build", After: []string{"unit"}},
		{Name: "e2e", After: []string{"build"}},
		{Name: "lint"},
	}
	got := map[string]StepState{}
	for _, s := range Pipeline(steps, map[string]string{"unit": ResultFail, "lint": ResultPass}, nil) {
		got[s.Name] = s
	}
	if s := got["unit"]; s.State != StateFailed {
		t.Errorf("unit: %+v", s)
	}
	if s := got["build"]; s.State != StateSkipped || s.Reason != "skipped: unit failed" {
		t.Errorf("build: %+v", s)
	}
	if s := got["e2e"]; s.State != StateSkipped || s.Reason != "skipped: build was skipped (unit failed)" {
		t.Errorf("e2e: %+v", s)
	}
	if s := got["lint"]; s.State != StatePassed {
		t.Errorf("lint: %+v", s)
	}
	skipped := PipelineSkipped(steps, map[string]string{"unit": ResultFail}, nil)
	if len(skipped) != 2 || skipped["e2e"] == "" {
		t.Errorf("skipped: %v", skipped)
	}
	if r := PipelineReady(steps, map[string]string{"unit": ResultFail}, nil); !reflect.DeepEqual(r, []string{"lint"}) {
		t.Errorf("ready after a failure: %v", r)
	}
}

// Cycles, self references, unknown names and inputs from checks that do
// not run first are refused when vx.toml is read.
func TestPipelineCyclesRefused(t *testing.T) {
	probs := ValidatePipeline([]Step{
		{Name: "a", After: []string{"c"}},
		{Name: "b", After: []string{"a"}},
		{Name: "c", After: []string{"b"}},
		{Name: "d", After: []string{"d"}},
		{Name: "e", After: []string{"nope", "a", "a"}},
	}, nil)
	var msgs []string
	for _, p := range probs {
		msgs = append(msgs, p.String())
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{
		`[[check]] "a": after makes a cycle: a -> c -> b -> a; a check cannot wait for itself`,
		`[[check]] "d": after names the check itself`,
		`[[check]] "e": after names "nope", which is not a check here`,
		`[[check]] "e": after names "a" twice`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Count(all, "cycle") != 1 {
		t.Errorf("the cycle is reported once:\n%s", all)
	}
	// Organization checks are known to a vx.toml.
	org := func(n string) bool { return n == "security" }
	if p := ValidatePipeline([]Step{{Name: "e2e", After: []string{"security"}, Inputs: []string{"security.report_url"}}}, org); len(p) != 0 {
		t.Errorf("after an organization check: %v", p)
	}
	// A cyclic pipeline still places every check (waiting), so status
	// never hides one.
	if got := Pipeline([]Step{{Name: "a", After: []string{"b"}}, {Name: "b", After: []string{"a"}}}, nil, nil); len(got) != 2 || got[0].State != StateWaiting {
		t.Errorf("cycle: %+v", got)
	}
}

// Outputs of an earlier check feed the inputs of a later one.
func TestPipelineOutputsIntoInputs(t *testing.T) {
	for in, want := range map[string]InputRef{
		"diff":                   {Raw: "diff"},
		"build.preview_url":      {Raw: "build.preview_url", Check: "build", Field: "preview_url"},
		"build.artifact:app.zip": {Raw: "build.artifact:app.zip", Check: "build", Artifact: "app.zip"},
	} {
		got, err := ParseInput(in)
		if err != nil || got != want {
			t.Errorf("ParseInput(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "build.", ".x", "build.artifact:", "build.artifact:a/b", "a b", "build.x:y"} {
		if _, err := ParseInput(bad); err == nil {
			t.Errorf("ParseInput(%q) accepted", bad)
		}
	}
	steps := []Step{
		{Name: "unit"},
		{Name: "build", After: []string{"unit"}},
		{Name: "e2e", After: []string{"build"}, Inputs: []string{"diff", "build.preview_url", "build.artifact:app.zip", "unit.coverage", "build.missing"}},
	}
	if p := ValidatePipeline(steps, nil); len(p) != 0 {
		t.Fatalf("valid pipeline refused: %v", p)
	}
	vals, missing := ResolveInputs(steps[2], map[string]Output{
		"build": {Data: map[string]string{"preview_url": "https://pr-12.preview.example"}, Artifacts: map[string]string{"app.zip": "blob:3f2a"}},
		"unit":  {Data: map[string]string{"coverage": "81.5"}},
	})
	want := map[string]string{"build.preview_url": "https://pr-12.preview.example", "build.artifact:app.zip": "blob:3f2a", "unit.coverage": "81.5"}
	if !reflect.DeepEqual(vals, want) || !reflect.DeepEqual(missing, []string{"build.missing"}) {
		t.Errorf("ResolveInputs = %v, missing %v", vals, missing)
	}
	// An input from a check that does not run first is refused.
	probs := ValidatePipeline([]Step{
		{Name: "build"},
		{Name: "e2e", Inputs: []string{"build.preview_url", "ghost.url", "e2e.x"}},
	}, nil)
	all := ""
	for _, p := range probs {
		all += p.String() + "\n"
	}
	for _, want := range []string{
		`input "build.preview_url" needs after = ["build"] so build has run first`,
		`input "ghost.url" names "ghost", which is not a check here`,
		`input "e2e.x" is the check's own output`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}
