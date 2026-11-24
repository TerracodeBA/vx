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
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// ---------- review ----------

func (a *App) reviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "List, approve and reject changes waiting for a person (landing policies)",
		Long: `A land that changes a file under a "human" [[policy]] of vx.toml waits for
a person: vx land stores it as a pending review and the effort head does not
move. People with write on the repository (and in the policy's approvers,
when it names them) approve it, which lands it with the normal merge, or
reject it with a reason. Agents never approve, not even for their own
person; you cannot approve a change you landed yourself.

  vx review list
  vx review show 1a2b3c4d
  vx review approve 1a2b3c4d
  vx review reject 1a2b3c4d --reason "Split the table change from the backfill"

The web UI has the same under Reviews.`,
	}
	cmd.AddCommand(a.reviewListCmd(), a.reviewShowCmd(), a.reviewApproveCmd(), a.reviewRejectCmd())
	return cmd
}

func (a *App) reviewListCmd() *cobra.Command {
	var repo, effort string
	var states []string
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pending reviews in the repositories you can read",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if effort != "" && repo == "" {
				if repo, err = a.resolveRepo(""); err != nil {
					return err
				}
			}
			if all {
				states = []string{"pending", "approved", "rejected", "withdrawn"}
			}
			resp, err := c.Reviews.ListReviews(ctxOf(cmd), connect.NewRequest(&vxv1.ListReviewsRequest{Repo: repo, Effort: effort, States: states}))
			if err != nil {
				return reviewsUnsupported(err)
			}
			rs := resp.Msg.Reviews
			if len(rs) == 0 {
				a.printf("No reviews waiting for a person.\n")
			}
			for _, r := range rs {
				a.printReviewLine(r)
			}
			a.emit(map[string]any{"reviews": rs})
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository")
	cmd.Flags().StringVar(&effort, "effort", "", "only this effort (with --repo, or in a working copy)")
	cmd.Flags().StringSliceVar(&states, "state", nil, "pending, approved, rejected or withdrawn (default: pending)")
	cmd.Flags().BoolVar(&all, "all", false, "every state")
	return cmd
}

func (a *App) printReviewLine(r *vxv1.Review) {
	who := r.Author.GetRef()
	a.printf("%s  %-9s %s/%s  %s  %s  by %s\n", noteID(r.Id), r.State, r.Repo, r.Effort, r.ChangeId, firstLine(r.Intent), who)
	a.printf("          files under the policy: %s\n", strings.Join(r.Paths, ", "))
	switch {
	case len(r.Conflicts) > 0:
		a.printf("          approved, but conflicts in %s: back with the author\n", strings.Join(r.Conflicts, ", "))
	case r.State == "rejected":
		a.printf("          rejected by %s: %s\n", r.DecidedBy.GetRef(), r.Reason)
	case r.State == "approved":
		a.printf("          approved by %s, landed as %s\n", r.DecidedBy.GetRef(), short(r.Landed))
	}
	if r.State == "pending" {
		if r.CanDecide {
			a.printf("          you can approve it: vx review approve %s\n", noteID(r.Id))
		} else if r.CannotDecide != "" {
			a.printf("          you cannot decide it: %s\n", r.CannotDecide)
		}
	}
}

func (a *App) reviewShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ID",
		Short: "Show one review: the change, its files, evidence and policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Reviews.GetReview(ctxOf(cmd), connect.NewRequest(&vxv1.GetReviewRequest{Id: args[0]}))
			if err != nil {
				return reviewsUnsupported(err)
			}
			r := resp.Msg.Review
			a.printReviewLine(r)
			for _, p := range r.Policies {
				a.printf("          policy: %s\n", p)
			}
			for _, l := range r.Approvers {
				a.printf("          approvers: %s\n", strings.Join(l.Approvers, ", "))
			}
			a.printf("\n")
			for _, f := range r.Files {
				a.printf("  %s %-48s +%d -%d\n", f.Op[:1], f.Path, f.Added, f.Deleted)
			}
			for _, ch := range r.GetEvidence().GetChecks() {
				how := "reported"
				if ch.RanByVx {
					how = "run by vx"
				}
				a.printf("  %s %s (%s)\n", ch.Result, ch.Name, how)
			}
			a.emit(map[string]any{"review": r})
			return nil
		},
	}
}

