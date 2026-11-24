package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

func (a *App) shipCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Turn an effort into a pull request on the git host",
		Long: `Ship an effort: every landed change becomes one commit, replayed onto
the latest head of the target branch, pushed to vx/<effort>. On GitHub,
GitLab (a merge request), Gitea, Forgejo and Bitbucket a pull request is
opened, or updated if one is open. vx never merges; that stays a decision
on the git host.

With --split an effort ships as several pull requests, one per part:
  independent  parts merge in any order; changes that touch a common file
               or depend on each other stay in one part
  stacked      each part is based on the previous part's branch; by
               default one part per change
Branches are vx/<effort>/<n>-<name>. vx ship plan --split shows the parts;
--group IDS (repeatable, comma-separated change ids) sets them yourself.`,
	}
	var repoFlag, target, split string
	var groupFlags []string
	groupsPB := func() []*vxv1.ShipGroupInput {
		var out []*vxv1.ShipGroupInput
		for _, g := range groupFlags {
			ids := strings.FieldsFunc(g, func(r rune) bool { return r == ',' || r == ' ' })
			out = append(out, &vxv1.ShipGroupInput{ChangeIds: ids})
		}
		return out
	}
	checkSplit := func() error {
		if len(groupFlags) > 0 && split == "" {
			return fmt.Errorf("--group needs --split independent or --split stacked")
		}
		return nil
	}
	// An older server ignores the split fields and answers with one PR.
	splitServed := func(p *vxv1.ShipmentPlan) error {
		if split != "" && p.Split == "" {
			return fmt.Errorf("this server does not split efforts into several pull requests yet; it needs vx 0.4 or later (vx version shows the server's)")
		}
		return nil
	}

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
		if len(p.Warnings) > 0 {
			a.printf("\n")
			a.printWarnings(p.Warnings)
		}
	}

	printSplit := func(p *vxv1.ShipmentPlan, bodies bool) {
		how := "independent: they merge in any order"
		if p.Split == "stacked" {
			how = "stacked: each is based on the one before"
		}
		who := "your grouping"
		if p.Suggested {
			who = "grouping suggested by vx"
		}
		a.printf("Shipping to %s (at %s) as %d pull requests, %s (%s)\n", p.Target, short(p.TargetGitSha), len(p.Groups), how, who)
		for _, g := range p.Groups {
			pr := ""
			if g.PrNumber != "" {
				pr = "  updates #" + g.PrNumber
			}
			a.printf("\nPart %d: %s\n  %s -> %s%s\n", g.Part, g.Title, g.Branch, g.Base, pr)
			for _, st := range g.Steps {
				cs := st.Changeset
				mark := " "
				if st.Skipped {
					mark = "="
				}
				a.printf("  %s %s  %-50s %s\n", mark, cs.ChangeId, firstLine(cs.Intent), cs.Author.Principal)
			}
			if len(g.Files) > 0 {
				a.printf("  files: %s\n", strings.Join(g.Files, ", "))
			}
			for _, w := range g.Why {
				a.printf("  together: %s\n", w)
			}
			if g.Problem != "" {
				a.printf("  CANNOT SHIP: %s\n", g.Problem)
			}
			if len(g.Warnings) > 0 {
				a.printWarnings(g.Warnings)
			}
			if bodies && g.Problem == "" {
				a.printf("\n  Title: %s\n\n%s", g.Title, indent(g.Body, "    "))
			}
		}
		for _, n := range p.Notes {
			a.printf("\nNote: %s\n", n)
		}
		for _, sh := range p.Stale {
			ref := sh.Branch
			if sh.PrNumber != "" {
				ref = "#" + sh.PrNumber + " (" + sh.Branch + ")"
			}
			a.printf("\n%s is no longer part of the split: vx stops updating it; close it on the host.\n", ref)
		}
		if len(p.Problems) > 0 {
			a.printf("\nThis split cannot ship:\n")
			for _, pr := range p.Problems {
				a.printf("  - %s\n", pr)
			}
			a.printf("Regroup with --group IDS (one flag per part), or try --split stacked.\n")
		}
	}

	plan := &cobra.Command{
		Use:   "plan [EFFORT]",
		Short: "Show the commits the effort would ship",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkSplit(); err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, slug, err := a.resolveEffort(repoFlag, args)
			if err != nil {
				return err
			}
			resp, err := c.Ships.PlanShipment(ctxOf(cmd), connect.NewRequest(&vxv1.PlanShipmentRequest{Repo: repo, Effort: slug, Target: target, Split: split, Groups: groupsPB()}))
			if err != nil {
				return err
			}
			if err := splitServed(resp.Msg.Plan); err != nil {
				return err
			}
			if split != "" {
				printSplit(resp.Msg.Plan, true)
				a.emit(resp.Msg)
				if len(resp.Msg.Plan.Problems) > 0 {
					return errConflict
				}
				return nil
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
			if err := checkSplit(); err != nil {
				return err
			}
			if split != "" && (title != "" || body != "") {
				return fmt.Errorf("--title and --body are for one pull request; with --split every part gets its own")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, slug, err := a.resolveEffort(repoFlag, args)
			if err != nil {
				return err
			}
			if split != "" {
				// Ask first: an older server would ignore --split and ship one PR.
				pr, err := c.Ships.PlanShipment(ctxOf(cmd), connect.NewRequest(&vxv1.PlanShipmentRequest{Repo: repo, Effort: slug, Target: target, Split: split, Groups: groupsPB()}))
				if err != nil {
					return err
				}
				if err := splitServed(pr.Msg.Plan); err != nil {
					return err
				}
			}
			resp, err := c.Ships.OpenShipment(ctxOf(cmd), connect.NewRequest(&vxv1.OpenShipmentRequest{
				Repo: repo, Effort: slug, Target: target, Title: title, Body: body, Draft: draft,
				Split: split, Groups: groupsPB(),
			}))
			if err != nil {
				return err
			}
			if split != "" {
				printSplit(resp.Msg.Plan, false)
				a.printf("\n")
				for i, s := range resp.Msg.Shipments {
					updated := i < len(resp.Msg.PrsUpdated) && resp.Msg.PrsUpdated[i]
					switch {
					case s.State == "pushed" && s.Url != "":
						a.printf("Part %d: pushed %s; open its pull request here: %s\n", s.Part, s.Branch, s.Url)
					case s.Url != "" && updated:
						a.printf("Part %d: pushed %s, updated pull request #%s: %s\n", s.Part, s.Branch, s.PrNumber, s.Url)
					case s.Url != "":
						a.printf("Part %d: pushed %s, opened pull request #%s: %s\n", s.Part, s.Branch, s.PrNumber, s.Url)
					default:
						a.printf("Part %d: pushed %s (%d commit(s)) for %s; this host has no pull requests.\n", s.Part, s.Branch, len(s.ChangeIds), s.Target)
					}
				}
				a.emit(resp.Msg)
				return nil
			}
			s := resp.Msg.Shipment
			printPlan(resp.Msg.Plan)
			a.printf("\nPushed %s (%d commit(s)) on top of %s.\n", s.Branch, len(s.ChangeIds), s.Target)
			switch {
			case s.State == "pushed" && s.Url != "":
				a.printf("The server has no API token for this host, so open the pull request here:\n  %s\n", s.Url)
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
				part := ""
				if s.Part > 0 {
					part = fmt.Sprintf("  part %d, %s", s.Part, s.Split)
				}
				a.printf("%-28s -> %-12s %-7s %d commit(s)  %s%s\n", s.Branch, s.Target, s.State, len(s.ChangeIds), s.Url, part)
				if line := ciLine(s); line != "" {
					a.printf("  %s\n", line)
				}
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
		c.Flags().StringVar(&split, "split", "", "ship several pull requests: independent or stacked")
		c.Flags().StringArrayVar(&groupFlags, "group", nil, "with --split: change ids of one part, comma-separated (repeat per part, in order)")
	}
	cmd.AddCommand(plan, open, list)
	return cmd
}

// indent prefixes every line of s.
func indent(s, prefix string) string {
	lines := strings.SplitAfter(s, "\n")
	var b strings.Builder
	for _, l := range lines {
		if l == "" {
			continue
		}
		if l == "\n" {
			b.WriteString(l)
			continue
		}
		b.WriteString(prefix + l)
	}
	return b.String()
}
