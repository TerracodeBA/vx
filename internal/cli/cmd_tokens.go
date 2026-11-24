package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// shortID is how token ids are shown in text; `vx token revoke` accepts it.
const shortID = 8

func (a *App) agentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your agents with their tokens and when they were last used",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Auth.ListAgents(ctxOf(cmd), connect.NewRequest(&vxv1.ListAgentsRequest{}))
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
			if len(resp.Msg.Agents) == 0 {
				a.printf("No agents yet. Create one with: vx agent add NAME\n")
			} else if !a.jsonOut {
				fmt.Fprintln(tw, "AGENT\tTOKENS\tLAST USED")
			}
			for _, ag := range resp.Msg.Agents {
				tokens := plural(int(ag.ActiveTokens), "active")
				if ag.RevokedTokens > 0 {
					tokens += ", " + plural(int(ag.RevokedTokens), "revoked")
				}
				if !a.jsonOut {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", ag.Agent.Handle, tokens, when(ag.LastUsedAt, "never"))
				}
			}
			tw.Flush()
			a.emit(resp.Msg)
			return nil
		},
	}
}

func plural(n int, what string) string { return fmt.Sprintf("%d %s", n, what) }

// when shows an RFC 3339 time in local time to the minute, or empty.
func when(rfc3339, empty string) string {
	if rfc3339 == "" {
		return empty
	}
	t, err := time.Parse(time.RFC3339Nano, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (a *App) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Create, see and revoke your tokens and your agents' tokens"}

	var agent string
	list := &cobra.Command{
		Use:   "list",
		Short: "List your tokens, or one agent's with --agent",
		Long: `List tokens: when they were created, last used and revoked. Secrets are
never shown again after a token is created.

Without --agent, lists the tokens of whoever runs it (you, or the agent
whose token is in use). Use an id from here with: vx token revoke ID`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Auth.ListTokens(ctxOf(cmd), connect.NewRequest(&vxv1.ListTokensRequest{Agent: agent}))
			if err != nil {
				return err
			}
			a.printf("Tokens of %s\n", resp.Msg.Principal.Ref)
			tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
			if len(resp.Msg.Tokens) == 0 {
				a.printf("  none\n")
			} else if !a.jsonOut {
				fmt.Fprintln(tw, "ID\tNAME\tCREATED\tLAST USED\tSTATE")
			}
			for _, t := range resp.Msg.Tokens {
				state := "active"
				if t.RevokedAt != "" {
					state = "revoked " + when(t.RevokedAt, "")
				}
				if t.Current {
					state += " (in use now)"
				}
				if !a.jsonOut {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Id[:shortID], t.Name, when(t.CreatedAt, ""), when(t.LastUsedAt, "never"), state)
				}
			}
			tw.Flush()
			a.emit(resp.Msg)
			return nil
		},
	}
	list.Flags().StringVar(&agent, "agent", "", "one of your agents, by name (backend) or handle (alice/backend)")

	var current bool
	revoke := &cobra.Command{
		Use:   "revoke ID",
		Short: "Revoke one of your tokens or one of your agents' tokens",
		Long: `Revoke a token so it stops working at once. ID is the token id from
vx token list, or its first 8 or more characters.

Revoking the token this command itself uses would sign you out, so it
needs --current.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Auth.RevokeToken(ctxOf(cmd), connect.NewRequest(&vxv1.RevokeTokenRequest{Id: args[0], AllowCurrent: current}))
			if err != nil {
				return err
			}
			t := resp.Msg.Token
			switch {
			case resp.Msg.AlreadyRevoked:
				a.printf("Token %s (%s) of %s was already revoked on %s.\n", t.Id[:shortID], t.Name, t.Principal, when(t.RevokedAt, ""))
			case t.Current:
				a.printf("Revoked token %s (%s) of %s. It was the token in use here, so log in again with another one.\n", t.Id[:shortID], t.Name, t.Principal)
			default:
				a.printf("Revoked token %s (%s) of %s.\n", t.Id[:shortID], t.Name, t.Principal)
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	revoke.Flags().BoolVar(&current, "current", false, "allow revoking the token this command uses")

	var createAgent, createName, createEnv string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a new token for yourself, or for one of your agents with --agent",
		Long: `Create a new token for yourself, or for one of your agents with --agent.
No admin is needed. The secret is shown once; only its hash is stored.

With --env-file, the token is written to a private file (mode 600) instead
of being printed, as vx agent add --env-file does. Use it with:
  source FILE

Only people can create tokens. An agent cannot create tokens, not even for
itself: a leaked agent token must not be able to mint new ones that outlive
its revocation. Ask the person the agent acts for instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			path := createEnv
			if path != "" {
				if !filepath.IsAbs(path) {
					path = filepath.Join(a.dir(), path)
				}
				// Refuse before creating, so a token is not issued and lost.
				if _, err := os.Lstat(path); err == nil {
					return fmt.Errorf("%s already exists; choose another path or remove it", path)
				}
			}
			resp, err := c.Auth.CreateToken(ctxOf(cmd), connect.NewRequest(&vxv1.CreateTokenRequest{Agent: createAgent, Name: createName}))
			if err != nil {
				return err
			}
			t, owner := resp.Msg.Token, resp.Msg.Principal
			if path != "" {
				if err := writeAgentEnv(path, a.settings().Server, resp.Msg.Secret); err != nil {
					return fmt.Errorf("created token %s for %s but could not write it: %w. Revoke it with: vx token revoke %s", t.Id[:shortID], owner.Ref, err, t.Id[:shortID])
				}
				a.printf("Created token %s (%s) for %s.\nIts environment is in %s (mode 600). Use it with:\n  source %s\n",
					t.Id[:shortID], t.Name, owner.Ref, path, path)
				a.emit(map[string]any{"principal": owner, "token": t, "env_file": path})
				return nil
			}
			a.printf("Created token %s (%s) for %s.\n\nToken (shown once):\n  %s\n\nUse it as:\n  export VX_TOKEN=%s\n",
				t.Id[:shortID], t.Name, owner.Ref, resp.Msg.Secret, resp.Msg.Secret)
			a.emit(resp.Msg)
			return nil
		},
	}
	create.Flags().StringVar(&createAgent, "agent", "", "one of your agents, by name (backend) or handle (alice/backend); default: yourself")
	create.Flags().StringVar(&createName, "name", "", "a label for the token, such as laptop or ci")
	create.Flags().StringVar(&createEnv, "env-file", "", "write the environment (server, token) to this file instead of printing the token")

	cmd.AddCommand(list, create, revoke)
	return cmd
}
