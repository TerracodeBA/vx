package cli

import (
	"context"
	"errors"
	"fmt"
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

const generatedHint = "generated file: regenerate it (for example `buf generate`) instead of editing the markers"

func (a *App) effortRebaseCmd() *cobra.Command {
	var repoFlag, onto string
	var cont, abort bool
	cmd := &cobra.Command{
		Use:   "rebase [EFFORT] [--onto BRANCH] [--continue | --abort]",
		Short: "Move the effort onto the latest head of its base branch",
		Long: `When the base branch moved on (another effort shipped), vx ship plan stops
with a conflict. vx effort rebase brings the effort up to date: the server
imports the branch's new commits, then replays the effort's changes, oldest
first, onto its head. Each change keeps its change id, intent, author and
evidence; it is marked as rebased, and its evidence as gathered before the
rebase. The effort head moves only when every change replayed; changes
landed meanwhile are replayed too, so nothing is lost.

If a change conflicts, the rebase stops there. In a working copy of the
effort (with no local changes), vx writes that change replayed onto the
branch into the files, with conflict markers, and prints what to do:
  edit the files, then          vx effort rebase --continue
  or drop the whole rebase:     vx effort rebase --abort
A generated file ("// Code generated ... DO NOT EDIT.") gets the effort's
version instead of markers: regenerate it (for example buf generate), then
continue. --continue in a working copy that does not hold the conflicts
yet writes them there, so another agent can take over a stopped rebase.

Only one rebase per effort runs at a time. Open workspaces keep working:
their next vx sync or vx land merges their edits onto the new head.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cont && abort {
				return errors.New("pass --continue or --abort, not both")
			}
			if (cont || abort) && onto != "" {
				return errors.New("--onto only applies when starting a rebase")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, slug, err := a.resolveEffort(repoFlag, args)
			if err != nil {
				return err
			}
			var wc *workcopy.WorkingCopy
			if w, err := a.workingCopy(); err == nil && w.Config.Repo == repo && w.Config.Effort == slug {
				wc = w
			}
			ctx := ctxOf(cmd)
			switch {
			case abort:
				return a.rebaseAbort(ctx, c, wc, repo, slug)
			case cont:
				return a.rebaseContinue(ctx, c, wc, repo, slug)
			}
			return a.rebaseStart(ctx, c, wc, repo, slug, onto)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository")
	cmd.Flags().StringVar(&onto, "onto", "", "branch to rebase onto (default: the effort's base branch)")
	cmd.Flags().BoolVar(&cont, "continue", false, "upload your resolution of the stopped change and replay the rest")
	cmd.Flags().BoolVar(&abort, "abort", false, "drop the rebase in progress; the effort stays as it was")
	return cmd
}

// errRebasing is what land, sync and new say while this working copy holds
// a stopped rebase's conflicts.
func errRebasing(wc *workcopy.WorkingCopy) error {
	r := wc.Config.Rebase
	return fmt.Errorf("this working copy is in the middle of a rebase of %s (stopped at change %s): resolve the conflicts, then run `vx effort rebase --continue`, or `vx effort rebase --abort` to drop it", wc.Config.Effort, r.ChangeID)
}

// requireClean refuses to write a rebase into a working copy with edits.
func requireClean(ctx context.Context, wc *workcopy.WorkingCopy) error {
	changes, err := wc.Status(ctx)
	if err != nil {
		return err
	}
	if len(changes) > 0 {
		return fmt.Errorf("this working copy has %d local change(s); a rebase writes its conflicts into these files. Land them first (vx land), or run vx effort rebase from a clean working copy", len(changes))
	}
	return nil
}

func (a *App) rebaseStart(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, repo, slug, onto string) error {
	if wc != nil {
		if wc.Config.Rebase != nil {
			return errRebasing(wc)
		}
		if err := requireClean(ctx, wc); err != nil {
			return err
		}
	}
	resp, err := c.Efforts.RebaseEffort(ctx, connect.NewRequest(&vxv1.RebaseEffortRequest{Repo: repo, Effort: slug, Onto: onto}))
	if err != nil {
		return err
	}
	m := resp.Msg
	if m.UpToDate {
		a.printf("Effort %s is up to date: it already sits on %s at %s.\n", slug, m.Effort.BaseBranch, short(m.Effort.Base))
		a.emit(m)
		return nil
	}
	return a.rebaseOutcome(ctx, c, wc, repo, slug, m.Rebase, m)
}

func (a *App) rebaseContinue(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, repo, slug string) error {
	if wc == nil {
		return fmt.Errorf("run vx effort rebase --continue in a working copy of %s (vx clone %s %s): the conflicts are written there", slug, repo, slug)
	}
	got, err := c.Efforts.GetRebase(ctx, connect.NewRequest(&vxv1.GetRebaseRequest{Repo: repo, Effort: slug}))
	if err != nil {
		return err
	}
	rb := got.Msg.Rebase
	if rb == nil || (rb.State != "stopped" && rb.State != "running") {
		if wc.Config.Rebase != nil {
			return fmt.Errorf("the rebase of %s is no longer in progress (someone finished or aborted it); run `vx effort rebase --abort` to put this working copy back", slug)
		}
		return fmt.Errorf("effort %s has no rebase in progress; start one with: vx effort rebase", slug)
	}
	if rb.State == "running" {
		return fmt.Errorf("the rebase of %s by %s is running right now; wait a moment, then run vx status", slug, rb.StartedBy)
	}
	if wc.Config.Rebase == nil || wc.Config.Rebase.StoppedAt != rb.StoppedAt.GetHash() {
		// Take over: write the stopped change's conflicts here.
		if wc.Config.Rebase == nil {
			if err := requireClean(ctx, wc); err != nil {
				return err
			}
		}
		if err := a.writeRebaseConflicts(ctx, c, wc, rb); err != nil {
			return err
		}
		a.printf("The rebase of %s is stopped at change %s; its conflicts are now written here.\n", slug, rb.StoppedAt.ChangeId)
		a.printRebaseStop(wc, rb)
		a.emit(got.Msg)
		return errConflict
	}

	tree, changes, err := wc.Snapshot(ctx)
	if err != nil {
		return err
	}
	if bad, err := filesWithMarkers(ctx, wc, changes); err != nil {
		return err
	} else if len(bad) > 0 {
		return fmt.Errorf("%s still has conflict markers; resolve them, then run vx effort rebase --continue again", strings.Join(bad, ", "))
	}
	trees, blobs, err := object.NewObjects(ctx, wc.Objects, wc.Config.BaseTreeHash(), tree)
	if err != nil {
		return err
	}
	if _, err := c.Upload(ctx, wc.Config.Repo, wc.Objects, append(trees, blobs...)); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	resp, err := c.Efforts.ContinueRebase(ctx, connect.NewRequest(&vxv1.ContinueRebaseRequest{
		Repo: repo, Effort: slug, StoppedAt: wc.Config.Rebase.StoppedAt, Tree: tree.String(),
	}))
	if err != nil {
		return err
	}
	a.printf("Recorded your resolution of change %s.\n", wc.Config.Rebase.ChangeID)
	return a.rebaseOutcome(ctx, c, wc, repo, slug, resp.Msg.Rebase, resp.Msg)
}

func (a *App) rebaseAbort(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, repo, slug string) error {
	resp, err := c.Efforts.AbortRebase(ctx, connect.NewRequest(&vxv1.AbortRebaseRequest{Repo: repo, Effort: slug}))
	if err != nil && !(connect.CodeOf(err) == connect.CodeFailedPrecondition && wc != nil && wc.Config.Rebase != nil) {
		return err
	}
	if err == nil {
		a.printf("Aborted the rebase of %s; the effort is unchanged.\n", slug)
	} else {
		a.printf("The rebase of %s was no longer in progress on the server.\n", slug)
	}
	if wc != nil && wc.Config.Rebase != nil {
		saved, err := hash.Parse(wc.Config.Rebase.SavedBaseTree)
		if err != nil {
			return err
		}
		if err := c.FetchTree(ctx, wc.Config.Repo, wc.Objects, saved); err != nil {
			return err
		}
		cur, _, err := wc.Snapshot(ctx)
		if err != nil {
			return err
		}
		back, err := object.DiffTrees(ctx, wc.Objects, cur, saved)
		if err != nil {
			return err
		}
		if err := wc.Apply(ctx, back); err != nil {
			return err
		}
		wc.Config.Base, wc.Config.BaseTree = wc.Config.Rebase.SavedBase, wc.Config.Rebase.SavedBaseTree
		wc.Config.Rebase = nil
		if err := wc.Save(); err != nil {
			return err
		}
		a.printf("Put this working copy back at %s (%d file(s) restored). Run `vx sync` if the effort moved on.\n", short(wc.Config.Base), len(back))
	}
	if resp != nil {
		a.emit(resp.Msg)
	} else {
		a.emit(map[string]any{"aborted": false, "restored": true})
	}
	return nil
}

// rebaseOutcome reports a finished or stopped rebase and updates the
// working copy: to the new head when done, to the conflicts when stopped.
func (a *App) rebaseOutcome(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, repo, slug string, rb *vxv1.Rebase, msg any) error {
	onto := rb.OntoBranch + " at " + short(rb.Onto)
	if rb.OntoGitSha != "" {
		onto = rb.OntoBranch + " at " + short(rb.OntoGitSha)
	}
	switch rb.State {
	case "done":
		a.printf("Rebased effort %s onto %s: %d change(s) replayed, change ids kept, head now %s.\n", slug, onto, rb.Replayed, short(rb.NewHead))
		a.printf("Their evidence is marked as gathered before the rebase.\n")
		if wc != nil {
			if err := a.leaveRebase(ctx, c, wc); err != nil {
				return err
			}
		}
		a.printf("\nNext: vx ship plan\n")
		a.emit(msg)
		return nil
	case "stopped":
		a.printf("Rebase of %s onto %s stopped at change %s: %s\n", slug, onto, rb.StoppedAt.GetChangeId(), firstLine(rb.StoppedAt.GetIntent()))
		a.printf("  %d change(s) replayed, %d to go. The effort is unchanged until the rebase finishes; lands still work and are replayed too.\n", rb.Replayed, rb.Pending)
		if wc == nil {
			a.printf("\nConflicts:\n")
			for _, cf := range rb.Conflicts {
				a.printf("  %-14s %s\n", cf.Kind, cf.Path)
			}
			a.printf("\nTo resolve them, run vx effort rebase --continue in a clean working copy of %s (vx clone %s %s): it writes the conflicts there.\nTo drop the rebase: vx effort rebase %s --abort\n", slug, repo, slug, slug)
			a.emit(msg)
			return errConflict
		}
		if err := a.writeRebaseConflicts(ctx, c, wc, rb); err != nil {
			return err
		}
		a.printRebaseStop(wc, rb)
		a.emit(msg)
		return errConflict
	}
	return fmt.Errorf("the rebase of %s is %s", slug, rb.State)
}

// leaveRebase moves a working copy to the effort head after a rebase
// finished. A working copy that held the conflicts takes the new head as
// is (its resolution is in it); one without local changes just syncs.
func (a *App) leaveRebase(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) error {
	if wc.Config.Rebase == nil {
		_, err := a.syncWorkingCopy(ctx, c, wc)
		return err
	}
	eff, err := c.Efforts.GetEffort(ctx, connect.NewRequest(&vxv1.GetEffortRequest{Repo: wc.Config.Repo, Effort: wc.Config.Effort}))
	if err != nil {
		return err
	}
	head := eff.Msg.Effort.Head
	cs, err := c.Efforts.GetChangeset(ctx, connect.NewRequest(&vxv1.GetChangesetRequest{Repo: wc.Config.Repo, Ref: head}))
	if err != nil {
		return err
	}
	tree, err := hash.Parse(cs.Msg.Changeset.Tree)
	if err != nil {
		return err
	}
	if err := c.FetchTree(ctx, wc.Config.Repo, wc.Objects, tree); err != nil {
		return err
	}
	cur, _, err := wc.Snapshot(ctx)
	if err != nil {
		return err
	}
	incoming, err := object.DiffTrees(ctx, wc.Objects, cur, tree)
	if err != nil {
		return err
	}
	if err := wc.Apply(ctx, incoming); err != nil {
		return err
	}
	wc.Config.Base, wc.Config.BaseTree, wc.Config.Rebase = head, tree.String(), nil
	if wc.Config.Workspace != "" {
		if _, err := c.Workspaces.SetBase(ctx, connect.NewRequest(&vxv1.SetBaseRequest{Id: wc.Config.Workspace, Base: head})); err != nil {
			return err
		}
	}
	if err := wc.Save(); err != nil {
		return err
	}
	a.printf("This working copy is at the new head %s (%d file(s) updated).\n", short(head), len(incoming))
	return nil
}

// writeRebaseConflicts writes the stopped change, replayed onto the rebased
// chain so far, into the working copy with conflict markers, and puts the
// working copy into the rebasing state. Generated files get the change's
// version instead of markers.
func (a *App) writeRebaseConflicts(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, rb *vxv1.Rebase) error {
	if rb.StoppedAt == nil {
		return errors.New("the server did not say which change the rebase stopped at; upgrade vx-server")
	}
	var trees [3]hash.Hash
	for i, s := range []string{rb.BaseTree, rb.ChangeTree, rb.OntoTree} {
		h, err := hash.Parse(s)
		if err != nil || h.IsZero() {
			return fmt.Errorf("the rebase state lacks its trees (%q): upgrade vx-server", s)
		}
		if err := c.FetchTree(ctx, wc.Config.Repo, wc.Objects, h); err != nil {
			return err
		}
		trees[i] = h
	}
	base, change, onto := trees[0], trees[1], trees[2]
	parent := ""
	if len(rb.StoppedAt.Parents) > 0 {
		parent = short(rb.StoppedAt.Parents[0])
	}
	ontoLabel := rb.OntoBranch
	if rb.OntoGitSha != "" {
		ontoLabel += " " + short(rb.OntoGitSha)
	}
	mr, err := engine.MergeTreesKeepConflicts(ctx, wc.Objects, base, change, onto, merge.Labels{
		Base: "base " + parent, Ours: "change " + rb.StoppedAt.ChangeId, Theirs: ontoLabel + " (rebased)",
	})
	if err != nil {
		return err
	}
	generated := map[string]bool{}
	for _, cf := range rb.Conflicts {
		if cf.Generated {
			generated[cf.Path] = true
		}
	}
	tree := mr.Tree
	var fix []object.Change
	var conflicts, gen []string
	for _, cf := range mr.Conflicts {
		conflicts = append(conflicts, cf.Path)
		if !generated[cf.Path] {
			continue
		}
		gen = append(gen, cf.Path)
		if !cf.Ours.Hash.IsZero() {
			fix = append(fix, object.Change{Path: cf.Path, Op: object.OpModify, New: cf.Ours})
		}
	}
	if len(fix) > 0 {
		if tree, err = object.ApplyChanges(ctx, wc.Objects, tree, fix); err != nil {
			return err
		}
	}
	cur, _, err := wc.Snapshot(ctx)
	if err != nil {
		return err
	}
	incoming, err := object.DiffTrees(ctx, wc.Objects, cur, tree)
	if err != nil {
		return err
	}
	if err := wc.Apply(ctx, incoming); err != nil {
		return err
	}
	st := &workcopy.Rebasing{
		StoppedAt: rb.StoppedAt.Hash, ChangeID: rb.StoppedAt.ChangeId, Intent: rb.StoppedAt.Intent,
		OntoBranch: rb.OntoBranch, OntoGitSHA: rb.OntoGitSha, Conflicts: conflicts, Generated: gen,
		SavedBase: wc.Config.Base, SavedBaseTree: wc.Config.BaseTree,
	}
	if prev := wc.Config.Rebase; prev != nil {
		st.SavedBase, st.SavedBaseTree = prev.SavedBase, prev.SavedBaseTree
	}
	wc.Config.Rebase = st
	wc.Config.Base, wc.Config.BaseTree = rb.NewHead, onto.String()
	return wc.Save()
}

// printRebaseStop says which change stopped, which files conflict and
// what to do next.
func (a *App) printRebaseStop(wc *workcopy.WorkingCopy, rb *vxv1.Rebase) {
	gen := map[string]bool{}
	if r := wc.Config.Rebase; r != nil {
		for _, p := range r.Generated {
			gen[p] = true
		}
	}
	a.printf("\nConflicts to resolve:\n")
	for _, cf := range rb.Conflicts {
		hint := "edit the file and remove the markers"
		switch {
		case gen[cf.Path]:
			hint = generatedHint + "; the effort's version is written"
		case cf.Kind == string(engine.ConflictModifyDelete):
			hint = "the branch deleted it; the change's version is kept, delete it if that is right"
		case cf.Kind == string(engine.ConflictDeleteModify):
			hint = "the change deleted it but the branch changed it; the branch's version is kept, delete it again if that is right"
		case cf.Kind == string(engine.ConflictBinary) || cf.Kind == string(engine.ConflictMode):
			hint = "the change's version is kept"
		}
		a.printf("  %-14s %s (%s)\n", cf.Kind, cf.Path, hint)
	}
	a.printf("\nThe files here are change %s replayed onto %s, with conflict markers (<<<<<<< change ... >>>>>>> %s).\n", rb.StoppedAt.GetChangeId(), rb.OntoBranch, rb.OntoBranch)
	a.printf("Resolve them, then run `vx effort rebase --continue`. To drop the rebase: `vx effort rebase --abort`.\n")
}

// printRebaseStatus is the rebase part of vx status.
func (a *App) printRebaseStatus(wc *workcopy.WorkingCopy, server *vxv1.Rebase) map[string]any {
	if r := wc.Config.Rebase; r != nil {
		a.printf("Rebasing onto %s: stopped at change %s (%s).\n", r.OntoBranch, r.ChangeID, firstLine(r.Intent))
		if len(r.Conflicts) > 0 {
			a.printf("  Conflicts: %s\n", strings.Join(r.Conflicts, ", "))
		}
		for _, p := range r.Generated {
			a.printf("  %s: %s\n", p, generatedHint)
		}
		a.printf("  Resolve them, then `vx effort rebase --continue`, or `vx effort rebase --abort`.\n")
		return map[string]any{"here": true, "stopped_at": r.StoppedAt, "change_id": r.ChangeID, "onto_branch": r.OntoBranch, "conflicts": r.Conflicts, "generated": r.Generated}
	}
	if server == nil {
		return nil
	}
	where := "running"
	if server.StoppedAt != nil {
		where = "stopped on a conflict at change " + server.StoppedAt.ChangeId
	}
	a.printf("A rebase of this effort onto %s by %s is %s.\n", server.OntoBranch, server.StartedBy, where)
	a.printf("  Lands still work; they are replayed when it finishes. To take it over: vx effort rebase --continue\n")
	out := map[string]any{"here": false, "state": server.State, "started_by": server.StartedBy, "onto_branch": server.OntoBranch}
	if server.StoppedAt != nil {
		out["change_id"] = server.StoppedAt.ChangeId
	}
	return out
}