func (a *App) reviewApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve ID",
		Short: "Approve a pending review: it lands onto the current effort head",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Reviews.ApproveReview(ctxOf(cmd), connect.NewRequest(&vxv1.ApproveReviewRequest{Id: args[0]}))
			if err != nil {
				return reviewsUnsupported(err)
			}
			m := resp.Msg
			a.emit(m)
			if m.Land == nil {
				a.printf("Not landed: the change no longer merges with the effort, conflicts in:\n")
				for _, cf := range m.Conflicts {
					a.printf("  %-14s %s\n", cf.Kind, cf.Path)
				}
				a.printf("The review stays pending; vx told %s to resolve them and land again.\n", m.Review.Author.GetRef())
				return &exitError{code: ExitConflict, err: errors.New("approval conflicts: the review stays pending")}
			}
			a.printf("Approved review %s: %s landed as %s (%s).\n", noteID(m.Review.Id), m.Review.ChangeId, short(m.Land.Changeset.GetHash()), firstLine(m.Review.Intent))
			return nil
		},
	}
}

func (a *App) reviewRejectCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "reject ID --reason TEXT",
		Short: "Reject a pending review; the author reads the reason",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return errors.New(`say why: vx review reject ID --reason "TEXT" (the author reads it)`)
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Reviews.RejectReview(ctxOf(cmd), connect.NewRequest(&vxv1.RejectReviewRequest{Id: args[0], Reason: reason}))
			if err != nil {
				return reviewsUnsupported(err)
			}
			r := resp.Msg.Review
			a.printf("Rejected review %s (%s): %s. %s was told.\n", noteID(r.Id), r.ChangeId, r.Reason, r.Author.GetRef())
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why (required)")
	return cmd
}

// reviewsUnsupported explains an old server that has no ReviewService.
func reviewsUnsupported(err error) error {
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return errors.New("this server has no landing policies yet (it runs a vx older than v0.3); update the server")
	}
	return err
}

// settleReview checks the open workspace's pending review, if any. When a
// person decided it, the working copy hears of it once: approved, the
// workspace landed (vx sync brings the change in); rejected, the workspace
// stays open with the reason; withdrawn, it is forgotten. It returns the
// review while it is still pending, else nil.
func (a *App) settleReview(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) (*vxv1.Review, error) {
	if wc.Config.Review == "" {
		return nil, nil
	}
	resp, err := c.Reviews.GetReview(ctx, connect.NewRequest(&vxv1.GetReviewRequest{Id: wc.Config.Review}))
	if err != nil {
		return nil, err
	}
	r := resp.Msg.Review
	switch r.State {
	case "pending":
		return r, nil
	case "approved":
		a.printf("%s approved your change %s; it landed as %s.\n", r.DecidedBy.GetRef(), r.ChangeId, short(r.Landed))
		wc.Config.Workspace, wc.Config.Intent, wc.Config.ChangeID = "", "", ""
		wc.Config.PendingLand = nil
	case "rejected":
		a.printf("%s rejected your change %s: %s\nThe workspace is still open: change the files and vx land again for a new review, or vx abandon.\n", r.DecidedBy.GetRef(), r.ChangeId, r.Reason)
	}
	wc.Config.Review = ""
	return nil, wc.Save()
}

// printWaiting says the open workspace waits for a person.
func (a *App) printWaiting(r *vxv1.Review) {
	a.printf("%s\n", r.Waiting)
	if len(r.Conflicts) > 0 {
		a.printf("  A person approved it, but it conflicts with the effort in %s: run `vx sync`, resolve them and `vx land` again.\n", strings.Join(r.Conflicts, ", "))
	}
	a.printf("  Review %s; `vx wait` returns when a person decides.\n", noteID(r.Id))
}

func errWaitingForReview(r *vxv1.Review) error {
	return fmt.Errorf("workspace %s waits for a person to review it (review %s): vx wait returns when they decide; vx abandon withdraws it", r.ChangeId, noteID(r.Id))
}
