package cli

import (
	"context"
	"errors"
	"fmt"
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
	var allowMarkers, upToDate, allowFailing, allowUnverified, fresh, noReuse bool
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

If someone lands while the checks run, vx stops them right away, syncs
and runs them again on the new merged files (up to 3 attempts; the last
one is not stopped and, if the effort moved meanwhile, lands merged with
its evidence marked as gathered before the merge).

vx remembers each command that passed, with the exact files it ran on
(the tree of the working copy). Running it again on byte-identical files
reuses that result instead ("pass CMD (reused from 14:02 ...)"; the
evidence says so too): for example after another check failed for a
reason outside the files, or a land that stopped before reaching the
effort. Any change to the files, including a sync that brought in others'
work, runs every command again: a check reads the whole tree, so only a
run on exactly what lands describes it. --no-reuse always runs them.

A failed command prints its last 30 lines of output; the full output of
each command is in .vx/last-check-N.log (N: its place among the --run
flags).

--check records a result you report yourself; it is marked as reported:
  vx land --check "go test ./...=pass:142"

Without --run, if others landed in the meantime the server merges your
changes onto theirs. The change is then marked as merged, and its evidence
as gathered before the merge. Only real conflicts stop a land: run vx
sync, fix the conflicting files and land again.

Landing policies. vx.toml [[policy]] may make some paths "gated" (the
land is refused unless every required check ran with --run and passed) or
"human" (a person approves first). A human land prints "Waiting for a
person to review: PATHS (policy in vx.toml)" and exits 0 with "status":
"pending_review" in --json: nothing is in the effort yet and the workspace
stays open. vx wait returns when a person decides; landing again from the
workspace replaces the snapshot under review.

-m on an open workspace replaces its intent for this land (and a retry of
it) and says so. Without a workspace, -m opens one first.

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
			if _, err := a.settleReview(ctx, c, wc); err != nil {
				return err
			}
			intent = strings.TrimSpace(intent)
			if wc.Config.Workspace == "" {
				if intent == "" {
					return errNoWorkspace(ctx, wc)
				}
				if _, err := a.openWorkspace(ctx, c, wc, intent); err != nil {
					return err
				}
			} else if intent != "" && intent != wc.Config.Intent {
				// -m on an open workspace replaces its intent: this land
				// and any retry of it use the new one.
				a.printf("Intent of workspace %s is now %q (was %q).\n", wc.Config.ChangeID, intent, wc.Config.Intent)
				wc.Config.Intent = intent
				if err := wc.Save(); err != nil {
					return err
				}
			}
			if intent == "" {
				intent = wc.Config.Intent
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
			if err := a.precheckScope(ctx, c, wc); err != nil {
				return err
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
					opts.mergedByDecl = unionSorted(opts.mergedByDecl, res.mergedByDecl)
				}
				// Stop the checks when someone lands meanwhile, except on
				// the last attempt, which lands merged if it has to.
				run, err := a.runChecks(ctx, c, wc, runs, checkOptions{stopIfMoved: attempt < verifiedAttempts, reuse: !noReuse})
				if err != nil {
					return err
				}
				if run.moved {
					a.printf("Someone landed while the checks ran; stopped them to sync and run them on the merged files (%d of %d).\n", attempt+1, verifiedAttempts)
					continue
				}
				if run.failed && !allowFailing {
					return errors.New("checks failed; nothing landed. Fix them, or pass --allow-failing to land anyway")
				}
				a.printChangedByChecks(run.changed)
				opts.ran = run.ran
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
	cmd.Flags().BoolVar(&a.noGenerate, "no-generate", false, "do not run vx.toml [[generated]] commands for generated files a merge kept from one side")
	cmd.Flags().BoolVar(&noReuse, "no-reuse", false, "with --run: run every command, even one that already passed on exactly these files")
	return cmd
}

// errNoWorkspace is vx land's refusal without a workspace or -m.
func errNoWorkspace(ctx context.Context, wc *workcopy.WorkingCopy) error {
	if changes, err := wc.Status(ctx); err == nil && len(changes) > 0 {
		return errors.New(noWorkspaceHint(len(changes)))
	}
	return errors.New(`nothing to land: no workspace open and no local changes. Start one with: vx new "INTENT"`)
}

// noWorkspaceHint is what vx status, sync and land say when there are local
// changes but no workspace to land them from.
func noWorkspaceHint(changes int) string {
	return fmt.Sprintf(`You have %d local change(s) but no workspace: vx new "INTENT" (or vx land -m "INTENT")`, changes)
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
	// mergedByDecl are .go files a sync before this land merged
	// declaration by declaration.
	mergedByDecl []string
}

var errEffortMoved = errors.New("effort moved")

func (a *App) effortMoved(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) (bool, error) {
	eff, err := c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort}))
	if err != nil {
		return false, err
	}
	return eff.Msg.Effort.Head != wc.Config.Base, nil
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
		if r.Status == "" {
			r.Status = "effort_moved"
		}
		if len(o.ran) > 0 {
			return errEffortMoved
		}
		a.printf("Not landed: the effort moved on to %s since your last sync.\nRun `vx sync`, check your work again, then %s.\n", short(r.EffortHead), landAgain(wc))
		a.emit(r)
		a.landResult = fmt.Sprintf("Not landed: the effort moved on to %s since your last sync (exit 2)", short(r.EffortHead))
		return errConflict
	}
	if r.Review != nil {
		return a.landPending(ctx, c, wc, r)
	}
	if !r.Landed {
		if r.Status == "" {
			r.Status = "conflicts"
		}
		a.printf("Not landed: your changes conflict with changes already in the effort (now at %s).\n\n", short(r.EffortHead))
		for _, cf := range r.Conflicts {
			a.printf("  %-14s %s %s\n", cf.Kind, cf.Path, cf.Detail)
		}
		a.printf("\nRun `vx sync` to bring in those changes and mark the conflicts, fix them, then %s again.\n", landAgain(wc))
		a.emit(r)
		a.landResult = notLandedConflicts(len(r.Conflicts))
		return errConflict
	}
	if r.Status == "" {
		r.Status = "landed"
	}
	wc.Config.Review = ""
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
	r.MergedByDecl = unionSorted(r.MergedByDecl, o.mergedByDecl)
	if len(r.MergedByDecl) > 0 {
		a.printf("  Merged by declaration (the lines conflicted, the Go declarations did not; check the result): %s\n", strings.Join(r.MergedByDecl, ", "))
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
	// The server kept one side of generated files both changed:
	// regenerate them here, as a local change to land next.
	regen, _ := a.regenerate(ctx, wc, generatedOf(r.Warnings))
	a.printWarnings(r.Warnings, regeneratedPaths(regen))
	if len(regeneratedPaths(regen)) > 0 {
		if changes, err := wc.Status(ctx); err == nil && len(changes) > 0 {
			a.printf("  The landed change has one side's generated files; land the regenerated ones: vx land -m \"Regenerate generated files\"\n")
		}
	}
	a.printRegenProblems(regen)
	a.printLandedOverClaims(r.OverlappingClaims)
	a.printOverlaps(r.Overlaps)
	board := a.landFeed(ctx, c, wc, r) // the board comes last
	out := protoWithBoard(r, board)
	if m, ok := out.(map[string]any); ok && regen != nil {
		m["regenerated"] = regen
	}
	a.emit(out)
	a.landResult = fmt.Sprintf("Landed %s (%s): %s", short(cs.Hash), cs.ChangeId, firstLine(cs.Intent))
	return nil
}

