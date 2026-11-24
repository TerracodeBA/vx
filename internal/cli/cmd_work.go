package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// ---------- clone ----------

func (a *App) cloneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clone REPO EFFORT [DIR]",
		Short: "Check out an effort into a new working copy",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			repo, slug := args[0], args[1]
			dir := slug
			if len(args) == 3 {
				dir = args[2]
			}
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(a.dir(), dir)
			}
			eff, err := c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: repo, Effort: slug}))
			if err != nil {
				return err
			}
			head := eff.Msg.Effort.Head
			cs, err := c.Efforts.GetChangeset(ctx, connect.NewRequest(&vxv1.GetChangesetRequest{Repo: repo, Ref: head}))
			if err != nil {
				return err
			}
			wc, err := workcopy.Create(dir, workcopy.Config{Repo: repo, Effort: slug})
			if err != nil {
				return err
			}
			tree, err := hash.Parse(cs.Msg.Changeset.Tree)
			if err != nil {
				return err
			}
			if err := c.FetchTree(ctx, repo, wc.Objects, tree); err != nil {
				return err
			}
			files, err := object.DiffTrees(ctx, wc.Objects, hash.Zero, tree)
			if err != nil {
				return err
			}
			if err := wc.Apply(ctx, files); err != nil {
				return err
			}
			wc.Config.Base, wc.Config.BaseTree = head, tree.String()
			if err := wc.Save(); err != nil {
				return err
			}
			a.printf("Cloned %s/%s at %s into %s (%d files)\n\nNext:\n  cd %s\n  vx new \"what you are about to do\"\n",
				repo, slug, short(head), dir, len(files), dir)
			a.emit(map[string]any{"dir": dir, "repo": repo, "effort": slug, "base": head, "files": len(files)})
			return nil
		},
	}
}

// ---------- new ----------

