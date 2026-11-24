package cli

import (
	"errors"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

func (a *App) effortCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "effort", Short: "Start and inspect efforts"}

	var repoFlag, goal, from string
	create := &cobra.Command{
		Use:   "new NAME",
		Short: "Start an effort on the current head of a branch",
		Long: `Start an effort. The base branch is chosen in this order:
  1. --from BRANCH
  2. the branch checked out in the git repository you are in
  3. the repo's default branch`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, err := a.resolveRepo(repoFlag)
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			base := from
			if base == "" {
				if b := currentGitBranch(a.dir()); b != "" {
					// Only use it if the upstream has that branch.
					rr, err := c.Repos.GetRepo(ctx, connect.NewRequest(&vxv1.GetRepoRequest{Name: repo}))
					if err == nil && rr.Msg.Repo.Provider != "none" {
						base = b
					}
				}
			}
			resp, err := c.Efforts.CreateEffort(ctx, connect.NewRequest(&vxv1.CreateEffortRequest{
				Repo: repo, Name: args[0], Goal: goal, BaseBranch: base,
			}))
			if err != nil {
				return err
			}
			e := resp.Msg.Effort
			a.printf("Started effort %s on %s at %s\n\nNext:\n  vx clone %s %s\n", e.Slug, e.BaseBranch, short(e.Base), repo, e.Slug)
			a.emit(resp.Msg)
			return nil
		},
	}
	create.Flags().StringVar(&repoFlag, "repo", "", "repository (default: VX_REPO or the current working copy's)")
	create.Flags().StringVar(&goal, "goal", "", "what the effort should achieve")
	create.Flags().StringVar(&from, "from", "", "base branch")

	var listRepo, state string
	var all bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List efforts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, err := a.resolveRepo(listRepo)
			if err != nil {
				return err
			}
			resp, err := c.Efforts.ListEfforts(ctxOf(cmd), connect.NewRequest(&vxv1.ListEffortsRequest{Repo: repo, State: state}))
			if err != nil {
				return err
			}
			// Without --state or --all, finished (merged, abandoned) efforts
			// are hidden; say how many.
			shown, hidden := resp.Msg.Efforts, 0
			if state == "" && !all {
				shown = nil
				for _, e := range resp.Msg.Efforts {
					if e.State == "merged" || e.State == "abandoned" {
						hidden++
						continue
					}
					shown = append(shown, e)
				}
			}
			if len(shown) == 0 && hidden == 0 {
				a.printf("No efforts in %s yet. Start one with: vx effort new \"NAME\" --repo %s\n", repo, repo)
			}
			for _, e := range shown {
				a.printf("%-36s %-9s %d open  %s\n", e.Slug, e.State, e.OpenWorkspaces, e.Name)
			}
			if hidden > 0 {
				a.printf("%d finished effort(s) hidden (merged or abandoned): vx effort list --all\n", hidden)
			}
			a.emit(&vxv1.ListEffortsResponse{Efforts: shown})
			return nil
		},
	}
	list.Flags().StringVar(&listRepo, "repo", "", "repository")
	list.Flags().StringVar(&state, "state", "", "open, shipping, merged or abandoned")
	list.Flags().BoolVar(&all, "all", false, "also list finished (merged and abandoned) efforts")

	var closeRepo string
	var merged, abandon bool
	closeCmd := &cobra.Command{
		Use:   "close [SLUG] (--merged | --abandon)",
		Short: "Mark an effort merged or abandoned",
		Long: `Mark an effort finished. Its changes and board stay readable, but nobody
can land into it any more, and lists, context and overlap forecasts leave
it out.

vx marks efforts merged by itself once their changes are on the target
branch (after vx repo sync, a GitHub push or a merged pull request), so
--merged is for hosts vx does not hear from. --abandon is for work that
will not ship.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if merged == abandon {
				return errors.New("say how the effort ends: --merged or --abandon")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, slug, err := a.resolveEffort(closeRepo, args)
			if err != nil {
				return err
			}
			st := "merged"
			if abandon {
				st = "abandoned"
			}
			resp, err := c.Efforts.CloseEffort(ctxOf(cmd), connect.NewRequest(&vxv1.CloseEffortRequest{Repo: repo, Effort: slug, State: st}))
			if err != nil {
				return err
			}
			a.printf("Effort %s is %s. Its history and board stay readable: vx effort show %s\n", slug, resp.Msg.Effort.State, slug)
			a.emit(resp.Msg)
			return nil
		},
	}
	closeCmd.Flags().StringVar(&closeRepo, "repo", "", "repository")
	closeCmd.Flags().BoolVar(&merged, "merged", false, "its work was merged on the git host")
	closeCmd.Flags().BoolVar(&abandon, "abandon", false, "its work will not ship")

	var showRepo string
	show := &cobra.Command{
		Use:   "show [SLUG]",
		Short: "Show an effort and its open workspaces",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, slug, err := a.resolveEffort(showRepo, args)
			if err != nil {
				return err
			}
			resp, err := c.Efforts.GetEffort(ctxOf(cmd), connect.NewRequest(&vxv1.GetEffortRequest{Repo: repo, Effort: slug}))
			if err != nil {
				return err
			}
			e := resp.Msg.Effort
			a.printf("%s  (%s)\n  %s\n", e.Name, e.State, e.Goal)
			a.printf("  repo %s, base %s at %s, head %s, owner %s\n", e.Repo, e.BaseBranch, short(e.Base), short(e.Head), e.Owner)
			if r := e.Rebase; r != nil {
				where := "running"
				if r.StoppedAt != nil {
					where = "stopped on a conflict at change " + r.StoppedAt.ChangeId
				}
				a.printf("  rebase onto %s by %s in progress: %s (%d replayed, %d to go)\n", r.OntoBranch, r.StartedBy, where, r.Replayed, r.Pending)
			}
			if len(resp.Msg.OpenWorkspaces) > 0 {
				a.printf("\nOpen workspaces:\n")
				for _, w := range resp.Msg.OpenWorkspaces {
					a.printf("  %s  %-28s %s\n", w.ChangeId, w.Owner, w.Intent)
				}
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	show.Flags().StringVar(&showRepo, "repo", "", "repository")

	cmd.AddCommand(create, list, show, closeCmd, a.effortRebaseCmd())
	return cmd
}

// resolveEffort picks repo and effort from args, flags or the working copy.
func (a *App) resolveEffort(repoFlag string, args []string) (string, string, error) {
	repo, err := a.resolveRepo(repoFlag)
	if err != nil {
		return "", "", err
	}
	if len(args) > 0 {
		return repo, args[0], nil
	}
	if wc, err := a.workingCopy(); err == nil && wc.Config.Repo == repo {
		return repo, wc.Config.Effort, nil
	}
	return "", "", errors.New("which effort? pass its slug or run inside a working copy")
}