// landPending reports a land that waits for a person (a "human" landing
// policy): nothing landed, the workspace stays open, and the working copy
// remembers the review so vx wait, vx status and vx sync follow it.
func (a *App) landPending(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, r *vxv1.LandResponse) error {
	rv := r.Review
	if r.Status == "" {
		r.Status = "pending_review"
	}
	wc.Config.Review = rv.Id
	if err := wc.Save(); err != nil {
		return err
	}
	a.printf("Not in the effort yet: %s\n", rv.Waiting)
	for _, p := range rv.Policies {
		a.printf("  policy: %s\n", p)
	}
	for _, l := range rv.Approvers {
		a.printf("  approvers: %s\n", strings.Join(l.Approvers, ", "))
	}
	a.printf("  Review %s. People with write on %s approve it in the web UI (Reviews) or with vx review approve %s.\n", noteID(rv.Id), wc.Config.Repo, noteID(rv.Id))
	a.printf("  Your workspace stays open; `vx wait` returns when a person decides. Landing again from here replaces the snapshot under review.\n")
	for _, c := range rv.GetEvidence().GetNotVerified() {
		a.printf("Not verified: %s\n", rules.NotVerified(c))
	}
	board := a.landFeed(ctx, c, wc, r)
	a.emit(protoWithBoard(r, board))
	a.landResult = rv.Waiting
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
// Generated files in regenerated were regenerated here already.
func (a *App) printWarnings(ws []*vxv1.Warning, regenerated map[string]bool) {
	code, generated := false, false
	for _, w := range ws {
		if engine.WarningKind(w.Kind) == engine.WarningGenerated && regenerated[w.Path] {
			continue
		}
		a.printf("Warning: %s\n", w.Detail)
		switch engine.WarningKind(w.Kind) {
		case engine.WarningDuplicate, engine.WarningParse, engine.WarningSignature, engine.WarningRemoved:
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
	// mergedByDecl are the merged .go files whose lines conflicted but
	// whose top-level declarations did not: rebuilt from both sides'
	// declarations.
	mergedByDecl []string
	// warnings are problems the merge created in Go packages you changed,
	// such as a function or method now declared in two files.
	warnings []engine.Warning
	// regenerated are the [[generated]] commands vx ran (or could not) for
	// generated files the merge kept from your side.
	regenerated []regenResult
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
	byDecl := r.mergedByDecl
	if byDecl == nil {
		byDecl = []string{}
	}
	wj := []map[string]any{}
	for _, w := range r.warnings {
		wj = append(wj, map[string]any{"path": w.Path, "kind": string(w.Kind), "name": w.Name, "decl": w.Decl, "files": w.Files, "detail": w.Detail})
	}
	regen := r.regenerated
	if regen == nil {
		regen = []regenResult{}
	}
	return map[string]any{"up_to_date": r.upToDate, "base": r.head, "updated": r.updated, "local_changes": r.local, "conflicts": cj, "merged": merged, "kept_both": keptBoth, "merged_by_decl": byDecl, "warnings": wj, "regenerated": regen}
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
	var keptBoth, byDecl []string
	var generated []engine.GeneratedFile
	if len(local) > 0 {
		mr, err := engine.MergeTreesKeepConflicts(ctx, wc.Objects, wc.Config.BaseTreeHash(), ours, theirs, merge.Labels{
			Base: "base " + short(wc.Config.Base), Ours: "local", Theirs: "effort " + short(head),
		}, rules.MergeOptions(ctx, wc.Objects, ours, theirs)...)
		if err != nil {
			return syncResult{}, err
		}
		result, conflicts, keptBoth, byDecl, generated = mr.Tree, mr.Conflicts, mr.KeptBoth, mr.MergedByDecl, mr.Generated
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
	sort.Strings(byDecl)
	a.printf("Synced to %s: %d file(s) updated", short(head), len(incoming))
	if len(local) > 0 {
		a.printf(", your %d local change(s) kept", len(local))
	}
	a.printf(".\n")
	a.printPaths("Merged with your edits", merged)
	a.printKeptBoth(keptBoth)
	a.printPaths("Merged by declaration (the lines conflicted, the Go declarations did not; check the result)", byDecl)
	warnings := syncWarnings(ctx, wc, result, local, incoming, conflicts)
	dups := len(warnings) > 0
	for _, w := range warnings {
		a.printf("Warning: %s\n", w.Detail)
	}
	if dups {
		a.printf("  The text merged cleanly but the code will not build: your edits and the incoming changes both declare it. Remove one before landing.\n")
	}
	if len(local) > 0 {
		sides, _ := engine.CheckGoSides(ctx, wc.Objects, engine.GoSides{
			Base: wc.Config.BaseTreeHash(), Ours: ours, Theirs: theirs, Merged: result,
			OursName: "your local changes", TheirsName: "the effort",
		})
		for _, w := range sides {
			a.printf("Warning: %s\n", w.Detail)
		}
		if len(sides) > 0 {
			a.printf("  A signature or name changed on one side is used the old way by the other; update the callers (or the signature) before landing.\n")
		}
		warnings = append(warnings, sides...)
	}
	regen, stale := a.regenerate(ctx, wc, generated)
	more := engine.GeneratedWarnings(stale)
	if len(local) > 0 {
		more = append(more, syncRuleWarnings(ctx, wc, theirs, result)...)
	}
	for _, w := range more {
		a.printf("Warning: %s\n", w.Detail)
	}
	if len(stale) > 0 {
		a.printf("  Both you and the effort changed generated files; vx kept yours. Regenerate them before landing.\n")
		a.printRegenProblems(regen)
	}
	warnings = append(warnings, more...)
	return syncResult{head: head, updated: len(incoming), local: len(local), conflicts: conflicts, merged: merged, keptBoth: keptBoth, mergedByDecl: byDecl, warnings: warnings, regenerated: regen}, nil
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
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Bring in changes others landed, keeping your local edits",
		Long: `Update the working copy to the effort head. Your local edits are kept and
merged with the incoming changes, line by line. Where both changed the same
lines, the file gets conflict markers (<<<<<<< local ... >>>>>>> effort);
edit them away, then land. vx land refuses files that still have markers.

Generated files (vx.toml [[generated]]) that both you and the effort
changed are never merged: vx keeps yours and, when no file has conflict
markers, runs the rule's command (for example buf generate) to regenerate
them from the merged sources, and says so. If the command is not on PATH
or fails, the warning stays and says what to run. --no-generate turns
this off (vx land and vx effort rebase take it too).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			if wc.Config.Rebase != nil {
				return errRebasing(wc)
			}
			ctx := ctxOf(cmd)
			pending, err := a.settleReview(ctx, c, wc)
			if err != nil {
				return err
			}
			res, err := a.syncWorkingCopy(ctx, c, wc)
			if err != nil {
				return err
			}
			if pending != nil {
				a.printWaiting(pending)
			}
			if res.upToDate {
				a.printf("Up to date at %s.\n", short(res.head))
			}
			if len(res.conflicts) > 0 {
				a.printConflicts(wc, res.conflicts)
			}
			out := res.json()
			if wc.Config.Workspace == "" {
				if changes, err := wc.Status(ctx); err == nil && len(changes) > 0 {
					a.printf("%s.\n", noWorkspaceHint(len(changes)))
					out["no_workspace"] = true
				}
			}
			out["board"] = a.syncFeed(ctx, c, wc) // the board comes last
			a.emit(out)
			if len(res.conflicts) > 0 {
				return errConflict
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&a.noGenerate, "no-generate", false, "do not run vx.toml [[generated]] commands for generated files the merge kept from your side")
	return cmd
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
question, answer, note or review). When your last land waits for a person
(a "human" landing policy in vx.toml), it returns as soon as they approve
or reject it (reason review). --no-notes waits for lands only. The note or
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
				if wc.Config.Review != "" {
					id := wc.Config.Review
					pending, err := a.settleReview(ctx, c, wc)
					if err != nil {
						return err
					}
					if pending == nil {
						r, err := c.Reviews.GetReview(ctx, connect.NewRequest(&vxv1.GetReviewRequest{Id: id}))
						if err != nil {
							return err
						}
						if r.Msg.Review.State == "approved" {
							a.printf("Run `vx sync` to bring it in.\n")
						}
						a.emit(map[string]any{"reason": "review", "review": r.Msg.Review})
						return nil
					}
				}
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
						err = fmt.Errorf("timed out: nothing landed in %s, no question or answer arrived for you and no decision or interface was posted on your changed files and no review comment or CI result arrived for you within %s", wc.Config.Effort, timeout)
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
