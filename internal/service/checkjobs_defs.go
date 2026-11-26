package service

import "github.com/TerracodeBA/vx/internal/rules"

// jobDefOf is one check as the queue sees it.
func jobDefOf(d rules.CheckDef) jobDef {
	return jobDef{
		Name: d.Name, When: string(d.When), Paths: d.Paths, Runner: d.Runner, Reporter: string(d.Reporter),
		AllowSamePerson: d.AllowSamePerson, Inputs: d.Inputs, After: d.After, Timeout: d.Timeout,
	}
}