func (a *App) newCmd() *cobra.Command {
	var taskID string
	var noSync bool
	var claim claimOptions
	cmd := &cobra.Command{
		Use:   "new INTENT [--task ID] [--claim TARGET...]",
		Short: "Open a workspace for one task in the current effort",
		Long: `Open a workspace: one task, described by its intent, that ends in one land.
Other agents see your intent in vx status while you work. The intent
becomes the change's description and, when shipped, its commit message.

Keep tasks small and land often: vx land closes the workspace, so run
vx new again for the next task.

vx new first syncs to the effort head (like vx sync), so the work starts
from what others landed; it says so in one line. --no-sync skips that.

With --task ID, vx new takes that task of the effort's plan (vx tasks
--open lists them) and uses its title as the intent when none is given.

With --claim, vx new also claims the paths or symbols you are about to
change, tied to the new workspace, so others see it before they edit them.
Claims block nobody; they end when the workspace lands or is abandoned.`,
		Example: `  vx new "Add limiter middleware"
  vx new --task 1a2b3c4d
  vx new "Move clientIP into httpx" --claim internal/httpx/ --claim internal/api/limit.go`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			if wc.Config.Rebase != nil {
				return errRebasing(wc)
			}
			ctx := ctxOf(cmd)
			if r, err := a.settleReview(ctx, c, wc); err != nil {
				return err
			} else if r != nil {
				return errWaitingForReview(r)
			}
			if wc.Config.Workspace != "" {
				return fmt.Errorf("workspace %q is still open: land it with `vx land` or close it with `vx abandon`", wc.Config.Intent)
			}
			if taskID == "" && len(args) == 0 {
				return fmt.Errorf("say what this workspace is for: vx new \"INTENT\", or start on a task with vx new --task ID")
			}
			// Start from the effort head, so the work begins on what
			// others landed instead of meeting it at land time.
			var synced *syncResult
			if !noSync {
				res, err := a.syncWorkingCopy(ctx, c, wc)
				if err != nil {
					return err
				}
				if res.upToDate {
					a.printf("Up to date at %s.\n", short(res.head))
				}
				synced = &res
			}
			task, intent, err := a.taskOnNew(ctx, c, taskID, args)
			if err != nil {
				return err
			}
			w, err := a.openWorkspace(ctx, c, wc, intent)
			if err != nil {
				return err
			}
			a.printf("Opened workspace %s: %s\n", w.ChangeId, w.Intent)
			out := map[string]any{"workspace": w}
			if synced != nil {
				out["sync"] = synced.json()
			}
			if task != nil {
				a.printf("You took task %s; when it has landed: vx task done %s\n", noteID(task.Id), noteID(task.Id))
				out["task"] = task
			}
			if len(claim.targets) > 0 {
				m, err := a.claimOnNew(ctx, c, wc, w, claim)
				if err != nil {
					return err
				}
				out["claim"], out["overlapping"] = m.Claim, m.Overlapping
			}
			if synced != nil && len(synced.conflicts) > 0 {
				a.printConflicts(wc, synced.conflicts)
				a.emit(out)
				return errConflict
			}
			a.printf("Edit files, then run `vx land`.\n")
			if len(out) == 1 {
				a.emit(w)
			} else {
				a.emit(out)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noSync, "no-sync", false, "open the workspace on the files as they are, without syncing to the effort head first")
	cmd.Flags().StringVar(&taskID, "task", "", "take this task of the effort's plan and work on it here")
	cmd.Flags().StringSliceVar(&claim.targets, "claim", nil, "path or symbol to claim for this workspace (repeatable)")
	claimFlags(cmd, &claim)
	return cmd
}

func (a *App) openWorkspace(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, intent string) (*vxv1.Workspace, error) {
	resp, err := c.Workspaces.OpenWorkspace(ctx, connect.NewRequest(&vxv1.OpenWorkspaceRequest{
		Repo: wc.Config.Repo, Effort: wc.Config.Effort, Intent: intent,
	}))
	if err != nil {
		return nil, err
	}
	w := resp.Msg.Workspace
	if w.Base != wc.Config.Base {
		// The files here are on an older snapshot; tell the server.
		sb, err := c.Workspaces.SetBase(ctx, connect.NewRequest(&vxv1.SetBaseRequest{Id: w.Id, Base: wc.Config.Base}))
		if err != nil {
			return nil, err
		}
		w = sb.Msg.Workspace
	}
	wc.Config.Workspace, wc.Config.Intent, wc.Config.ChangeID = w.Id, w.Intent, w.ChangeId
	return w, wc.Save()
}

func (a *App) clientAndCopy() (*client.Client, *workcopy.WorkingCopy, error) {
	wc, err := a.workingCopy()
	if err != nil {
		return nil, nil, err
	}
	c, err := a.client()
	if err != nil {
		return nil, nil, err
	}
	return c, wc, nil
}

// ---------- status ----------

func opLetter(op object.Op) string {
	switch op {
	case object.OpAdd:
		return "A"
	case object.OpDelete:
		return "D"
	}
	return "M"
}

func (a *App) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show local changes and whether the effort moved on",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wc, err := a.workingCopy()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			changes, err := wc.Status(ctx)
			if err != nil {
				return err
			}
			if err := wc.Save(); err != nil {
				return err
			}
			out := map[string]any{"repo": wc.Config.Repo, "effort": wc.Config.Effort, "base": wc.Config.Base}
			var mine *vxv1.Review
			if c, err := a.client(); err == nil && wc.Config.Review != "" {
				if mine, err = a.settleReview(ctx, c, wc); err != nil {
					a.printf("Could not check your pending review: %v\n", err)
				}
			}
			a.printf("Effort %s in %s, files at %s\n", wc.Config.Effort, wc.Config.Repo, short(wc.Config.Base))
			if wc.Config.Workspace != "" {
				a.printf("Workspace %s: %s\n", wc.Config.ChangeID, wc.Config.Intent)
				out["workspace"] = map[string]string{"id": wc.Config.Workspace, "change_id": wc.Config.ChangeID, "intent": wc.Config.Intent}
				if mine != nil {
					a.printWaiting(mine)
					out["review"] = mine
				}
			} else if wc.Config.Rebase == nil && len(changes) > 0 {
				a.printf("%s\n", noWorkspaceHint(len(changes)))
			} else if wc.Config.Rebase == nil {
				a.printf("No workspace open. Start one with: vx new \"INTENT\"\n")
			}
			var serverRebase *vxv1.Rebase
			c, cerr := a.client()
			var eff *connect.Response[vxv1.GetEffortResponse]
			if cerr == nil {
				eff, cerr = c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort}))
				if cerr == nil {
					serverRebase = eff.Msg.Effort.Rebase
				}
			}
			if rb := a.printRebaseStatus(wc, serverRebase); rb != nil {
				out["rebase"] = rb
			}
			if cerr == nil {
				head := eff.Msg.Effort.Head
				out["effort_head"] = head
				if head != wc.Config.Base && wc.Config.Rebase == nil {
					if wc.Config.Workspace != "" {
						a.printf("The effort moved on to %s. Run `vx sync`, or `vx land --run` syncs for you.\n", short(head))
					} else {
						a.printf("The effort moved on to %s. Run `vx sync` to pick up others' changes.\n", short(head))
					}
				}
				var others []map[string]string
				for _, w := range eff.Msg.OpenWorkspaces {
					if w.Id == wc.Config.Workspace {
						continue
					}
					others = append(others, map[string]string{"owner": w.Owner, "intent": w.Intent, "change_id": w.ChangeId})
				}
				if len(others) > 0 {
					a.printf("\nOthers working in this effort:\n")
					for _, o := range others {
						a.printf("  %-30s %s\n", o["owner"], o["intent"])
					}
				}
				out["others"] = others
				if log, err := c.Efforts.Log(ctx, connect.NewRequest(&vxv1.LogRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort, Limit: 3})); err == nil && len(log.Msg.Changesets) > 0 {
					a.printf("\nRecently landed:\n")
					var recent []map[string]string
					for _, cs := range log.Msg.Changesets {
						a.printf("  %s %s  %-44s %s\n", short(cs.Hash), cs.ChangeId, firstLine(cs.Intent), cs.Author.Principal)
						recent = append(recent, map[string]string{"hash": cs.Hash, "change_id": cs.ChangeId, "intent": cs.Intent, "author": cs.Author.Principal})
					}
					out["recent"] = recent
					if warned := recentWarnings(log.Msg.Changesets); len(warned) > 0 {
						a.printf("\nWarnings in recent lands: %s\n", strings.Join(warned, "; "))
						out["warnings"] = warned
					}
				}
				if ov := a.overlapStatus(ctx, c, wc.Config.Repo, wc.Config.Effort); ov != nil {
					out["overlaps"] = ov
				}
				if rs, err := c.Reviews.ListReviews(ctx, connect.NewRequest(&vxv1.ListReviewsRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort})); err == nil && len(rs.Msg.Reviews) > 0 {
					a.printf("\nWaiting for a person to review (not in the effort yet; vx review list):\n")
					for _, r := range rs.Msg.Reviews {
						a.printf("  %s %s  %-44s %s  %s\n", noteID(r.Id), r.ChangeId, firstLine(r.Intent), r.Author.GetRef(), strings.Join(r.Paths, ", "))
					}
					out["pending_reviews"] = rs.Msg.Reviews
				}
				if ships := a.shipStatus(ctx, c, wc.Config.Repo, wc.Config.Effort); ships != nil {
					out["shipments"] = ships
				}
			}
			var files []map[string]string
			if len(changes) == 0 {
				a.printf("\nNo local changes.\n")
			} else {
				a.printf("\nChanges:\n")
			}
			for _, ch := range changes {
				a.printf("  %s %s\n", opLetter(ch.Op), ch.Path)
				files = append(files, map[string]string{"op": string(ch.Op), "path": ch.Path})
			}
			out["changes"] = files
			marked, err := filesWithMarkers(ctx, wc, changes)
			if err != nil {
				return err
			}
			switch {
			case len(marked) > 0 && wc.Config.Rebase != nil:
				a.printf("\nUnresolved conflict markers in: %s\nvx effort rebase --continue will refuse these files until the markers are gone.\n", strings.Join(marked, ", "))
			case len(marked) > 0:
				a.printf("\nUnresolved conflict markers in: %s\nvx land will refuse these files until the markers are gone.\n", strings.Join(marked, ", "))
			case wc.Config.Rebase != nil:
				a.printf("\nNo conflict markers; continue with: vx effort rebase --continue\n")
			case len(changes) > 0 && wc.Config.Workspace == "":
				a.printf("\nNo conflict markers; open a workspace to land them: vx new \"INTENT\" (or vx land -m \"INTENT\").\n")
			case len(changes) > 0:
				a.printf("\nNo conflict markers; ready to land.\n")
			}
			out["conflict_markers"] = marked
			if c, err := a.client(); err == nil {
				if ai := a.ownAgent(ctx, c); ai != nil {
					if ai.Unlimited {
						a.printf("\nYour scope (%s): no limits\n", ai.Agent.Ref)
					} else {
						a.printf("\nYour scope (%s):\n", ai.Agent.Ref)
						for _, l := range scopeLines(ai) {
							a.printf("  %s\n", l)
						}
					}
					paths := make([]string, len(changes))
					for i, ch := range changes {
						paths[i] = ch.Path
					}
					outside := outsideScope(ai.Scope, paths)
					if len(outside) > 0 {
						a.printf("Outside your scope, vx land will refuse: %s\n", strings.Join(outside, ", "))
					}
					out["agent"] = ai
					out["outside_scope"] = outside
				}
				if board := a.boardStatus(ctx, c, wc, changes); board != nil {
					out["board"] = board
				}
				if tasks := a.taskStatus(ctx, c, wc.Config.Repo, wc.Config.Effort); tasks != nil {
					out["tasks"] = tasks
				}
			}
			a.emit(out)
			return nil
		},
	}
}

