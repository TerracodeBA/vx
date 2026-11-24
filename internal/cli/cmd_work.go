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
		Long: `Open a workspace: one task, described by its intent, that ends in one land.
Other agents see your intent in vx status while you work. The intent
becomes the change's description and, when shipped, its commit message.

Keep tasks small and land often: vx land closes the workspace, so run
vx new again for the next task.`,
		Args: cobra.ExactArgs(1),
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
			if len(marked) > 0 {
				a.printf("\nUnresolved conflict markers in: %s\nvx land will refuse these files until the markers are gone.\n", strings.Join(marked, ", "))
			} else if len(changes) > 0 {
				a.printf("\nNo conflict markers; ready to land.\n")
			}
			out["conflict_markers"] = marked
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
			a.printf(")\n")
		}
		if len(ev.NewTests) > 0 {
			a.printf("%snew tests: %s\n", indent, strings.Join(ev.NewTests, ", "))
		}
	}
	if len(cs.DependsOn) > 0 {
		a.printf("%sdepends on: %s\n", indent, strings.Join(cs.DependsOn, ", "))
	}
	for _, w := range cs.Warnings {
		a.printf("%swarning: %s\n", indent, w.Detail)
	}
}
