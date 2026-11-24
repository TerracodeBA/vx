package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// regenTimeout bounds one [[generated]] command.
const regenTimeout = 10 * time.Minute

// regenResult is one [[generated]] command vx ran (or could not run) for
// generated files a merge kept from one side.
type regenResult struct {
	Command string   `json:"command"`
	Paths   []string `json:"paths"`
	Ran     bool     `json:"ran"`
	OK      bool     `json:"ok"`
	Seconds float64  `json:"seconds,omitempty"`
	// Problem says why the files still need regenerating: no command,
	// not on PATH, --no-generate, conflict markers, or the failure.
	Problem string `json:"problem,omitempty"`
	// Output is the end of a failed command's output.
	Output []string `json:"output,omitempty"`
}

// regenerate runs the vx.toml [[generated]] command of each generated file
// a merge kept from one side (vx sync, a land the server merged, a
// rebase), once per command, in the working copy. It says what it ran and
// returns the files that still need regenerating: the command is unknown,
// not on PATH, failed, --no-generate is set, or files still have conflict
// markers (a generator reads its sources; they must be resolved first).
func (a *App) regenerate(ctx context.Context, wc *workcopy.WorkingCopy, files []engine.GeneratedFile) ([]regenResult, []engine.GeneratedFile) {
	if len(files) == 0 {
		return nil, nil
	}
	match := diskGeneratedMatcher(wc)
	byCmd := map[string][]engine.GeneratedFile{}
	var order []string
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.Path] {
			continue
		}
		seen[f.Path] = true
		if f.Command == "" && match != nil {
			f.Command, _ = match(f.Path)
		}
		if _, ok := byCmd[f.Command]; !ok {
			order = append(order, f.Command)
		}
		byCmd[f.Command] = append(byCmd[f.Command], f)
	}
	sort.Strings(order)

	blocked := ""
	if a.noGenerate {
		blocked = "--no-generate"
	} else if _, changes, err := wc.Snapshot(ctx); err == nil {
		if marked, err := filesWithMarkers(ctx, wc, changes); err == nil && len(marked) > 0 {
			blocked = "conflict markers remain in " + strings.Join(marked, ", ")
		}
	}

	var results []regenResult
	var remaining []engine.GeneratedFile
	for _, command := range order {
		fs := byCmd[command]
		r := regenResult{Command: command}
		for _, f := range fs {
			r.Paths = append(r.Paths, f.Path)
		}
		switch {
		case command == "":
			r.Problem = "no [[generated]] rule in vx.toml says which command regenerates it"
		case blocked != "":
			r.Problem = blocked
		default:
			if prog := commandProgram(command); prog != "" {
				if _, err := exec.LookPath(prog); err != nil {
					r.Problem = "`" + prog + "` is not on PATH"
					break
				}
			}
			r.Ran = true
			start := time.Now()
			out, err := a.runGenerator(ctx, wc.Root, command)
			r.Seconds = time.Since(start).Seconds()
			if err != nil {
				r.Problem = "`" + command + "` failed (" + err.Error() + ")"
				r.Output, _ = tailLines(out, 10)
				break
			}
			r.OK = true
		}
		if r.OK {
			a.printf("Regenerated %s: ran `%s` (%.1fs).\n", strings.Join(r.Paths, ", "), command, r.Seconds)
		} else {
			remaining = append(remaining, fs...)
		}
		results = append(results, r)
	}
	return results, remaining
}

// printRegenProblems says, for each command vx could not run or that
// failed, why and exactly what to run.
func (a *App) printRegenProblems(results []regenResult) {
	for _, r := range results {
		if r.OK || r.Command == "" {
			continue
		}
		if strings.HasPrefix(r.Problem, "conflict markers") {
			a.printf("  Not regenerated yet: %s. Resolve them, then run `%s`.\n", r.Problem, r.Command)
			continue
		}
		if len(r.Output) > 0 {
			a.printf("  Not regenerated: %s:\n", r.Problem)
			for _, l := range r.Output {
				a.printf("    %s\n", l)
			}
			a.printf("  Fix that, then run `%s` at the root of the working copy.\n", r.Command)
			continue
		}
		a.printf("  Not regenerated: %s. Run `%s` at the root of the working copy.\n", r.Problem, r.Command)
	}
}

