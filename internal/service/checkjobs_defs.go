package service

import "github.com/TerracodeBA/vx/internal/rules"

// jobDefsOf turns a repository's check definitions into what the queue
// needs: the [[check]] tables (the older [checks] required and [[policy]]
// tables have no runner).
func jobDefsOf(rs rules.Rules) []jobDef {
	out := make([]jobDef, 0, len(rs.Checks))
	for _, d := range rs.Checks {
		out = append(out, jobDefOf(d))
	}
	return out
}

// jobDefOf is one check as the queue sees it.
func jobDefOf(d rules.CheckDef) jobDef {
	return jobDef{
		Name: d.Name, When: string(d.When), Paths: d.Paths, Runner: d.Runner, Reporter: string(d.Reporter),
		AllowSamePerson: d.AllowSamePerson, Inputs: d.Inputs, After: d.After, Timeout: d.Timeout,
	}
}
