package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// failTailLines is how much of a failed check's output vx land prints; the
// rest is in .vx/last-check-N.log.
const failTailLines = 30

// movedPoll is how often vx land --run asks whether the effort moved on
// while its checks run.
const movedPoll = 2 * time.Second

// checkOptions say how runChecks treats one pass over the --run commands.
type checkOptions struct {
	// stopIfMoved stops the checks as soon as someone lands in the effort:
	// what they test is no longer what would land.
	stopIfMoved bool
	// reuse takes a remembered passing result of the same command on the
	// same files instead of running it again.
	reuse bool
	// keepGoing runs every command even after one failed; by default the
	// checks stop at the first failure.
	keepGoing bool
}

// checkRun is the outcome of one pass over the --run commands.
type checkRun struct {
	ran    []*vxv1.Check
	failed bool
	// moved: the effort moved on while the checks ran; they were stopped
	// and ran is incomplete.
	moved bool
	// changed are files the checks themselves changed in the working copy.
	changed []string
}

// runChecks runs each command with sh -c in the working copy and records
// the real result. A command that passed before on exactly these files
// (the same working copy tree) is not run again when o.reuse is set: its
// result is reused and says so. Every command's output goes to
// .vx/last-check-N.log; a failure prints its last lines.
func (a *App) runChecks(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, cmds []string, o checkOptions) (checkRun, error) {
	var res checkRun
	tree, _, err := wc.Snapshot(ctx)
	if err != nil {
		return res, err
	}
	wc.RemoveCheckLogs()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var moved, interrupted atomic.Bool
	done := make(chan struct{})
	defer close(done)
	if o.stopIfMoved {
		go func() {
			t := time.NewTicker(movedPoll)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-runCtx.Done():
					return
				case <-t.C:
					if m, err := a.effortMoved(runCtx, c, wc); err == nil && m {
						moved.Store(true)
						cancel()
						return
					}
				}
			}
		}()
	}
	// The commands run in their own process group (to stop all of them
	// when the effort moves), so pass an interrupt on.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-done:
		case <-sigs:
			interrupted.Store(true)
			cancel()
		}
	}()

	var fresh []workcopy.PassedCheck
	var failedNames, notRun []string
	order := cheapestFirst(wc, cmds)
	for _, i := range order {
		command := cmds[i]
		if res.failed && !o.keepGoing {
			notRun = append(notRun, command)
			continue
		}
		if o.reuse {
			if p, ok := wc.PassedCheck(command, tree.String()); ok {
				when := reusedWhen(p.At)
				res.ran = append(res.ran, &vxv1.Check{
					Name: command, Result: "pass", RanByVx: true, DurationMs: p.DurationMs,
					Notes: joinNotes(p.Notes, "reused from "+when+": passed on these exact files"),
				})
				a.printf("pass %s (reused from %s: it passed on these exact files in %.1fs)\n", command, when, float64(p.DurationMs)/1000)
				continue
			}
		}
		start := time.Now()
		cmd := exec.CommandContext(runCtx, "sh", "-c", command)
		cmd.Dir = wc.Root
		ownProcessGroup(cmd)
		cmd.WaitDelay = 5 * time.Second
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		runErr := cmd.Run()
		if interrupted.Load() {
			return res, errors.New("interrupted: stopped the checks; nothing landed")
		}
		if moved.Load() {
			res.moved = true
			return res, nil
		}
		logPath := wc.CheckLogPath(i + 1)
		_ = os.WriteFile(logPath, buf.Bytes(), 0o644)
		ch := &vxv1.Check{Name: command, Result: "pass", RanByVx: true, DurationMs: time.Since(start).Milliseconds(), Notes: cacheNote(buf.String())}
		_ = wc.RecordCheckDuration(command, ch.DurationMs)
		if runErr != nil {
			ch.Result = "fail"
			res.failed = true
			failedNames = append(failedNames, command)
			a.printf("FAIL %s (%.1fs)\n", command, float64(ch.DurationMs)/1000)
			out, total, picked := failureLines(buf.String())
			for _, l := range out {
				a.printf("  %s\n", l)
			}
			switch {
			case picked:
				a.printf("  The failing tests and packages of %d lines; full output: %s\n", total, a.relPath(logPath))
			case total > len(out):
				a.printf("  Last %d of %d lines; full output: %s\n", len(out), total, a.relPath(logPath))
			case total == 0:
				a.printf("  (no output; %v)\n", runErr)
				if inner, ok := testZInner(command); ok {
					a.printf("  Hint: test -z fails when `%s` prints something, and the test swallows what it printed: run `%s` to see it.\n", inner, inner)
				}
			default:
				a.printf("  Full output: %s\n", a.relPath(logPath))
			}
		} else {
			a.printf("pass %s (%.1fs)\n", command, float64(ch.DurationMs)/1000)
			fresh = append(fresh, workcopy.PassedCheck{Command: command, Tree: tree.String(), DurationMs: ch.DurationMs, Notes: ch.Notes, At: start})
		}
		if ch.Notes != "" {
			a.printf("  %s\n  Hint: add -count=1 to run every test instead of using cached results.\n", ch.Notes)
		}
		res.ran = append(res.ran, ch)
	}
	// The evidence keeps the order of the --run flags.
	pos := map[string]int{}
	for i, c := range cmds {
		if _, ok := pos[c]; !ok {
			pos[c] = i
		}
	}
	sort.SliceStable(res.ran, func(i, j int) bool { return pos[res.ran[i].Name] < pos[res.ran[j].Name] })
	if len(failedNames) > 0 && len(cmds) > 1 {
		a.printf("%s\n", failSummary(len(cmds), failedNames, notRun))
	}

	after, _, err := wc.Snapshot(ctx)
	if err != nil {
		return res, err
	}
	if after != tree {
		// A check rewrote files (a formatter, a generator): its result
		// describes the files before, so nothing is remembered.
		changes, err := object.DiffTrees(ctx, wc.Objects, tree, after)
		if err != nil {
			return res, err
		}
		for _, ch := range changes {
			res.changed = append(res.changed, ch.Path)
		}
		return res, nil
	}
	for _, p := range fresh {
		_ = wc.RecordPassedCheck(p)
	}
	return res, nil
}

