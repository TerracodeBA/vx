package main

// vx-server org: purge a deleted organization at once, and produce the
// deletion record of a purged one (#40).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/service"
)

func orgCmd(dbURL, blobURL, dataDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Purge deleted organizations and confirm deletions",
		Long: `Owners delete an organization with vx org delete (or on its page in the web
UI). It is hidden at once and purged by the server after the grace period
(VX_ORG_DELETE_GRACE, default 30d). These commands are for the operator:

  vx-server org purge acme --now        # purge a deleted organization today, for a customer who asks
  vx-server org deletion-record acme    # the confirmation to send the customer, also after the purge`,
	}
	var now bool
	purge := &cobra.Command{
		Use:   "purge ORG --now",
		Short: "Purge a deleted organization before its grace period ends",
		Long: `Delete every repository, effort, change, note, stored object and git mirror
of a deleted organization now, instead of when its grace period ends. The
organization must be deleted already (by an owner: vx org delete ORG). It
cannot be restored afterwards. Safe to run while servers run: only one
purges at a time, and a purge that stops continues where it was.

Git mirrors on other servers' disks are removed by those servers within
10 minutes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !now {
				return fmt.Errorf("purging cannot be undone: add --now to purge %s today", args[0])
			}
			ctx := cmd.Context()
			st, err := openMigrated(ctx, *dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			blobs, err := openBlobs(*blobURL, *dataDir, false)
			if err != nil {
				return err
			}
			bridge := gitbridge.New(st, blobs, filepath.Join(*dataDir, "git"), nil)
			svc := service.New(st, blobs, bridge)
			d, err := svc.ConsolePurgeOrg(ctx, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Purged organization %s (%s).\n\n%s", d.Org, d.OrgID, d.Confirmation(""))
			return nil
		},
	}
	purge.Flags().BoolVar(&now, "now", false, "purge now (required: it cannot be undone)")

	var asJSON bool
	record := &cobra.Command{
		Use:   "deletion-record ORG",
		Short: "Print the deletion confirmation of an organization, also after its purge",
		Long: `Print the record of an organization's deletion: its id and handle, who
deleted it, when, when it was purged and how many of each kind of record
the purge removed. It names no repository and holds no content. A handle
can be deleted more than once (it is free again after a purge); every
record is printed, newest first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := openMigrated(ctx, *dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			recs, err := service.New(st, nil, nil).ConsoleOrgDeletionRecords(ctx, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(recs)
			}
			for i, d := range recs {
				if i > 0 {
					fmt.Fprintln(out, "\n----")
				}
				fmt.Fprint(out, d.Confirmation(os.Getenv("VX_PUBLIC_URL")))
			}
			return nil
		},
	}
	record.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(purge, record)
	return cmd
}
