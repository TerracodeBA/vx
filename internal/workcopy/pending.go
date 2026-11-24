package workcopy

import (
	"regexp"
	"strings"
)

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_./:=,+@%-]+$`)

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if plainArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Command is the vx land command line the pending land stands for.
func (p PendingLand) Command() string {
	parts := []string{"vx land"}
	add := func(flag string, vals []string) {
		for _, v := range vals {
			parts = append(parts, flag+" "+shellQuote(v))
		}
	}
	add("--run", p.Runs)
	add("--check", p.Checks)
	add("--new-test", p.NewTests)
	add("--depends-on", p.DependsOn)
	if p.AllowFailing {
		parts = append(parts, "--allow-failing")
	}
	if p.AllowUnverified {
		parts = append(parts, "--allow-unverified-tests")
	}
	return strings.Join(parts, " ")
}
