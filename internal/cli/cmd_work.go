package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
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
			if err := c.FetchTree(ctx, wc.Objects, tree); err != nil {
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
	return &cobra.Command{
		Use:   "new INTENT",
		Short: "Open a workspace for one task in the current effort",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			if wc.Config.Workspace != "" {
				return fmt.Errorf("workspace %q is still open: land it with `vx land` or close it with `vx abandon`", wc.Config.Intent)
			}
			w, err := a.openWorkspace(ctxOf(cmd), c, wc, args[0])
			if err != nil {
				return err
			}
			a.printf("Opened workspace %s: %s\nEdit files, then run `vx land`.\n", w.ChangeId, w.Intent)
			a.emit(w)
			return nil
		},
	}
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
			a.printf("Effort %s in %s, files at %s\n", wc.Config.Effort, wc.Config.Repo, short(wc.Config.Base))
			if wc.Config.Workspace != "" {
				a.printf("Workspace %s: %s\n", wc.Config.ChangeID, wc.Config.Intent)
				out["workspace"] = map[string]string{"id": wc.Config.Workspace, "change_id": wc.Config.ChangeID, "intent": wc.Config.Intent}
			} else {
				a.printf("No workspace open. Start one with: vx new \"INTENT\"\n")
			}
			if c, err := a.client(); err == nil {
				if eff, err := c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort})); err == nil {
					head := eff.Msg.Effort.Head
					out["effort_head"] = head
					if head != wc.Config.Base {
						a.printf("The effort moved on to %s. Run `vx sync` to pick up others' changes.\n", short(head))
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

// ---------- land ----------

func (a *App) landCmd() *cobra.Command {
	var intent, model, session string
	var checks, dependsOn, newTests []string
	var allowMarkers bool
	cmd := &cobra.Command{
		Use:   "land",
		Short: "Land your changes into the effort",
		Long: `Land the working copy's changes into the effort.

If others landed in the meantime, the server merges your changes onto
theirs. Only real conflicts stop the land; then run vx sync, fix the
conflicting files and land again.

Record what you checked, so reviewers see the evidence:
  vx land --check "go test ./...=pass:142"`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			if wc.Config.Workspace == "" {
				if intent == "" {
					return errors.New("no workspace open: run `vx new \"INTENT\"` first, or pass --intent")
				}
				if _, err := a.openWorkspace(ctx, c, wc, intent); err != nil {
					return err
				}
			}
			evidence, err := parseChecks(checks, newTests)
			if err != nil {
				return err
			}
			tree, changes, err := wc.Snapshot(ctx)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				return errors.New("nothing to land: no local changes")
			}
			if !allowMarkers {
				for _, ch := range changes {
					if ch.Op == object.OpDelete {
						continue
					}
					data, err := wc.ReadBlob(ctx, ch.New.Hash)
					if err != nil {
						return err
					}
					if workcopy.HasConflictMarkers(data) {
						return fmt.Errorf("%s still has conflict markers; resolve them or pass --allow-markers", ch.Path)
					}
				}
			}
			trees, blobs, err := object.NewObjects(ctx, wc.Objects, wc.Config.BaseTreeHash(), tree)
			if err != nil {
				return err
			}
			if _, err := c.Upload(ctx, wc.Objects, append(trees, blobs...)); err != nil {
				return fmt.Errorf("upload: %w", err)
			}
			if model == "" {
				model = a.Getenv("VX_MODEL")
			}
			if session == "" {
				session = a.Getenv("VX_SESSION")
			}
			resp, err := c.Workspaces.Land(ctx, connect.NewRequest(&vxv1.LandRequest{
				WorkspaceId: wc.Config.Workspace, Base: wc.Config.Base, Tree: tree.String(),
				Intent: intent, Evidence: evidence, DependsOn: dependsOn, Model: model, Session: session,
			}))
			if err != nil {
				return err
			}
			r := resp.Msg
			if !r.Landed {
				a.printf("Not landed: your changes conflict with changes already in the effort (now at %s).\n\n", short(r.EffortHead))
				for _, cf := range r.Conflicts {
					a.printf("  %-14s %s %s\n", cf.Kind, cf.Path, cf.Detail)
				}
				a.printf("\nRun `vx sync` to bring in those changes and mark the conflicts, fix them, then `vx land` again.\n")
				a.emit(r)
				return errConflict
			}
			// Bring the working copy to the new effort head: our snapshot
			// plus whatever the merge brought in from others.
			cs := r.Changeset
			newTree, err := hash.Parse(cs.Tree)
			if err != nil {
				return err
			}
			if err := c.FetchTree(ctx, wc.Objects, newTree); err != nil {
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
			if err := wc.Save(); err != nil {
				return err
			}
			var added, deleted int32
			for _, f := range cs.Files {
				added += f.Added
				deleted += f.Deleted
			}
			a.printf("Landed %s (%s): %s\n  %d file(s), +%d -%d\n", short(cs.Hash), cs.ChangeId, firstLine(cs.Intent), len(cs.Files), added, deleted)
			if r.Rebased {
				a.printf("  The effort had moved; your changes were merged onto it")
				if len(r.AutoMerged) > 0 {
					a.printf(" (merged line by line: %s)", strings.Join(r.AutoMerged, ", "))
				}
				a.printf(".\n")
			}
			if len(incoming) > 0 {
				a.printf("  Updated %d file(s) here with others' changes.\n", len(incoming))
			}
			a.emit(r)
			return nil
		},
	}
	cmd.Flags().StringVarP(&intent, "intent", "m", "", "what this change does (default: the workspace intent)")
	cmd.Flags().StringArrayVar(&checks, "check", nil, `evidence, as "NAME=pass|fail[:PASSED[:FAILED]]" (repeatable)`)
	cmd.Flags().StringArrayVar(&newTests, "new-test", nil, "name of a test this change adds (repeatable)")
	cmd.Flags().StringArrayVar(&dependsOn, "depends-on", nil, "symbol this change relies on, e.g. auth.Login (repeatable)")
	cmd.Flags().StringVar(&model, "model", "", "model that wrote the change (default: VX_MODEL)")
	cmd.Flags().StringVar(&session, "session", "", "agent session id (default: VX_SESSION)")
	cmd.Flags().BoolVar(&allowMarkers, "allow-markers", false, "land even if files contain conflict markers")
	return cmd
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

func (a *App) syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Bring in changes others landed, keeping your local edits",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			eff, err := c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort}))
			if err != nil {
				return err
			}
			head := eff.Msg.Effort.Head
			if head == wc.Config.Base {
				a.printf("Up to date at %s.\n", short(head))
				a.emit(map[string]any{"up_to_date": true, "base": head})
				return nil
			}
			cs, err := c.Efforts.GetChangeset(ctx, connect.NewRequest(&vxv1.GetChangesetRequest{Repo: wc.Config.Repo, Ref: head}))
			if err != nil {
				return err
			}
			theirs, err := hash.Parse(cs.Msg.Changeset.Tree)
			if err != nil {
				return err
			}
			if err := c.FetchTree(ctx, wc.Objects, theirs); err != nil {
				return err
			}
			ours, local, err := wc.Snapshot(ctx)
			if err != nil {
				return err
			}
			result := theirs
			var conflicts []engine.Conflict
			if len(local) > 0 {
				mr, err := engine.MergeTreesKeepConflicts(ctx, wc.Objects, wc.Config.BaseTreeHash(), ours, theirs, merge.Labels{
					Base: "base " + short(wc.Config.Base), Ours: "local", Theirs: "effort " + short(head),
				})
				if err != nil {
					return err
				}
				result, conflicts = mr.Tree, mr.Conflicts
			}
			incoming, err := object.DiffTrees(ctx, wc.Objects, ours, result)
			if err != nil {
				return err
			}
			if err := wc.Apply(ctx, incoming); err != nil {
				return err
			}
			wc.Config.Base, wc.Config.BaseTree = head, theirs.String()
			if wc.Config.Workspace != "" {
				if _, err := c.Workspaces.SetBase(ctx, connect.NewRequest(&vxv1.SetBaseRequest{Id: wc.Config.Workspace, Base: head})); err != nil {
					return err
				}
			}
			if err := wc.Save(); err != nil {
				return err
			}
			a.printf("Synced to %s: %d file(s) updated", short(head), len(incoming))
			if len(local) > 0 {
				a.printf(", your %d local change(s) kept", len(local))
			}
			a.printf(".\n")
			var cj []map[string]string
			for _, cf := range conflicts {
				cj = append(cj, map[string]string{"path": cf.Path, "kind": string(cf.Kind), "detail": cf.Detail})
			}
			out := map[string]any{"base": head, "updated": len(incoming), "local_changes": len(local), "conflicts": cj}
			if len(conflicts) > 0 {
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
				a.printf("\nThen run `vx land`.\n")
				a.emit(out)
				return errConflict
			}
			a.emit(out)
			return nil
		},
	}
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
			wc.Config.Workspace, wc.Config.Intent, wc.Config.ChangeID = "", "", ""
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
				if cs.Evidence != nil {
					for _, ch := range cs.Evidence.Checks {
						a.printf("    check %s: %s", ch.Name, ch.Result)
						if ch.Passed > 0 || ch.Failed > 0 {
							a.printf(" (%d passed, %d failed)", ch.Passed, ch.Failed)
						}
						a.printf("\n")
					}
				}
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