// reusedWhen is when a reused check ran: "14:02", or "Nov 23 14:02" when
// not today.
func reusedWhen(t time.Time) string {
	t = t.Local()
	if t.Format("2006-01-02") == time.Now().Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

func joinNotes(notes ...string) string {
	var out []string
	for _, n := range notes {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, "; ")
}

// tailLines returns the last n lines of s and how many lines s has.
func tailLines(s string, n int) ([]string, int) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil, 0
	}
	parts := strings.Split(s, "\n")
	total := len(parts)
	if total > n {
		parts = parts[total-n:]
	}
	return parts, total
}

// relPath shows path relative to the directory vx runs in when it is
// inside it, else as is.
func (a *App) relPath(path string) string {
	if rel, err := filepath.Rel(a.dir(), path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// printChangedByChecks warns that the --run commands rewrote files: the
// land takes the files as they are now, the evidence describes them
// before the commands ran.
func (a *App) printChangedByChecks(paths []string) {
	if len(paths) == 0 {
		return
	}
	a.printf("Warning: the checks changed %s here. They land as they are now, but the checks ran on the files before; run vx land again to check the result.\n", strings.Join(paths, ", "))
}

// cheapestFirst returns the positions of cmds in the order to run them:
// by the time each took when it last ran here (.vx/checks.json), fastest
// first, so a failure shows up early; commands never run here go last, in
// the order given.
func cheapestFirst(wc *workcopy.WorkingCopy, cmds []string) []int {
	order := make([]int, len(cmds))
	cost := make([]int64, len(cmds))
	for i, c := range cmds {
		order[i] = i
		if ms, ok := wc.CheckDuration(c); ok {
			cost[i] = ms
		} else {
			cost[i] = 1 << 62
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return cost[order[a]] < cost[order[b]] })
	return order
}

// failSummary is the line after the checks when some failed:
//
//	2 of 4 checks failed: go vet ./..., go test ./...
//	1 of 3 checks failed: go vet ./...; not run after it: go test ./... (--keep-going runs every check)
func failSummary(total int, failed, notRun []string) string {
	s := fmt.Sprintf("%d of %d checks failed: %s", len(failed), total, strings.Join(failed, ", "))
	if len(notRun) > 0 {
		s += fmt.Sprintf("; not run after it: %s (--keep-going runs every check)", strings.Join(notRun, ", "))
	}
	return s
}

var testZ = regexp.MustCompile(`^\s*test\s+-z\s+"\$\((.+)\)"\s*$`)

// testZInner returns CMD of a check written as test -z "$(CMD)", which
// fails without output when CMD prints something.
func testZInner(command string) (string, bool) {
	m := testZ.FindStringSubmatch(command)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// goFailLines bounds the lines failureLines picks from Go test output.
const goFailLines = 80

// failureLines picks what to show of a failed check's output. Go test
// output shows its failing tests (the --- FAIL blocks with their log
// lines), panics and the FAIL lines of each package, and build errors
// (# pkg and the lines under it); anything else shows its last lines.
// picked reports whether the Go lines were picked; total is the number
// of lines of the output.
func failureLines(out string) (lines []string, total int, picked bool) {
	all := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if strings.TrimSpace(out) == "" {
		return nil, 0, false
	}
	total = len(all)
	if !strings.Contains(out, "--- FAIL") && !strings.Contains(out, "\nFAIL\t") && !strings.HasPrefix(out, "FAIL\t") && !strings.Contains(out, "[build failed]") {
		l, n := tailLines(out, failTailLines)
		return l, n, false
	}
	in := false // inside a --- FAIL block, a panic or a build error
	for _, l := range all {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "--- FAIL"), strings.HasPrefix(l, "panic:"), strings.HasPrefix(l, "# "):
			in = true
			lines = append(lines, l)
		case strings.HasPrefix(l, "FAIL"), strings.HasPrefix(l, "ok "), strings.HasPrefix(l, "ok\t"):
			in = false
			if strings.HasPrefix(l, "FAIL") {
				lines = append(lines, l)
			}
		case strings.HasPrefix(t, "=== RUN"), strings.HasPrefix(t, "--- PASS"), strings.HasPrefix(t, "--- SKIP"), strings.HasPrefix(t, "=== PAUSE"), strings.HasPrefix(t, "=== CONT"):
			in = false
		case t == "PASS":
			in = false
		default:
			if in {
				lines = append(lines, l)
			}
		}
	}
	if len(lines) == 0 {
		l, n := tailLines(out, failTailLines)
		return l, n, false
	}
	if len(lines) > goFailLines {
		lines = append(lines[:goFailLines], fmt.Sprintf("... %d more failing lines", len(lines)-goFailLines))
	}
	return lines, total, true
}
