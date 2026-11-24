package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/go-git/go-git/v5"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
)

func (a *App) loginCmd() *cobra.Command {
	var server, token string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save the server address and your token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if server == "" || token == "" {
				return errors.New("both --server and --token are needed")
			}
			c := client.New(server, token)
			resp, err := c.Auth.WhoAmI(ctxOf(cmd), connect.NewRequest(&vxv1.WhoAmIRequest{}))
			if err != nil {
				return err
			}
			path := a.settingsPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			data, _ := json.MarshalIndent(Settings{Server: server, Token: token}, "", "  ")
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				return err
			}
			a.printf("Logged in to %s as %s\n", server, resp.Msg.Principal.Ref)
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "server URL, e.g. http://localhost:7070")
	cmd.Flags().StringVar(&token, "token", "", "your vx token")
	return cmd
}

func (a *App) whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who the token belongs to",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Auth.WhoAmI(ctxOf(cmd), connect.NewRequest(&vxv1.WhoAmIRequest{}))
			if err != nil {
				return err
			}
			p := resp.Msg.Principal
			a.printf("%s", p.Ref)
			if p.OnBehalfOf != "" {
				a.printf(" (acting for %s)", p.OnBehalfOf)
			}
			a.printf("\n")
			a.emit(resp.Msg)
			return nil
		},
	}
}

func (a *App) agentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Manage agents that act for you"}
	var display, envFile string
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Create an agent that acts for you",
		Long: `Create an agent identity that acts for you, with its own token.

With --env-file, the token is written to a private file (mode 600) instead
of being printed, together with the server address and a separate config
path. An agent then only needs:
  source FILE && vx clone REPO EFFORT DIR`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			path := envFile
			if path != "" {
				if !filepath.IsAbs(path) {
					path = filepath.Join(a.dir(), path)
				}
				// Refuse before creating, so the agent's token is not lost.
				if _, err := os.Lstat(path); err == nil {
					return fmt.Errorf("%s already exists; choose another path or remove it", path)
				}
			}
			resp, err := c.Auth.CreateAgent(ctxOf(cmd), connect.NewRequest(&vxv1.CreateAgentRequest{Name: args[0], DisplayName: display}))
			if err != nil {
				return err
			}
			if path != "" {
				if err := writeAgentEnv(path, a.settings().Server, resp.Msg.Token); err != nil {
					return err
				}
				a.printf("Created %s, acting for %s.\nIts environment is in %s (mode 600). Use it with:\n  source %s\n",
					resp.Msg.Agent.Ref, resp.Msg.Agent.OnBehalfOf, path, path)
				a.emit(map[string]any{"agent": resp.Msg.Agent, "env_file": path})
				return nil
			}
			a.printf("Created %s, acting for %s.\n\nToken (shown once):\n  %s\n\nGive it to the agent as:\n  export VX_TOKEN=%s\n",
				resp.Msg.Agent.Ref, resp.Msg.Agent.OnBehalfOf, resp.Msg.Token, resp.Msg.Token)
			a.emit(resp.Msg)
			return nil
		},
	}
	add.Flags().StringVar(&display, "display-name", "", "name shown in logs and PRs")
	add.Flags().StringVar(&envFile, "env-file", "", "write the agent's environment (server, token) to this file instead of printing the token")
	cmd.AddCommand(add, a.agentListCmd())
	return cmd
}

