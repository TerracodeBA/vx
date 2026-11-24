package cli

import (
	"fmt"
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
			if len(resp.Msg.Agents) == 0 {
				a.printf("No agents yet. Create one with: vx agent add NAME\n")
			} else {
				a.printf("%-32s %-20s %s\n", "AGENT", "TOKENS", "LAST USED")
			}
			for _, ag := range resp.Msg.Agents {
				tokens := plural(int(ag.ActiveTokens), "active")
				if ag.RevokedTokens > 0 {
					tokens += ", " + plural(int(ag.RevokedTokens), "revoked")
				}
				a.printf("%-32s %-20s %s\n", ag.Agent.Handle, tokens, when(ag.LastUsedAt, "never"))
			}
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
	cmd := &cobra.Command{Use: "token", Short: "See and revoke your tokens and your agents' tokens"}

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
			if len(resp.Msg.Tokens) == 0 {
				a.printf("  none\n")
			} else {
				a.printf("%-10s %-24s %-17s %-17s %s\n", "ID", "NAME", "CREATED", "LAST USED", "STATE")
			}
			for _, t := range resp.Msg.Tokens {
				state := "active"
				if t.RevokedAt != "" {
					state = "revoked " + when(t.RevokedAt, "")
				}
				if t.Current {
					state += " (in use now)"
				}
				a.printf("%-10s %-24s %-17s %-17s %s\n", t.Id[:shortID], t.Name, when(t.CreatedAt, ""), when(t.LastUsedAt, "never"), state)
			}
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

	cmd.AddCommand(list, revoke)
	return cmd
}
