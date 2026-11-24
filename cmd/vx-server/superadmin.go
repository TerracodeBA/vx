package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/service"
)

// superadminCmd manages superadmins from the server console: the people
// who run the server's admin panel (people, invites, the GitHub App).
func superadminCmd(dbURL *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "superadmin",
		Short: "Grant, revoke and list superadmins (who run this server's admin panel)",
		Long: `Superadmins run the server: Admin > People, invites and sign-up, the GitHub
App, merging duplicate accounts. Being one gives no role on any repository.

  vx-server superadmin grant alice
  vx-server superadmin revoke alice
  vx-server superadmin list

Each change is recorded in the operation log (person.superadmin_grant,
person.superadmin_revoke) without an actor: it came from the console.`,
	}
	set := func(on bool) *cobra.Command {
		use, short := "grant HANDLE", "Make a person a superadmin"
		if !on {
			use, short = "revoke HANDLE", "Take superadmin away from a person"
		}
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				st, err := openMigrated(cmd.Context(), *dbURL)
				if err != nil {
					return err
				}
				defer st.Close()
				svc := service.New(st, nil, nil)
				p, changed, err := svc.ConsoleSetSuperadmin(cmd.Context(), args[0], on)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				switch {
				case on && changed:
					fmt.Fprintf(out, "%s is now a superadmin.\n", p.Ref())
				case on:
					fmt.Fprintf(out, "%s already is a superadmin; nothing changed.\n", p.Ref())
				case changed:
					fmt.Fprintf(out, "%s is no longer a superadmin.\n", p.Ref())
				default:
					fmt.Fprintf(out, "%s is not a superadmin; nothing changed.\n", p.Ref())
				}
				if !on {
					if left, err := svc.ConsoleSuperadmins(cmd.Context()); err == nil && len(left) == 0 {
						fmt.Fprintln(out, "Nobody is a superadmin now: grant one with vx-server superadmin grant HANDLE.")
					}
				}
				return nil
			},
		}
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the superadmins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openMigrated(cmd.Context(), *dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			people, err := service.New(st, nil, nil).ConsoleSuperadmins(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(people) == 0 {
				fmt.Fprintln(out, "No superadmins. Grant one with vx-server superadmin grant HANDLE.")
				return nil
			}
			for _, p := range people {
				fmt.Fprintf(out, "%s\t%s\t%s\n", p.Ref(), p.DisplayName, p.Email)
			}
			return nil
		},
	}
	cmd.AddCommand(set(true), set(false), list)
	return cmd
}