func (a *App) repoCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "repo", Short: "Register and inspect repositories"}

	var gh, gitURL, branch, org string
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Register a repository, optionally connected to GitHub or a git remote",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			req := &vxv1.CreateRepoRequest{Name: args[0], Provider: "none", DefaultBranch: branch, Org: org}
			switch {
			case gh != "" && gitURL != "":
				return errors.New("use either --github or --git, not both")
			case gh != "":
				req.Provider, req.Remote = "github", gh
			case gitURL != "":
				req.Provider, req.Remote = "git", gitURL
			}
			resp, err := c.Repos.CreateRepo(ctxOf(cmd), connect.NewRequest(req))
			if err != nil {
				return err
			}
			r := resp.Msg.Repo
			a.printf("Created repo %s", r.Name)
			if r.Remote != "" {
				a.printf(" following %s %s", r.Provider, r.Remote)
			}
			if r.Org != "" {
				a.printf(" in organization %s", r.Org)
			}
			a.printf(" (default branch %s)\n", r.DefaultBranch)
			for _, b := range r.Branches {
				a.printf("  %s at %s %s\n", b.Name, short(b.Head), short(b.GitSha))
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	create.Flags().StringVar(&gh, "github", "", "GitHub repository as owner/name")
	create.Flags().StringVar(&gitURL, "git", "", "any git remote URL or local path")
	create.Flags().StringVar(&branch, "branch", "", "default branch (default: ask the provider)")
	create.Flags().StringVar(&org, "org", "", "owning organization (default: the one named like NAME's owner part if you own or admin it, else your personal organization)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List repositories",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Repos.ListRepos(ctxOf(cmd), connect.NewRequest(&vxv1.ListReposRequest{}))
			if err != nil {
				return err
			}
			if len(resp.Msg.Repos) == 0 {
				a.printf("No repositories you can read. Ask an organization admin for access, or create one: vx repo create NAME\n")
			}
			for _, r := range resp.Msg.Repos {
				org := r.Org
				if org == "" {
					org = "-"
				}
				a.printf("%-30s %-16s %-6s %-7s %s\n", r.Name, org, r.MyRole, r.Provider, r.Remote)
			}
			a.emit(resp.Msg)
			return nil
		},
	}

	var syncRepo, syncBranch string
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Import the latest commits of an upstream branch",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, err := a.resolveRepo(syncRepo)
			if err != nil {
				return err
			}
			resp, err := c.Repos.SyncBranch(ctxOf(cmd), connect.NewRequest(&vxv1.SyncBranchRequest{Repo: repo, Branch: syncBranch}))
			if err != nil {
				return err
			}
			b := resp.Msg.Branch
			a.printf("%s at %s (git %s), %d new commit(s) imported\n", b.Name, short(b.Head), short(b.GitSha), resp.Msg.Imported)
			a.emit(resp.Msg)
			return nil
		},
	}
	sync.Flags().StringVar(&syncRepo, "repo", "", "repository (default: the current working copy's)")
	sync.Flags().StringVar(&syncBranch, "branch", "", "branch (default: the repo's default branch)")

	cmd.AddCommand(create, list, sync)
	cmd.AddCommand(a.repoAccessCmds()...)
	return cmd
}

// resolveRepo picks the repo from a flag, VX_REPO or the working copy.
func (a *App) resolveRepo(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := a.Getenv("VX_REPO"); v != "" {
		return v, nil
	}
	if wc, err := a.workingCopy(); err == nil {
		return wc.Config.Repo, nil
	}
	return "", errors.New("which repo? pass --repo, set VX_REPO, or run inside a working copy")
}

// currentGitBranch returns the branch checked out in a git repository
// containing dir, so efforts can start from the branch you are on.
func currentGitBranch(dir string) string {
	repo, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return ""
	}
	head, err := repo.Head()
	if err != nil || !head.Name().IsBranch() {
		return ""
	}
	return strings.TrimPrefix(head.Name().String(), "refs/heads/")
}

// writeAgentEnv writes a shell environment file for an agent, readable only
// by the owner, with its own vx config path next to it.
func writeAgentEnv(path, server, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	config := strings.TrimSuffix(path, filepath.Ext(path)) + ".config.json"
	body := fmt.Sprintf("# vx agent environment. Keep this file private.\nexport VX_SERVER=%q\nexport VX_TOKEN=%q\nexport VX_CONFIG=%q\n", server, token, config)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; choose another path or remove it", path)
		}
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
