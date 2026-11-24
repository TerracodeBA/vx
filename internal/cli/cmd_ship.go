package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

func (a *App) shipCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Turn an effort into a pull request on the git host",
		Long: `Ship an effort: every landed change becomes one commit, replayed onto
the latest head of the target branch, pushed to vx/<effort>. On GitHub a
pull request is opened, or updated if one is open. vx never merges; that
stays a decision on the git host.`,
	}
	var repoFlag, target string

	printPlan := func(p *vxv1.ShipmentPlan) {
		a.printf("Shipping to %s (at %s) on branch %s\n\n", p.Target, short(p.TargetGitSha), p.Branch)
		for _, st := range p.Steps {
			cs := st.Changeset
			mark := " "
			if st.Skipped {
				mark = "="
			}
			a.printf(" %s %s  %-50s %s\n", mark, cs.ChangeId, firstLine(cs.Intent), cs.Author.Principal)
		}
		if len(p.Conflicts) > 0 {
			a.printf("\nChange %s conflicts with %s:\n", p.ConflictedChange, p.Target)
			for _, cf := range p.Conflicts {
				a.printf("  %-14s %s\n", cf.Kind, cf.Path)
			}
		}
	}

	plan := &cobra.Command{
		Use:   "plan [EFFORT]",
		Short: "Show the commits the effort would ship",
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
			resp, err := c.Ships.PlanShipment(ctxOf(cmd), connect.NewRequest(&vxv1.PlanShipmentRequest{Repo: repo, Effort: slug, Target: target}))
			if err != nil {
				return err
			}
			printPlan(resp.Msg.Plan)
			if len(resp.Msg.Plan.Conflicts) == 0 {
				a.printf("\nTitle: %s\n\n%s", resp.Msg.Plan.Title, resp.Msg.Plan.Body)
			}
			a.emit(resp.Msg)
			if len(resp.Msg.Plan.Conflicts) > 0 {
				return errConflict
			}
			return nil
		},
	}

	var draft bool
	var title, body string
	open := &cobra.Command{
		Use:   "open [EFFORT]",
		Short: "Push vx/<effort> and open or update the pull request",
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
			resp, err := c.Ships.OpenShipment(ctxOf(cmd), connect.NewRequest(&vxv1.OpenShipmentRequest{
				Repo: repo, Effort: slug, Target: target, Title: title, Body: body, Draft: draft,
			}))
			if err != nil {
				return err
			}
			s := resp.Msg.Shipment
			printPlan(resp.Msg.Plan)
			a.printf("\nPushed %s (%d commit(s)) on top of %s.\n", s.Branch, len(s.ChangeIds), s.Target)
			switch {
			case s.Url != "" && resp.Msg.PrUpdated:
				a.printf("Updated pull request #%s: %s\n", s.PrNumber, s.Url)
			case s.Url != "":
				a.printf("Opened pull request #%s: %s\n", s.PrNumber, s.Url)
			default:
				a.printf("This host has no pull requests; open one from %s yourself.\n", s.Branch)
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	open.Flags().BoolVar(&draft, "draft", false, "open the pull request as a draft")
	open.Flags().StringVar(&title, "title", "", "pull request title (default: the effort name)")
	open.Flags().StringVar(&body, "body", "", "pull request body (default: generated from intents and evidence)")

	list := &cobra.Command{
		Use:   "list [EFFORT]",
		Short: "List the effort's shipments",
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
			resp, err := c.Ships.ListShipments(ctxOf(cmd), connect.NewRequest(&vxv1.ListShipmentsRequest{Repo: repo, Effort: slug}))
			if err != nil {
				return err
			}
			if len(resp.Msg.Shipments) == 0 {
				a.printf("Not shipped yet. Run: vx ship open\n")
			}
			for _, s := range resp.Msg.Shipments {
				a.printf("%-28s -> %-12s %-7s %d commit(s)  %s\n", s.Branch, s.Target, s.State, len(s.ChangeIds), s.Url)
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	for _, c := range []*cobra.Command{plan, open, list} {
		c.Flags().StringVar(&repoFlag, "repo", "", "repository")
	}
	for _, c := range []*cobra.Command{plan, open} {
		c.Flags().StringVar(&target, "to", "", "target branch (default: the effort's base branch)")
	}
	cmd.AddCommand(plan, open, list)
	return cmd
}