func (a *App) runGenerator(ctx context.Context, dir, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, regenTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Dir = dir
	ownProcessGroup(c)
	c.WaitDelay = 5 * time.Second
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	return buf.String(), err
}

// commandProgram is the program a shell command line starts: its first
// word after any VAR=value assignments; empty when that is not a plain
// word (a subshell, a quoted name).
func commandProgram(command string) string {
	for _, w := range strings.Fields(command) {
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") {
			continue
		}
		if strings.ContainsAny(w, `"'(){}$;&|<>`+"`") {
			return ""
		}
		return w
	}
	return ""
}

// diskGeneratedMatcher is the [[generated]] matcher of the vx.toml in the
// working copy as it is on disk; nil without one.
func diskGeneratedMatcher(wc *workcopy.WorkingCopy) func(string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(wc.Root, rules.File))
	if err != nil {
		return nil
	}
	r, err := rules.Parse(data)
	if err != nil || len(r.Generated) == 0 {
		return nil
	}
	return r.GeneratedMatcher()
}

// generatedOf picks the generated-file warnings out of a land's warnings.
func generatedOf(ws []*vxv1.Warning) []engine.GeneratedFile {
	var out []engine.GeneratedFile
	for _, w := range ws {
		if engine.WarningKind(w.Kind) == engine.WarningGenerated {
			out = append(out, engine.GeneratedFile{Path: w.Path})
		}
	}
	return out
}

// regeneratedPaths is the set of files the results regenerated.
func regeneratedPaths(results []regenResult) map[string]bool {
	out := map[string]bool{}
	for _, r := range results {
		if r.OK {
			for _, p := range r.Paths {
				out[p] = true
			}
		}
	}
	return out
}

// regenerateRebase regenerates the generated files of a stopped rebase
// written into this working copy, once no file has conflict markers, and
// remembers which it did.
func (a *App) regenerateRebase(ctx context.Context, wc *workcopy.WorkingCopy) error {
	r := wc.Config.Rebase
	if r == nil || len(r.Generated) == 0 {
		return nil
	}
	var files []engine.GeneratedFile
	for _, p := range r.Generated {
		files = append(files, engine.GeneratedFile{Path: p})
	}
	results, _ := a.regenerate(ctx, wc, files)
	for _, res := range results {
		if !res.OK && res.Command != "" && strings.HasPrefix(res.Problem, "conflict markers") {
			a.printf("vx effort rebase --continue regenerates %s with `%s` once the conflict markers are gone.\n", strings.Join(res.Paths, ", "), res.Command)
		}
	}
	var others []regenResult
	for _, res := range results {
		if !strings.HasPrefix(res.Problem, "conflict markers") {
			others = append(others, res)
		}
	}
	a.printRegenProblems(others)
	done := regeneratedPaths(results)
	if len(done) == 0 {
		return nil
	}
	var keep []string
	for _, p := range r.Generated {
		if done[p] {
			r.Regenerated = append(r.Regenerated, p)
		} else {
			keep = append(keep, p)
		}
	}
	r.Generated = keep
	return wc.Save()
}

// regenerateRebased regenerates, after a finished rebase, the generated
// files its replayed changes kept from one side (their warnings), and says
// what to do before shipping; empty when nothing is left to do.
func (a *App) regenerateRebased(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, replayed int) string {
	if replayed <= 0 {
		return ""
	}
	log, err := c.Efforts.Log(ctx, connect.NewRequest(&vxv1.LogRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort, Limit: int32(replayed)}))
	if err != nil {
		return ""
	}
	var files []engine.GeneratedFile
	for _, cs := range log.Msg.Changesets {
		files = append(files, generatedOf(cs.Warnings)...)
	}
	if len(files) == 0 {
		return ""
	}
	results, stale := a.regenerate(ctx, wc, files)
	for _, w := range engine.GeneratedWarnings(stale) {
		a.printf("Warning: %s\n", w.Detail)
	}
	a.printRegenProblems(results)
	if len(stale) > 0 {
		return "regenerate the generated files above, land them, then vx ship plan"
	}
	if changes, err := wc.Status(ctx); err == nil && len(changes) > 0 {
		return `land the regenerated files (vx land -m "Regenerate generated files"), then vx ship plan`
	}
	return ""
}