// ---------- diff ----------

func (a *App) diffCmd() *cobra.Command {
	var stat bool
	cmd := &cobra.Command{
		Use:   "diff [PATH...]",
		Short: "Show local changes as a unified diff",
		RunE: func(cmd *cobra.Command, args []string) error {
			wc, err := a.workingCopy()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			changes, err := wc.Status(ctx)
			if err != nil {
				return err
			}
			changes = filterPaths(changes, args)
			var files []map[string]any
			for _, ch := range changes {
				old, err := wc.ReadBlob(ctx, ch.Old.Hash)
				if err != nil {
					return err
				}
				cur, err := wc.ReadBlob(ctx, ch.New.Hash)
				if err != nil {
					return err
				}
				added, deleted := merge.LineStats(old, cur)
				files = append(files, map[string]any{"path": ch.Path, "op": string(ch.Op), "added": added, "deleted": deleted})
				if stat {
					a.printf(" %-50s +%d -%d\n", ch.Path, added, deleted)
					continue
				}
				oldName, newName := ch.Path, ch.Path
				if ch.Op == object.OpAdd {
					oldName = ""
				}
				if ch.Op == object.OpDelete {
					newName = ""
				}
				a.printf("%s", merge.Unified(oldName, newName, old, cur, 3))
			}
			a.emit(map[string]any{"files": files})
			return nil
		},
	}
	cmd.Flags().BoolVar(&stat, "stat", false, "only show line counts per file")
	return cmd
}

