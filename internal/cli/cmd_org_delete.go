package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// Deleting and restoring organizations (#40).

func shortDate(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2 Jan 2006 15:04 UTC")
}

func (a *App) orgDeleteCmds() []*cobra.Command {
	var confirm string
	del := &cobra.Command{
		Use:   "delete ORG --confirm ORG",
		Short: "Delete an organization and all its data (owners; restorable until the purge)",
		Long: `Delete an organization: its repositories, efforts, changes, board notes,
claims, tasks, shipments, grants, teams and memberships. People keep their
accounts.

It is hidden at once: its repositories are "not found" for everyone,
shipping and syncing stop, and its handle stays reserved. An owner can
bring it back unchanged with vx org restore until the grace period ends
(30 days unless the server says otherwise); then the server purges it for
good. Only owners may, and personal organizations cannot be deleted.

A GitHub-backed organization stops syncing; the vx GitHub App stays
installed on GitHub until you uninstall it there, which is your choice.

  vx org delete acme --confirm acme
  vx org deletion acme -o acme-deletion.txt    # the deletion confirmation`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if confirm == "" {
				return fmt.Errorf("deleting organization %s removes all its repositories and their history after the grace period: run vx org delete %s --confirm %s", args[0], args[0], args[0])
			}
			resp, err := c.Orgs.DeleteOrg(ctxOf(cmd), connect.NewRequest(&vxv1.DeleteOrgRequest{Handle: args[0], Confirm: confirm}))
			if err != nil {
				return err
			}
			d := resp.Msg.Deletion
			a.printf("Deleted organization %s. It is hidden now and will be purged on %s.\n", d.Org, shortDate(d.PurgeAfter))
			a.printf("Until then an owner can restore it unchanged: vx org restore %s\n", d.Org)
			a.printf("Keep the confirmation: vx org deletion %s -o %s-deletion.txt\n", d.Org, d.Org)
			a.emit(resp.Msg)
			return nil
		},
	}
	del.Flags().StringVar(&confirm, "confirm", "", "the organization's handle again, to confirm")

	restore := &cobra.Command{
		Use:   "restore ORG",
		Short: "Bring back a deleted organization before its purge (owners)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.RestoreOrg(ctxOf(cmd), connect.NewRequest(&vxv1.RestoreOrgRequest{Handle: args[0]}))
			if err != nil {
				return err
			}
			a.printf("Restored organization %s with its repositories, members and settings as they were.\n", resp.Msg.Org.Handle)
			a.emit(resp.Msg)
			return nil
		},
	}

	var out string
	record := &cobra.Command{
		Use:   "deletion ORG",
		Short: "Print the deletion confirmation of a deleted organization (owners, until the purge)",
		Long: `Print the confirmation of an organization's deletion: who deleted it, when,
and when it will be purged. It names no repository. Save it with -o.
After the purge the server's operator produces the final confirmation with
the counts of what was removed (vx-server org deletion-record ORG).

  vx org deletion acme -o acme-deletion.txt
  vx org deletion --list     # every deleted organization you own`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				resp, err := c.Orgs.ListDeletedOrgs(ctxOf(cmd), connect.NewRequest(&vxv1.ListDeletedOrgsRequest{}))
				if err != nil {
					return err
				}
				if len(resp.Msg.Deletions) == 0 {
					a.printf("You own no deleted organizations waiting for their purge.\n")
				}
				for _, d := range resp.Msg.Deletions {
					state := "purge on " + shortDate(d.PurgeAfter)
					if d.PurgeStarted != "" {
						state = "being purged"
					}
					a.printf("%-24s deleted %s by %s, %s\n", d.Org, shortDate(d.DeletedAt), d.RequestedBy, state)
				}
				a.emit(resp.Msg)
				return nil
			}
			resp, err := c.Orgs.GetOrgDeletion(ctxOf(cmd), connect.NewRequest(&vxv1.GetOrgDeletionRequest{Handle: args[0]}))
			if err != nil {
				return err
			}
			text := resp.Msg.Deletion.Confirmation
			if out != "" {
				path := out
				if !filepath.IsAbs(path) {
					path = filepath.Join(a.dir(), path)
				}
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					return err
				}
				fmt.Fprintf(a.Err, "Wrote the deletion confirmation of %s to %s.\n", resp.Msg.Deletion.Org, out)
			} else {
				a.printf("%s\n", strings.TrimRight(text, "\n"))
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	record.Flags().StringVarP(&out, "output", "o", "", "write the confirmation to this file")
	return []*cobra.Command{del, restore, record}
}
