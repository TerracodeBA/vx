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
			if len(resp.Msg.Efforts) == 0 {
				a.printf("No efforts in %s yet. Start one with: vx effort new \"NAME\" --repo %s\n", repo, repo)
			}
			for _, e := range resp.Msg.Efforts {
				a.printf("%-36s %-9s %d open  %s\n", e.Slug, e.State, e.OpenWorkspaces, e.Name)
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	list.Flags().StringVar(&listRepo, "repo", "", "repository")
	list.Flags().StringVar(&state, "state", "", "open, shipping, merged or abandoned")

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

	cmd.AddCommand(create, list, show)
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