func filterPaths(changes []object.Change, paths []string) []object.Change {
	if len(paths) == 0 {
		return changes
	}
	var out []object.Change
	for _, c := range changes {
		for _, p := range paths {
			p = filepath.ToSlash(filepath.Clean(p))
			if p == "." || object.HasPrefixPath(c.Path, p) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// ---------- abandon ----------

func (a *App) abandonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "abandon",
		Short: "Close the open workspace without landing (local files stay)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			if wc.Config.Workspace == "" {
				return errors.New("no workspace is open")
			}
			resp, err := c.Workspaces.Abandon(ctxOf(cmd), connect.NewRequest(&vxv1.AbandonRequest{Id: wc.Config.Workspace}))
			if err != nil {
				return err
			}
			a.printf("Abandoned workspace %s. Local files are unchanged.\n", wc.Config.ChangeID)
			if wc.Config.Review != "" {
				a.printf("Its pending review %s is withdrawn.\n", noteID(wc.Config.Review))
			}
			wc.Config.Workspace, wc.Config.Intent, wc.Config.ChangeID, wc.Config.Review = "", "", "", ""
			wc.Config.PendingLand = nil
			a.emit(resp.Msg)
			return wc.Save()
		},
	}
}

// ---------- log / show ----------

func (a *App) logCmd() *cobra.Command {
	var limit int
	var files bool
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "log [EFFORT]",
		Short: "List changes landed in the effort, newest first",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, slug, err := a.resolveEffort(repoFlag, args)
			if err != nil {
				return err
			}
			resp, err := c.Efforts.Log(ctxOf(cmd), connect.NewRequest(&vxv1.LogRequest{Repo: repo, Effort: slug, Limit: int32(limit), IncludeFiles: files}))
			if err != nil {
				return err
			}
			if len(resp.Msg.Changesets) == 0 {
				a.printf("Nothing landed in %s yet.\n", slug)
			}
			for _, cs := range resp.Msg.Changesets {
				a.printf("%s %s  %s\n", short(cs.Hash), cs.ChangeId, firstLine(cs.Intent))
				who := cs.Author.Principal
				if cs.Author.OnBehalfOf != "" {
					who += " for " + cs.Author.OnBehalfOf
				}
				if cs.Author.Model != "" {
					who += ", " + cs.Author.Model
				}
				a.printf("    %s, %s\n", who, cs.CreatedAt)
				a.printDetails(cs, "    ")
				for _, f := range cs.Files {
					a.printf("    %s %-48s +%d -%d\n", opLetter(object.Op(f.Op)), f.Path, f.Added, f.Deleted)
				}
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "number of changes")
	cmd.Flags().BoolVar(&files, "files", false, "list changed files")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository")
	return cmd
}

