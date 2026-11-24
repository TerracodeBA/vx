package cli

import (
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// supportCmd is vx repo support: support access for superadmins (#33).
func (a *App) supportCmd() *cobra.Command {
	var role, reason string
	var dur time.Duration
	var revoke bool
	cmd := &cobra.Command{
		Use:   "support REPO [PERSON] [--role read|write] [--for 24h] [--reason TEXT] [--revoke]",
		Short: "Give a superadmin time-limited support access to a repository, end it, or list it (repository admins)",
		Long: `Superadmins run the vx server but have no access to repositories. When you
want one of them to help with a repository, give them support access: read
(the default) or write, for a limited time (--for, default 24h, at most
720h). It is recorded in the operation log and shows on the access page as
"operator (support access) until DATE". It ends by itself; end it earlier
with --revoke. The superadmin can also end their own.

With only REPO, lists the support access in effect.`,
		Example: `  vx repo support acme/shop root --role read --for 72h --reason "ticket 4711: land fails"
  vx repo support acme/shop root --revoke
  vx repo support acme/shop`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo := args[0]
			if len(args) == 1 {
				if revoke {
					return fmt.Errorf("whose support access? vx repo support %s PERSON --revoke", repo)
				}
				resp, err := c.Orgs.ListSupport(ctxOf(cmd), connect.NewRequest(&vxv1.ListSupportRequest{Repo: repo}))
				if err != nil {
					return err
				}
				if len(resp.Msg.Grants) == 0 {
					a.printf("No support access on %s. Superadmins have no access to it.\n", repo)
				} else {
					a.printf("Support access on %s (%d):\n", repo, len(resp.Msg.Grants))
					for _, g := range resp.Msg.Grants {
						line := fmt.Sprintf("  %-30s %-5s until %s", g.Person.Ref, g.Role, untilText(g.ExpiresAt))
						if g.GrantedBy != "" {
							line += ", granted by " + g.GrantedBy
						}
						if g.Reason != "" {
							line += ": " + g.Reason
						}
						a.printf("%s\n", line)
					}
				}
				a.emit(resp.Msg)
				return nil
			}
			if revoke {
				resp, err := c.Orgs.RevokeSupport(ctxOf(cmd), connect.NewRequest(&vxv1.RevokeSupportRequest{Repo: repo, Person: args[1]}))
				if err != nil {
					return err
				}
				a.printf("Ended the support access of %s to %s.\n", args[1], repo)
				a.emit(resp.Msg)
				return nil
			}
			resp, err := c.Orgs.GrantSupport(ctxOf(cmd), connect.NewRequest(&vxv1.GrantSupportRequest{
				Repo: repo, Person: args[1], Role: role, DurationSeconds: int64(dur / time.Second), Reason: reason,
			}))
			if err != nil {
				return err
			}
			g := resp.Msg.Grant
			a.printf("Gave %s %s support access to %s until %s. End it earlier with: vx repo support %s %s --revoke\n",
				g.Person.Ref, g.Role, g.Repo, untilText(g.ExpiresAt), g.Repo, g.Person.Handle)
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "read", "read or write (support access never includes admin)")
	cmd.Flags().DurationVar(&dur, "for", 24*time.Hour, "how long it lasts, at most 720h")
	cmd.Flags().StringVar(&reason, "reason", "", "why, shown on the access page and in the operation log")
	cmd.Flags().BoolVar(&revoke, "revoke", false, "end PERSON's support access now")
	return cmd
}

// untilText shows an RFC 3339 time as "2026-12-01 14:00 UTC".
func untilText(rfc string) string {
	t, err := time.Parse(time.RFC3339Nano, rfc)
	if err != nil {
		return rfc
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
