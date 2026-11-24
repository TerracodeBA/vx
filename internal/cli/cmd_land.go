package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// ---------- land ----------

// verifiedAttempts bounds how often land --run syncs and re-runs checks
// when others keep landing first.
const verifiedAttempts = 3

func (a *App) landCmd() *cobra.Command {
	var intent, model, session string
	var checks, runs, dependsOn, newTests []string
	var allowMarkers, upToDate, allowFailing, allowUnverified, fresh bool
	cmd := &cobra.Command{
		Use:   "land",
		Short: "Land your changes into the effort",
		Long: `Land the working copy's changes into the effort, then update the working
copy to the new effort head (your changes plus anything others landed).

Evidence. Prefer --run: vx runs the command itself in the working copy
and records the real result. If the effort moved on, vx first syncs, runs
the commands on the merged result and lands only that, so the evidence
always describes exactly what lands:
  vx land --run "go test ./..." --run "go vet ./..."

--check records a result you report yourself; it is marked as reported:
  vx land --check "go test ./...=pass:142"

Without --run, if others landed in the meantime the server merges your
changes onto theirs. The change is then marked as merged, and its evidence
as gathered before the merge. Only real conflicts stop a land: run vx
sync, fix the conflicting files and land again.

If a land stops (conflicts, failing checks), vx keeps its evidence flags
(--run, --check, --new-test, --depends-on). A later vx land without any of
them in the same workspace reuses them and says so; --fresh ignores them.

vx land always ends with one line saying what happened, after the board
feed: "Landed HASH (CHANGE): INTENT", or "Not landed: ..." with the exit
code (2: conflicts or the effort moved on), so vx land ... | tail -1 keeps
the result.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("vx land takes no arguments, got %q; did you mean -m %q? (-m sets the intent; without it the open workspace's intent is used)", strings.Join(args, " "), strings.Join(args, " "))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			if wc.Config.Rebase != nil {
				return errRebasing(wc)
			}
			ctx := ctxOf(cmd)
			if wc.Config.Workspace == "" {
				if intent == "" {
					return errors.New(`no workspace open: run vx new "INTENT" first, or pass -m "INTENT"`)
				}
				if _, err := a.openWorkspace(ctx, c, wc, intent); err != nil {
					return err
				}
			}
			given := len(runs)+len(checks)+len(newTests)+len(dependsOn) > 0
			if p := wc.Config.PendingLand; p != nil && p.Workspace != wc.Config.Workspace {
				wc.Config.PendingLand = nil
			}
			if p := wc.Config.PendingLand; p != nil && !given && !fresh {
				runs, checks, newTests, dependsOn = p.Runs, p.Checks, p.NewTests, p.DependsOn
				allowFailing = allowFailing || p.AllowFailing
				allowUnverified = allowUnverified || p.AllowUnverified
				a.printf("Reusing the evidence of your interrupted land: %s\n", p.Command())
			}
			if given || (wc.Config.PendingLand != nil && !fresh) {
				// Kept until a land in this workspace succeeds.
				wc.Config.PendingLand = &workcopy.PendingLand{
					Workspace: wc.Config.Workspace, Runs: runs, Checks: checks, NewTests: newTests,
					DependsOn: dependsOn, AllowFailing: allowFailing, AllowUnverified: allowUnverified,
				}
			} else {
				wc.Config.PendingLand = nil
			}
			if err := wc.Save(); err != nil {
				return err
			}
			tests := splitNewTests(newTests)
			reported, err := parseChecks(checks, tests)
			if err != nil {
				return err
			}
			if len(tests) > 0 && !allowUnverified {
				_, changes, err := wc.Snapshot(ctx)
				if err != nil {
					return err
				}
				if err := verifyNewTests(ctx, wc, changes, tests); err != nil {
					return err
				}
			}
			opts := landOptions{
				intent: intent, model: model, session: session, dependsOn: dependsOn,
				reported: reported, allowMarkers: allowMarkers, requireUpToDate: upToDate,
			}
			if opts.model == "" {
				opts.model = a.Getenv("VX_MODEL")
			}
			if opts.session == "" {
				opts.session = a.Getenv("VX_SESSION")
			}
			if len(runs) == 0 {
				return a.landOnce(ctx, c, wc, opts)
			}
			// Verified land: sync, run the checks on exactly what will land,
			// land only if nobody landed in between; otherwise repeat.
			for attempt := 1; attempt <= verifiedAttempts; attempt++ {
				moved, err := a.effortMoved(ctx, c, wc)
				if err != nil {
					return err
				}
				if moved {
					a.printf("The effort moved on; syncing before running checks.\n")
					res, err := a.syncWorkingCopy(ctx, c, wc)
					if err != nil {
						return err
					}
					if len(res.conflicts) > 0 {
						a.printConflicts(wc, res.conflicts)
						out := res.json()
						out["board"] = a.syncFeed(ctx, c, wc)
						a.emit(out)
						a.landResult = notLandedConflicts(len(res.conflicts))
						return errConflict
					}
					opts.keptBoth = unionSorted(opts.keptBoth, res.keptBoth)
				}
				ran, failed := a.runChecks(wc.Root, runs)
				if failed && !allowFailing {
					return errors.New("checks failed; nothing landed. Fix them, or pass --allow-failing to land anyway")
				}
				opts.ran = ran
				opts.requireUpToDate = attempt < verifiedAttempts || upToDate
				err = a.landOnce(ctx, c, wc, opts)
				if !errors.Is(err, errEffortMoved) {
					return err
				}
				a.printf("Someone landed while the checks ran; trying again (%d of %d).\n", attempt+1, verifiedAttempts)
			}
			return errors.New("the effort keeps moving; try again in a moment")
		},
	}
	land := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := land(cmd, args)
		if err == nil || errors.Is(err, errConflict) {
			a.printLandResult()
		}
		return err
	}
	cmd.Flags().StringVarP(&intent, "intent", "m", "", "what this change does (default: the workspace intent)")
	cmd.Flags().StringArrayVar(&runs, "run", nil, `command for vx to run as evidence, e.g. "go test ./..." (repeatable)`)
	cmd.Flags().StringArrayVar(&checks, "check", nil, `self-reported evidence, as "NAME=pass|fail[:PASSED[:FAILED]]" (repeatable)`)
	cmd.Flags().StringArrayVar(&newTests, "new-test", nil, "name of a test this change adds or changes, e.g. TestLogin (repeatable or comma-separated; must be a func in a changed _test.go file)")
	cmd.Flags().BoolVar(&allowUnverified, "allow-unverified-tests", false, "land even if a --new-test name is not a func in a changed _test.go file")
	cmd.Flags().StringArrayVar(&dependsOn, "depends-on", nil, "symbol this change relies on, e.g. auth.Login (repeatable)")
	cmd.Flags().StringVar(&model, "model", "", "model that wrote the change (default: VX_MODEL)")
	cmd.Flags().StringVar(&session, "session", "", "agent session id (default: VX_SESSION)")
	cmd.Flags().BoolVar(&allowMarkers, "allow-markers", false, "land even if files contain conflict markers")
	cmd.Flags().BoolVar(&upToDate, "require-up-to-date", false, "refuse to land if others landed since your last sync")
	cmd.Flags().BoolVar(&allowFailing, "allow-failing", false, "with --run: land even if a command fails")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "ignore the evidence flags saved from an interrupted land")
	return cmd
}

type landOptions struct {
	intent, model, session string
	dependsOn              []string
	reported               *vxv1.Evidence
	ran                    []*vxv1.Check
	allowMarkers           bool
	requireUpToDate        bool
	// keptBoth are files a sync before this land merged by keeping both
	// sides' insertions; shown with the server's list.
	keptBoth []string
}

var errEffortMoved = errors.New("effort moved")

func (a *App) effortMoved(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) (bool, error) {
	eff, err := c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort}))
	if err != nil {
		return false, err
	}
	return eff.Msg.Effort.Head != wc.Config.Base, nil
}

// runChecks runs each command with sh -c in dir and records the result.
func (a *App) runChecks(dir string, cmds []string) ([]*vxv1.Check, bool) {
	var out []*vxv1.Check
	failed := false
	for _, command := range cmds {
		start := time.Now()
		c := exec.Command("sh", "-c", command)
		c.Dir = dir
		var buf bytes.Buffer
		c.Stdout, c.Stderr = &buf, &buf
		err := c.Run()
		ch := &vxv1.Check{Name: command, Result: "pass", RanByVx: true, DurationMs: time.Since(start).Milliseconds(), Notes: cacheNote(buf.String())}
		if err != nil {
			ch.Result = "fail"
			failed = true
			a.printf("FAIL %s (%.1fs)\n%s\n", command, float64(ch.DurationMs)/1000, tail(buf.String(), 40))
		} else {
			a.printf("pass %s (%.1fs)\n", command, float64(ch.DurationMs)/1000)
		}
		if ch.Notes != "" {
			a.printf("  %s\n  Hint: add -count=1 to run every test instead of using cached results.\n", ch.Notes)
		}
		out = append(out, ch)
	}
	return out, failed
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func (a *App) landOnce(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, o landOptions) error {
	tree, changes, err := wc.Snapshot(ctx)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return errors.New("nothing to land: no local changes")
	}
	if !o.allowMarkers {
		if bad, err := filesWithMarkers(ctx, wc, changes); err != nil {
			return err
		} else if len(bad) > 0 {
			return fmt.Errorf("%s still has conflict markers; resolve them or pass --allow-markers", strings.Join(bad, ", "))
		}
	}
	trees, blobs, err := object.NewObjects(ctx, wc.Objects, wc.Config.BaseTreeHash(), tree)
	if err != nil {
		return err
	}
	if _, err := c.Upload(ctx, wc.Config.Repo, wc.Objects, append(trees, blobs...)); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	evidence := o.reported
	if len(o.ran) > 0 {
		if evidence == nil {
			evidence = &vxv1.Evidence{}
		}
		evidence.Checks = append(append([]*vxv1.Check(nil), o.ran...), evidence.Checks...)
	}
	resp, err := c.Workspaces.Land(ctx, connect.NewRequest(&vxv1.LandRequest{
		WorkspaceId: wc.Config.Workspace, Base: wc.Config.Base, Tree: tree.String(),
		Intent: o.intent, Evidence: evidence, DependsOn: o.dependsOn, Model: o.model, Session: o.session,
		RequireUpToDate: o.requireUpToDate,
	}))
	if err != nil {
		return err
	}
	r := resp.Msg
	if r.EffortMoved {
		if len(o.ran) > 0 {
			return errEffortMoved
		}
		a.printf("Not landed: the effort moved on to %s since your last sync.\nRun `vx sync`, check your work again, then %s.\n", short(r.EffortHead), landAgain(wc))
		a.emit(r)
		a.landResult = fmt.Sprintf("Not landed: the effort moved on to %s since your last sync (exit 2)", short(r.EffortHead))
		return errConflict
	}
	if !r.Landed {
		a.printf("Not landed: your changes conflict with changes already in the effort (now at %s).\n\n", short(r.EffortHead))
		for _, cf := range r.Conflicts {
			a.printf("  %-14s %s %s\n", cf.Kind, cf.Path, cf.Detail)
		}
		a.printf("\nRun `vx sync` to bring in those changes and mark the conflicts, fix them, then %s again.\n", landAgain(wc))
		a.emit(r)
		a.landResult = notLandedConflicts(len(r.Conflicts))
		return errConflict
	}
	cs := r.Changeset
	newTree, err := hash.Parse(cs.Tree)
	if err != nil {
		return err
	}
	if err := c.FetchTree(ctx, wc.Config.Repo, wc.Objects, newTree); err != nil {
		return err
	}
	incoming, err := object.DiffTrees(ctx, wc.Objects, tree, newTree)
	if err != nil {
		return err
	}
	if err := wc.Apply(ctx, incoming); err != nil {
		return err
	}
	wc.Config.Base, wc.Config.BaseTree = cs.Hash, cs.Tree
	wc.Config.Workspace, wc.Config.Intent, wc.Config.ChangeID = "", "", ""
	wc.Config.PendingLand = nil
	if err := wc.Save(); err != nil {
		return err
	}
	var added, deleted int32
	for _, f := range cs.Files {
		added += f.Added
		deleted += f.Deleted
	}
	a.printf("Landed %s (%s): %s\n", short(cs.Hash), cs.ChangeId, firstLine(cs.Intent))
	for _, f := range cs.Files {
		a.printf("  %s %-48s +%d -%d\n", opLetter(object.Op(f.Op)), f.Path, f.Added, f.Deleted)
	}
	a.printf("  %d file(s), +%d -%d\n", len(cs.Files), added, deleted)
	if r.Rebased {
		a.printf("  Others had landed first; the server merged your changes onto theirs")
		if len(r.AutoMerged) > 0 {
			a.printf(" (merged line by line: %s)", strings.Join(r.AutoMerged, ", "))
		}
		a.printf(".\n")
		if evidence != nil && len(evidence.Checks) > 0 {
			a.printf("  Your evidence was gathered before that merge and is marked so. Use --run to check the merged result instead.\n")
		}
	}
	r.KeptBoth = unionSorted(r.KeptBoth, o.keptBoth)
	if len(r.KeptBoth) > 0 {
		a.printf("  Kept both insertions (check the order): %s\n", strings.Join(r.KeptBoth, ", "))
	}
	if len(incoming) > 0 {
		a.printf("  Updated %d file(s) here with others' changes.\n", len(incoming))
	}
	if evidence != nil && len(evidence.NewTests) > 0 {
		a.printf("New tests: %s\n", strings.Join(evidence.NewTests, ", "))
	}
	for _, c := range cs.GetEvidence().GetNotVerified() {
		a.printf("Not verified: %s\n", rules.NotVerified(c))
	}
	if notesDropped(evidence, cs.Evidence) {
		a.printf("Warning: the server did not record the check notes (it may run an older vx); the landed evidence lacks them.\n")
	}
	a.printWarnings(r.Warnings)
	a.printLandedOverClaims(r.OverlappingClaims)
	board := a.landFeed(ctx, c, wc, r) // the board comes last
	a.emit(protoWithBoard(r, board))
	a.landResult = fmt.Sprintf("Landed %s (%s): %s", short(cs.Hash), cs.ChangeId, firstLine(cs.Intent))
	return nil
}

func notLandedConflicts(n int) string {
	return fmt.Sprintf("Not landed: conflicts in %d file(s) (exit 2)", n)
}

// printLandResult ends vx land with its one-line result, set by the land.
func (a *App) printLandResult() {
	if a.landResult != "" {
		a.printf("\n%s\n", a.landResult)
		a.landResult = ""
	}
}

// printWarnings shows problems the server found in a landed or planned
// tree. They did not stop anything, so they come after the result.
func (a *App) printWarnings(ws []*vxv1.Warning) {
	code, generated := false, false
	for _, w := range ws {
		a.printf("Warning: %s\n", w.Detail)
		switch engine.WarningKind(w.Kind) {
		case engine.WarningDuplicate, engine.WarningParse:
			code = true
		case engine.WarningGenerated:
			generated = true
		}
	}
	if code {
		a.printf("  The text merged cleanly but the code may not build; fix it in a new change.\n")
	}
	if generated {
		a.printf("  vx did not merge generated files that both sides changed; regenerate them and land the result.\n")
	}
}

func filesWithMarkers(ctx context.Context, wc *workcopy.WorkingCopy, changes []object.Change) ([]string, error) {
	var bad []string
	for _, ch := range changes {
		if ch.Op == object.OpDelete {
			continue
		}
		data, err := wc.ReadBlob(ctx, ch.New.Hash)
		if err != nil {
			return nil, err
		}
		if workcopy.HasConflictMarkers(data) {
			bad = append(bad, ch.Path)
		}
	}
	return bad, nil
}

func parseChecks(checks, newTests []string) (*vxv1.Evidence, error) {
	if len(checks) == 0 && len(newTests) == 0 {
		return nil, nil
	}
	ev := &vxv1.Evidence{NewTests: newTests}
	for _, s := range checks {
		name, rest, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("check %q: use NAME=pass or NAME=fail:PASSED:FAILED", s)
		}
		parts := strings.Split(rest, ":")
		ch := &vxv1.Check{Name: strings.TrimSpace(name), Result: parts[0]}
		if ch.Result != "pass" && ch.Result != "fail" {
			return nil, fmt.Errorf("check %q: result must be pass or fail", s)
		}
		for i, dst := range []*int32{&ch.Passed, &ch.Failed} {
			if len(parts) > i+1 {
				n, err := strconv.Atoi(parts[i+1])
				if err != nil {
					return nil, fmt.Errorf("check %q: %v", s, err)
				}
				*dst = int32(n)
			}
		}
		ev.Checks = append(ev.Checks, ch)
	}
	return ev, nil
}

// ---------- sync ----------

type syncResult struct {
	upToDate  bool
	head      string
	updated   int
	local     int
	conflicts []engine.Conflict
	// merged are files both you and the effort changed that merged without
	// conflicts: their contents on disk are now a mix of both.
	merged []string
	// keptBoth are the merged files where both sides inserted different
	// lines at the same place and the merge kept both, effort side first.
	keptBoth []string
	// warnings are problems the merge created in Go packages you changed,
	// such as a function or method now declared in two files.
	warnings []engine.Warning
}

func (r syncResult) json() map[string]any {
	var cj []map[string]string
	for _, cf := range r.conflicts {
		cj = append(cj, map[string]string{"path": cf.Path, "kind": string(cf.Kind), "detail": cf.Detail})
	}
	merged := r.merged
	if merged == nil {
		merged = []string{}
	}
	keptBoth := r.keptBoth
	if keptBoth == nil {
		keptBoth = []string{}
	}
	wj := []map[string]any{}
	for _, w := range r.warnings {
		wj = append(wj, map[string]any{"path": w.Path, "kind": string(w.Kind), "name": w.Name, "decl": w.Decl, "files": w.Files, "detail": w.Detail})
	}
	return map[string]any{"up_to_date": r.upToDate, "base": r.head, "updated": r.updated, "local_changes": r.local, "conflicts": cj, "merged": merged, "kept_both": keptBoth, "warnings": wj}
}

// syncWorkingCopy brings the working copy to the effort head, merging local
// edits; text conflicts are written into the files with markers.
func (a *App) syncWorkingCopy(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) (syncResult, error) {
	eff, err := c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort}))
	if err != nil {
		return syncResult{}, err
	}
	head := eff.Msg.Effort.Head
	if head == wc.Config.Base {
		return syncResult{upToDate: true, head: head}, nil
	}
	cs, err := c.Efforts.GetChangeset(ctx, connect.NewRequest(&vxv1.GetChangesetRequest{Repo: wc.Config.Repo, Ref: head}))
	if err != nil {
		return syncResult{}, err
	}
	theirs, err := hash.Parse(cs.Msg.Changeset.Tree)
	if err != nil {
		return syncResult{}, err
	}
	if err := c.FetchTree(ctx, wc.Config.Repo, wc.Objects, theirs); err != nil {
		return syncResult{}, err
	}
	ours, local, err := wc.Snapshot(ctx)
	if err != nil {
		return syncResult{}, err
	}
	result := theirs
	var conflicts []engine.Conflict
	var keptBoth []string
	var generated []engine.GeneratedFile
	if len(local) > 0 {
		mr, err := engine.MergeTreesKeepConflicts(ctx, wc.Objects, wc.Config.BaseTreeHash(), ours, theirs, merge.Labels{
			Base: "base " + short(wc.Config.Base), Ours: "local", Theirs: "effort " + short(head),
		}, rules.MergeOptions(ctx, wc.Objects, ours, theirs)...)
		if err != nil {
			return syncResult{}, err
		}
		result, conflicts, keptBoth, generated = mr.Tree, mr.Conflicts, mr.KeptBoth, mr.Generated
	}
	incoming, err := object.DiffTrees(ctx, wc.Objects, ours, result)
	if err != nil {
		return syncResult{}, err
	}
	if err := wc.Apply(ctx, incoming); err != nil {
		return syncResult{}, err
	}
	wc.Config.Base, wc.Config.BaseTree = head, theirs.String()
	if wc.Config.Workspace != "" {
		if _, err := c.Workspaces.SetBase(ctx, connect.NewRequest(&vxv1.SetBaseRequest{Id: wc.Config.Workspace, Base: head})); err != nil {
			return syncResult{}, err
		}
	}
	if err := wc.Save(); err != nil {
		return syncResult{}, err
	}
	merged := mergedFiles(local, incoming, conflicts)
	sort.Strings(keptBoth)
	a.printf("Synced to %s: %d file(s) updated", short(head), len(incoming))
	if len(local) > 0 {
		a.printf(", your %d local change(s) kept", len(local))
	}
	a.printf(".\n")
	a.printPaths("Merged with your edits", merged)
	a.printKeptBoth(keptBoth)
	warnings := syncWarnings(ctx, wc, result, local, incoming, conflicts)
	dups := len(warnings) > 0
	for _, w := range warnings {
		a.printf("Warning: %s\n", w.Detail)
	}
	if dups {
		a.printf("  The text merged cleanly but the code will not build: your edits and the incoming changes both declare it. Remove one before landing.\n")
	}
	more := engine.GeneratedWarnings(generated)
	if len(local) > 0 {
		more = append(more, syncRuleWarnings(ctx, wc, theirs, result)...)
	}
	for _, w := range more {
		a.printf("Warning: %s\n", w.Detail)
	}
	if len(generated) > 0 {
		a.printf("  Both you and the effort changed generated files; vx kept yours. Regenerate them before landing.\n")
	}
	warnings = append(warnings, more...)
	return syncResult{head: head, updated: len(incoming), local: len(local), conflicts: conflicts, merged: merged, keptBoth: keptBoth, warnings: warnings}, nil
}

// syncWarnings runs the land-time Go check on the merged tree, for the Go
// packages of your local changes, when the sync brought in changes: a clean
// text merge can still declare the same function or method twice (two
// agents each adding a helper in a different file). Files that still have
// conflict markers are left out; the check never fails a sync.
func syncWarnings(ctx context.Context, wc *workcopy.WorkingCopy, tree hash.Hash, local, incoming []object.Change, conflicts []engine.Conflict) []engine.Warning {
	if len(local) == 0 || len(incoming) == 0 {
		return nil
	}
	conflicted := map[string]bool{}
	for _, cf := range conflicts {
		conflicted[cf.Path] = true
	}
	var paths []string
	for _, ch := range local {
		if ch.Op != object.OpDelete && !conflicted[ch.Path] {
			paths = append(paths, ch.Path)
		}
	}
	ws, err := engine.CheckGo(ctx, wc.Objects, tree, paths)
	if err != nil {
		return nil
	}
	var out []engine.Warning
	for _, w := range ws {
		if w.Kind == engine.WarningDuplicate {
			out = append(out, w)
		}
	}
	return out
}

// syncRuleWarnings checks the merged working copy against its vx.toml:
// [[unique]] keys your local changes share with the effort (two
// migrations with the same number). The check never fails a sync.
func syncRuleWarnings(ctx context.Context, wc *workcopy.WorkingCopy, effort, merged hash.Hash) []engine.Warning {
	r, err := rules.At(ctx, wc.Objects, merged)
	if err != nil {
		return nil
	}
	ws, err := rules.Check(ctx, wc.Objects, r, effort, merged)
	if err != nil {
		return nil
	}
	return ws
}

// printPaths prints "LABEL: PATH" for one path, or LABEL and an indented
// list for several; nothing for none.
func (a *App) printPaths(label string, paths []string) {
	switch len(paths) {
	case 0:
	case 1:
		a.printf("%s: %s\n", label, paths[0])
	default:
		a.printf("%s:\n", label)
		for _, p := range paths {
			a.printf("  %s\n", p)
		}
	}
}

// printKeptBoth lists files where both sides inserted different lines at
// the same place and the merge kept both (the effort's lines first). The
// text is all there; whether the order is right needs a look.
func (a *App) printKeptBoth(paths []string) {
	a.printPaths("Kept both insertions (check the order)", paths)
}

// unionSorted returns the sorted union of path lists.
func unionSorted(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, p := range l {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// mergedFiles returns, sorted, the files you had changed that the sync
// rewrote without a conflict: both sides changed them and the result is a
// line-by-line merge, worth a look before landing.
func mergedFiles(local, incoming []object.Change, conflicts []engine.Conflict) []string {
	mine := map[string]bool{}
	for _, ch := range local {
		mine[ch.Path] = true
	}
	conflicted := map[string]bool{}
	for _, cf := range conflicts {
		conflicted[cf.Path] = true
	}
	var out []string
	for _, ch := range incoming {
		if mine[ch.Path] && !conflicted[ch.Path] {
			out = append(out, ch.Path)
		}
	}
	sort.Strings(out)
	return out
}

// landAgain is how to resume a land: plain `vx land`, which reuses saved
// evidence flags, and what it will run.
func landAgain(wc *workcopy.WorkingCopy) string {
	if p := wc.Config.PendingLand; p != nil && p.Workspace == wc.Config.Workspace && p.Command() != "vx land" {
		return "`vx land` (it reuses your evidence: " + p.Command() + ")"
	}
	return "`vx land`"
}

func (a *App) printConflicts(wc *workcopy.WorkingCopy, conflicts []engine.Conflict) {
	a.printf("\nConflicts to resolve:\n")
	for _, cf := range conflicts {
		hint := "edit the file and remove the markers"
		switch cf.Kind {
		case engine.ConflictModifyDelete:
			hint = "the effort deleted it; your version is kept, delete it if that is right"
		case engine.ConflictDeleteModify:
			hint = "you deleted it but the effort changed it; their version is back, delete it again if that is right"
		case engine.ConflictBinary, engine.ConflictMode:
			hint = "your version is kept"
		}
		a.printf("  %-14s %s (%s)\n", cf.Kind, cf.Path, hint)
	}
	a.printf("\nThen run %s.\n", landAgain(wc))
}

func (a *App) syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Bring in changes others landed, keeping your local edits",
		Long: `Update the working copy to the effort head. Your local edits are kept and
merged with the incoming changes, line by line. Where both changed the same
lines, the file gets conflict markers (<<<<<<< local ... >>>>>>> effort);
edit them away, then land. vx land refuses files that still have markers.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			if wc.Config.Rebase != nil {
				return errRebasing(wc)
			}
			ctx := ctxOf(cmd)
			res, err := a.syncWorkingCopy(ctx, c, wc)
			if err != nil {
				return err
			}
			if res.upToDate {
				a.printf("Up to date at %s.\n", short(res.head))
			}
			if len(res.conflicts) > 0 {
				a.printConflicts(wc, res.conflicts)
			}
			out := res.json()
			out["board"] = a.syncFeed(ctx, c, wc) // the board comes last
			a.emit(out)
			if len(res.conflicts) > 0 {
				return errConflict
			}
			return nil
		},
	}
}