func (a *App) showCmd() *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "show REF",
		Short: "Show one change: hash, hash prefix or change id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, err := a.resolveRepo(repoFlag)
			if err != nil {
				return err
			}
			resp, err := c.Efforts.GetChangeset(ctxOf(cmd), connect.NewRequest(&vxv1.GetChangesetRequest{Repo: repo, Ref: args[0]}))
			if err != nil {
				return err
			}
			cs := resp.Msg.Changeset
			a.printf("change %s  %s\n", cs.ChangeId, cs.Hash)
			a.printf("origin %s", cs.Origin)
			if cs.GitSha != "" {
				a.printf(", git %s", cs.GitSha)
			}
			if cs.Effort != "" {
				a.printf(", effort %s", cs.Effort)
			}
			a.printf("\nauthor %s", cs.Author.Principal)
			if cs.Author.OnBehalfOf != "" {
				a.printf(" for %s", cs.Author.OnBehalfOf)
			}
			if cs.Author.Model != "" {
				a.printf(" (%s)", cs.Author.Model)
			}
			a.printf("\ndate   %s\n\n    %s\n\n", cs.CreatedAt, strings.ReplaceAll(strings.TrimSpace(cs.Intent), "\n", "\n    "))
			a.printDetails(cs, "")
			if len(cs.Files) > 0 {
				a.printf("\n")
			}
			for _, f := range cs.Files {
				a.printf("  %s %-48s +%d -%d\n", opLetter(object.Op(f.Op)), f.Path, f.Added, f.Deleted)
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository")
	return cmd
}

// recentWarnings lists the warnings stored with changes, each naming the
// change it landed with: "duplicate func clientIP in internal/httpapi
// (audit.go, limit.go) (change qx9b-mwns)".
func recentWarnings(css []*vxv1.Changeset) []string {
	var out []string
	for _, cs := range css {
		for _, w := range cs.Warnings {
			out = append(out, fmt.Sprintf("%s (change %s)", w.Detail, cs.ChangeId))
		}
	}
	return out
}

// printDetails prints a change's evidence, new tests, dependencies and
// whether the server merged it onto newer work.
func (a *App) printDetails(cs *vxv1.Changeset, indent string) {
	if cs.Merged {
		a.printf("%smerged onto newer work by the server; evidence below was gathered before the merge\n", indent)
	}
	if cs.Rebased {
		onto := cs.RebasedOntoBranch
		if onto == "" {
			onto = "the target"
		}
		a.printf("%srebased onto %s at %s", indent, onto, short(cs.RebasedOnto))
		if cs.RebasedBy != "" {
			a.printf(" by %s", cs.RebasedBy)
		}
		a.printf("; evidence below was gathered before the rebase\n")
		if cs.ResolvedBy != "" {
			a.printf("%sconflict resolved during the rebase by %s\n", indent, cs.ResolvedBy)
		}
	}
	if ev := cs.Evidence; ev != nil {
		for _, ch := range ev.Checks {
			how := "reported"
			if ch.RanByVx {
				how = fmt.Sprintf("run by vx, %.1fs", float64(ch.DurationMs)/1000)
			}
			a.printf("%scheck %s: %s (%s", indent, ch.Name, ch.Result, how)
			if ch.Passed > 0 || ch.Failed > 0 {
				a.printf(", %d passed, %d failed", ch.Passed, ch.Failed)
			}
			if ch.Notes != "" {
				a.printf(", %s", ch.Notes)
			}
			a.printf(")\n")
		}
		if len(ev.NewTests) > 0 {
			a.printf("%snew tests: %s\n", indent, strings.Join(ev.NewTests, ", "))
		}
		for _, c := range ev.NotVerified {
			a.printf("%snot verified: %s\n", indent, rules.NotVerified(c))
		}
		for _, ap := range ev.Approvals {
			a.printf("%sapproved by %s (%s, landing policy in vx.toml for %s)\n", indent, ap.By, ap.At, strings.Join(ap.Paths, ", "))
		}
	}
	if len(cs.DependsOn) > 0 {
		a.printf("%sdepends on: %s\n", indent, strings.Join(cs.DependsOn, ", "))
	}
	for _, w := range cs.Warnings {
		a.printf("%swarning: %s\n", indent, w.Detail)
	}
}
