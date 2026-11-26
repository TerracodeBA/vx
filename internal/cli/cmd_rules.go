package cli

import (
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// ---------- rules, reserve ----------

func (a *App) rulesCmd() *cobra.Command {
	var repoFlag, effortFlag string
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "Show the repository's vx.toml rules and open reservations",
		Long: `Show the rules in vx.toml at the root of the repository, which vx applies on
every land, sync and ship:

  [[unique]]     files matching glob may not share a key (e.g. a migration
                 number); vx warns when a change makes two of them clash,
                 and vx reserve hands out the next free one
  [[generated]]  files matching glob are never text-merged: when both sides
                 changed one, vx keeps yours and says to run the command
  [checks]       required commands: vx land says "Not verified" for each
                 one it did not run with --run
  [[policy]]     how changes to some paths land: fast (default), gated
                 (required checks run by vx and passing) or human (a
                 person approves first; see vx review)
  [[check]]      what must be proven before a change lands or ships:
                 a command vx runs, or a check an agent, CI or a person
                 reports (see vx check, docs/checks.md)

Inside a working copy the rules come from the effort's head; outside one
from the default branch (or --effort).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, err := a.resolveRepo(repoFlag)
			if err != nil {
				return err
			}
			effort := effortFlag
			if effort == "" {
				if wc, err := a.workingCopy(); err == nil && wc.Config.Repo == repo {
					effort = wc.Config.Effort
				}
			}
			ctx := ctxOf(cmd)
			resp, err := c.Rules.GetRules(ctx, connect.NewRequest(&vxv1.GetRulesRequest{Repo: repo, Effort: effort}))
			if err != nil {
				return err
			}
			res, err := c.Rules.ListReservations(ctx, connect.NewRequest(&vxv1.ListReservationsRequest{Repo: repo}))
			if err != nil {
				return err
			}
			r := resp.Msg.Rules
			if r.GetSource() == "" {
				a.printf("No vx.toml in %s", repo)
				if effort != "" {
					a.printf(" (effort %s)", effort)
				}
				a.printf(": no rules. See docs/agents.md, \"Repository rules (vx.toml)\".\n")
			} else {
				a.printf("Rules from %s\n", r.Source)
				if len(r.Ignored) > 0 {
					a.printf("\nIgnored: written for a newer vx than this server; update vx-server to apply them:\n")
					for _, msg := range r.Ignored {
						a.printf("  %s\n", msg)
					}
				}
				if len(r.Unique) > 0 {
					a.printf("\nUnique (two files may not share the key; content: two lines may not share the value):\n")
					for _, u := range r.Unique {
						switch {
						case u.Content != "" && u.Scope != "":
							a.printf("  %-12s %s  content %s  per scope %s\n", u.Name, u.Glob, u.Content, u.Scope)
						case u.Content != "":
							a.printf("  %-12s %s  content %s\n", u.Name, u.Glob, u.Content)
						default:
							a.printf("  %-12s %s  key %s\n", u.Name, u.Glob, u.Key)
						}
					}
				}
				if len(r.Generated) > 0 {
					a.printf("\nGenerated (never text-merged; regenerate instead):\n")
					for _, g := range r.Generated {
						cmd := g.Command
						if cmd == "" {
							cmd = "(no command)"
						}
						a.printf("  %-30s %s\n", g.Glob, cmd)
					}
				}
				if len(r.RequiredChecks) > 0 {
					a.printf("\nRequired checks (vx land --run each):\n")
					for _, c := range r.RequiredChecks {
						a.printf("  %s\n", c)
					}
				}
				if len(r.Policies) > 0 {
					a.printf("\nLanding policies (the strictest matching one applies):\n")
					for _, p := range r.Policies {
						a.printf("  %-6s %s\n", p.Mode, strings.Join(p.Paths, ", "))
						if p.Mode != "fast" && len(p.Require) > 0 {
							a.printf("         needs, run by vx: %s\n", strings.Join(p.Require, "; "))
						}
						if len(p.Approvers) > 0 {
							a.printf("         approvers: %s\n", strings.Join(p.Approvers, ", "))
						}
					}
				}
				if len(r.Checks) > 0 {
					a.printf("\nChecks (vx check status shows what a change still needs):\n")
					w := 0
					for _, c := range r.Checks {
						if len(c.Name) > w {
							w = len(c.Name)
						}
					}
					for _, c := range r.Checks {
						how := c.Run
						if how == "" {
							how = "runner " + c.Runner
						} else {
							how = "run `" + how + "`"
						}
						a.printf("  %-*s  when %-6s blocks %-7s reporter %-11s %s\n", w, c.Name, c.When, c.Blocks, c.Reporter, how)
						if len(c.Paths) > 0 {
							a.printf("  %-*s  paths: %s\n", w, "", strings.Join(c.Paths, ", "))
						}
						if len(c.After) > 0 {
							a.printf("  %-*s  after: %s\n", w, "", strings.Join(c.After, ", "))
						}
						if len(c.OutputsRequired) > 0 || len(c.OutputsArtifacts) > 0 {
							a.printf("  %-*s  outputs: required %s, artifacts %s\n", w, "", listOrNone(c.OutputsRequired), listOrNone(c.OutputsArtifacts))
						}
					}
				}
			}
			if len(r.OrgChecks) > 0 {
				a.printf("\nOrganization checks, from %s (vx.toml cannot remove or weaken them):\n", r.OrgChecksFrom)
				for _, c := range r.OrgChecks {
					how := "runner " + c.Runner
					if c.Run != "" {
						how = "run `" + c.Run + "`"
					}
					a.printf("  %-16s when %-6s blocks %-7s %s\n", c.Name, c.When, c.Blocks, how)
				}
				for _, w := range r.CheckWarnings {
					a.printf("  Warning: %s\n", w)
				}
			}
			if len(res.Msg.Reservations) > 0 {
				a.printf("\nReservations:\n")
				for _, x := range res.Msg.Reservations {
					a.printf("  %-12s %-8s %-36s %s\n", x.Rule, x.Value, x.Effort, x.ReservedBy)
				}
			}
			a.emit(map[string]any{"rules": r, "reservations": res.Msg.Reservations})
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository (default: VX_REPO or the current working copy's)")
	cmd.Flags().StringVar(&effortFlag, "effort", "", "read the rules at this effort's head (default: the working copy's effort, else the default branch)")
	return cmd
}

func (a *App) reserveCmd() *cobra.Command {
	var repoFlag, effortFlag string
	cmd := &cobra.Command{
		Use:   "reserve RULE",
		Short: "Reserve the next free key of a vx.toml [[unique]] rule, e.g. a migration number",
		Long: `Reserve the next free value of a [[unique]] rule in vx.toml for this effort:
one more than the largest key used on the target branch, in any open
effort, or reserved before, formatted like the existing keys. Two agents
reserving at the same time get different values.

  vx reserve migration
  Reserved migration 017 for this effort: name it internal/store/migrations/017_<name>.sql

Reserve before you add the file, so parallel efforts never pick the same
number. Numbers follow the order of reservations, not the order changes
land or migrations run: an effort that reserved 017 can ship after one that
reserved 018, so a migration must not rely on a lower-numbered one from
another open effort. vx rules lists the rules and reservations.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var rest []string
			if effortFlag != "" {
				rest = []string{effortFlag}
			}
			repo, effort, err := a.resolveEffort(repoFlag, rest)
			if err != nil {
				return err
			}
			resp, err := c.Rules.Reserve(ctxOf(cmd), connect.NewRequest(&vxv1.ReserveRequest{Repo: repo, Effort: effort, Rule: args[0]}))
			if err != nil {
				return err
			}
			whose := "this effort"
			if wc, err := a.workingCopy(); err != nil || wc.Config.Repo != repo || wc.Config.Effort != effort {
				whose = "effort " + effort
			}
			a.printf("Reserved %s %s for %s: name it %s\n", args[0], resp.Msg.Value, whose, resp.Msg.Example)
			a.printf("Numbers follow reservation order, not run order: another open effort may ship a lower number after yours.\n")
			a.emit(map[string]string{"rule": args[0], "value": resp.Msg.Value, "example": resp.Msg.Example, "repo": repo, "effort": effort})
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository (default: VX_REPO or the current working copy's)")
	cmd.Flags().StringVar(&effortFlag, "effort", "", "effort slug (default: the current working copy's)")
	return cmd
}

func listOrNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