// ---------- wait ----------

func (a *App) waitCmd() *cobra.Command {
	var timeout, interval time.Duration
	var noNotes bool
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Wait until someone lands, asks or answers you, or posts a decision on your files",
		Long: `Block until the effort head moves past this working copy's base, then
print the new changes. Use it instead of polling vx log in a loop:
  vx wait --timeout 10m && vx sync

vx wait also returns when a question addressed to you arrives, a question
you asked is answered, or someone posts a decision or interface note on a
file you changed here, and says which happened ("reason" in --json: land,
question, answer or note). --no-notes waits for lands only. The note or
answer that woke it counts as read, so vx sync and vx inbox do not repeat it.

Exit codes: 0 when something happened, 4 when nothing did within
--timeout, 1 on errors. So vx wait --timeout 15m && vx sync syncs only
after an event.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			deadline := time.Now().Add(timeout)
			var seen *inboxSeen
			var watch *noteWatch
			if !noNotes {
				if in, err := a.inboxSnapshot(ctx, c); err == nil {
					s := seenOf(in)
					seen = &s
				}
				watch = a.startNoteWatch(ctx, c, wc)
			}
			for {
				eff, err := c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort}))
				if err != nil {
					return err
				}
				if head := eff.Msg.Effort.Head; head != wc.Config.Base {
					log, err := c.Efforts.Log(ctx, connect.NewRequest(&vxv1.LogRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort, Limit: 50, IncludeFiles: true}))
					if err != nil {
						return err
					}
					var fresh []*vxv1.Changeset
					for _, cs := range log.Msg.Changesets {
						if cs.Hash == wc.Config.Base {
							break
						}
						fresh = append(fresh, cs)
					}
					a.printf("%d new change(s) in %s:\n", len(fresh), wc.Config.Effort)
					for _, cs := range fresh {
						a.printf("  %s %s  %-44s %s\n", short(cs.Hash), cs.ChangeId, firstLine(cs.Intent), cs.Author.Principal)
					}
					a.printf("Run `vx sync` to bring them in.\n")
					a.emit(map[string]any{"reason": "land", "head": head, "changesets": fresh})
					return nil
				}
				if a.waitNotes(ctx, c, seen) || a.waitBoardNote(ctx, c, wc, watch) {
					return nil
				}
				if time.Now().After(deadline) {
					var err error
					switch {
					case watch != nil:
						err = fmt.Errorf("timed out: nothing landed in %s, no question or answer arrived for you and no decision or interface was posted on your changed files within %s", wc.Config.Effort, timeout)
					case seen != nil:
						err = fmt.Errorf("timed out: nothing landed in %s and no question or answer arrived for you within %s", wc.Config.Effort, timeout)
					default:
						err = fmt.Errorf("timed out: nothing landed in %s within %s", wc.Config.Effort, timeout)
					}
					return &exitError{code: ExitTimeout, err: err}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "give up after this long")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "how often to check")
	cmd.Flags().BoolVar(&noNotes, "no-notes", false, "wait for lands only, not for questions, answers and notes")
	return cmd
}
