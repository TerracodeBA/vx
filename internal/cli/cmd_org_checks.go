package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// vx org checks ORG [--file checks.toml | --clear] [--history]

func (a *App) orgChecksCmd() *cobra.Command {
	var file string
	var clear, history bool
	cmd := &cobra.Command{
		Use:   "checks ORG",
		Short: "Show or set the checks every repository of an organization must pass",
		Long: `Organization checks are [[check]] tables, written as in vx.toml, that apply
to every repository of the organization. A repository's vx.toml can add
checks of its own but cannot remove or weaken these: a vx.toml check with
the name of an organization check is ignored, with a warning.

  vx org checks acme                         the checks in force
  vx org checks acme --file checks.toml      save a new version (owners and admins)
  vx org checks acme --file -                read the new version from stdin
  vx org checks acme --clear                 remove every organization check
  vx org checks acme --history               every saved version, newest first

Only people who are owners or admins of the organization change them, never
agents. Every version is kept and every change is in the operation log
(org.checks). The organization page shows them too.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			org := args[0]
			if file != "" || clear {
				if file != "" && clear {
					return fmt.Errorf("give --file or --clear, not both")
				}
				source := ""
				if file != "" {
					var data []byte
					if file == "-" {
						data, err = io.ReadAll(cmd.InOrStdin())
					} else {
						data, err = os.ReadFile(file)
					}
					if err != nil {
						return err
					}
					source = string(data)
					if strings.TrimSpace(source) == "" {
						return fmt.Errorf("%s is empty; to remove every organization check use --clear", file)
					}
				}
				resp, err := c.Orgs.SetOrgChecks(ctx, connect.NewRequest(&vxv1.SetOrgChecksRequest{Org: org, Source: source}))
				if err != nil {
					return err
				}
				v := resp.Msg.Checks
				switch {
				case !resp.Msg.Changed:
					a.printf("Organization %s: these checks are already in force (version %d); nothing changed.\n", v.Org, v.Version)
				case len(v.Checks) == 0:
					a.printf("Organization %s: removed every organization check (version %d).\n", v.Org, v.Version)
				default:
					a.printf("Organization %s: saved version %d with %d check(s): %s.\n", v.Org, v.Version, len(v.Checks), strings.Join(v.Checks, ", "))
					a.printf("They apply to every repository of %s from its next land; vx check status shows them as \"organization %s\".\n", v.Org, v.Org)
				}
				a.emit(resp.Msg)
				return nil
			}
			resp, err := c.Orgs.GetOrgChecks(ctx, connect.NewRequest(&vxv1.GetOrgChecksRequest{Org: org, History: history}))
			if err != nil {
				return err
			}
			cur := resp.Msg.Current
			if history {
				if len(resp.Msg.History) == 0 {
					a.printf("Organization %s never defined checks.\n", cur.Org)
				}
				for _, v := range resp.Msg.History {
					names := strings.Join(v.Checks, ", ")
					if names == "" {
						names = "(none)"
					}
					a.printf("version %-3d %s  %-24s %s\n", v.Version, v.At, v.By, names)
				}
				a.emit(resp.Msg)
				return nil
			}
			if cur.Version == 0 {
				a.printf("Organization %s has no checks of its own; each repository's vx.toml decides. Set them with: vx org checks %s --file checks.toml\n", cur.Org, cur.Org)
				a.emit(resp.Msg)
				return nil
			}
			a.printf("Organization %s, version %d, saved by %s at %s:\n\n", cur.Org, cur.Version, cur.By, cur.At)
			if strings.TrimSpace(cur.Source) == "" {
				a.printf("(no checks)\n")
			} else {
				a.printf("%s", cur.Source)
				if !strings.HasSuffix(cur.Source, "\n") {
					a.printf("\n")
				}
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "save the [[check]] tables in this file as a new version (- for stdin)")
	cmd.Flags().BoolVar(&clear, "clear", false, "save an empty version: no organization checks")
	cmd.Flags().BoolVar(&history, "history", false, "list every saved version, newest first")
	return cmd
}
