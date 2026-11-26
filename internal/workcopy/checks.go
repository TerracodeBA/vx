package workcopy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// checksFile holds the passing results of vx land --run commands, keyed by
// the command and the tree of the files it ran on.
const checksFile = "checks.json"

// maxPassedChecks bounds the remembered results; the oldest go first.
const maxPassedChecks = 64

// PassedCheck is a vx land --run command that passed on exactly the files
// of Tree (the working copy snapshot, ignored files excluded).
type PassedCheck struct {
	Command    string    `json:"command"`
	Tree       string    `json:"tree"`
	DurationMs int64     `json:"duration_ms"`
	Notes      string    `json:"notes,omitempty"`
	At         time.Time `json:"at"`
}

type passedChecks struct {
	Passed []PassedCheck `json:"passed"`
	// Durations are how long each command took when it last ran here,
	// passed or failed, in milliseconds: vx land --run runs the cheapest
	// first.
	Durations map[string]int64 `json:"durations,omitempty"`
}

func (wc *WorkingCopy) loadPassedChecks() passedChecks {
	var p passedChecks
	data, err := os.ReadFile(filepath.Join(wc.Root, Dir, checksFile))
	if err != nil {
		return p
	}
	if json.Unmarshal(data, &p) != nil {
		return passedChecks{}
	}
	return p
}

// PassedCheck returns the remembered passing run of command on tree.
func (wc *WorkingCopy) PassedCheck(command, tree string) (PassedCheck, bool) {
	command = strings.TrimSpace(command)
	p := wc.loadPassedChecks()
	for i := len(p.Passed) - 1; i >= 0; i-- {
		if c := p.Passed[i]; c.Command == command && c.Tree == tree {
			return c, true
		}
	}
	return PassedCheck{}, false
}

// RecordPassedCheck remembers that c.Command passed on c.Tree.
func (wc *WorkingCopy) RecordPassedCheck(c PassedCheck) error {
	c.Command = strings.TrimSpace(c.Command)
	p := wc.loadPassedChecks()
	kept := p.Passed[:0]
	for _, old := range p.Passed {
		if old.Command != c.Command || old.Tree != c.Tree {
			kept = append(kept, old)
		}
	}
	kept = append(kept, c)
	if len(kept) > maxPassedChecks {
		kept = kept[len(kept)-maxPassedChecks:]
	}
	p.Passed = kept
	return wc.savePassedChecks(p)
}

func (wc *WorkingCopy) savePassedChecks(p passedChecks) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(wc.Root, Dir, checksFile), append(data, '\n'), 0o644)
}

// CheckDuration is how long command took when it last ran here.
func (wc *WorkingCopy) CheckDuration(command string) (int64, bool) {
	ms, ok := wc.loadPassedChecks().Durations[strings.TrimSpace(command)]
	return ms, ok
}

// RecordCheckDuration remembers how long command took, passed or failed.
func (wc *WorkingCopy) RecordCheckDuration(command string, ms int64) error {
	p := wc.loadPassedChecks()
	if p.Durations == nil {
		p.Durations = map[string]int64{}
	}
	p.Durations[strings.TrimSpace(command)] = ms
	if len(p.Durations) > maxPassedChecks {
		// Forget an arbitrary one; a command missing here only runs later.
		for k := range p.Durations {
			if k != strings.TrimSpace(command) {
				delete(p.Durations, k)
				break
			}
		}
	}
	return wc.savePassedChecks(p)
}

// CheckLogPath is where vx land --run keeps the full output of its nth
// command (1-based) of the last run.
func (wc *WorkingCopy) CheckLogPath(n int) string {
	return filepath.Join(wc.Root, Dir, "last-check-"+strconv.Itoa(n)+".log")
}

// RemoveCheckLogs deletes the check logs of an earlier run.
func (wc *WorkingCopy) RemoveCheckLogs() {
	logs, _ := filepath.Glob(filepath.Join(wc.Root, Dir, "last-check-*.log"))
	for _, l := range logs {
		os.Remove(l)
	}
}
