package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/blob"
)

// openBlobs opens the server's blob store: VX_BLOB_URL (or --blob-url),
// else the disk store in the data directory.
func openBlobs(blobURL, dataDir string, noCache bool) (blob.Store, error) {
	return blob.Open(blobURL, blob.OpenOptions{DiskDir: filepath.Join(dataDir, "objects"), NoCache: noCache})
}

// describeStore names a store for messages; URLs never hold credentials.
func describeStore(url, dataDir string) string {
	if url == "" {
		return "disk store " + filepath.Join(dataDir, "objects")
	}
	return url
}

func blobsCmd(blobURL, dataDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "blobs",
		Short: "Manage the blob store (file contents and trees)",
	}
	var to, from string
	var jobs int
	var verifyExisting bool
	copyCmd := &cobra.Command{
		Use:   "copy --to URL",
		Short: "Copy every object into another store, to move to object storage",
		Long: `Copy every object of the current blob store (VX_BLOB_URL, or the disk store
in the data directory) into another store, then switch VX_BLOB_URL to it.

Each object is read (and checked against its hash), written and read back
from the destination. Objects the destination already has are skipped, so
an interrupted copy resumes where it stopped: run it again. Safe while the
server runs; copy once live, stop the server, copy again (only the objects
written since), set VX_BLOB_URL and start it.

Destination credentials: VX_BLOB_TO_ACCESS_KEY_ID and
VX_BLOB_TO_SECRET_ACCESS_KEY, else VX_BLOB_*, else AWS_* (each also as
..._FILE, a file holding the value).

  vx-server blobs copy --to 's3://vx-objects/prod?region=auto&endpoint=https://ACCOUNT.r2.cloudflarestorage.com'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if to == "" {
				return fmt.Errorf("--to is required: the URL of the destination store")
			}
			srcURL := *blobURL
			if cmd.Flags().Changed("from") {
				srcURL = from
			}
			if strings.TrimSpace(srcURL) == strings.TrimSpace(to) {
				return fmt.Errorf("the source and the destination are the same store (%s)", to)
			}
			src, err := openBlobs(srcURL, *dataDir, true)
			if err != nil {
				return fmt.Errorf("source: %w", err)
			}
			dst, err := blob.Open(to, blob.OpenOptions{NoCache: true, Getenv: func(k string) string {
				if strings.HasPrefix(k, "VX_BLOB_") {
					if v := os.Getenv("VX_BLOB_TO_" + strings.TrimPrefix(k, "VX_BLOB_")); v != "" {
						return v
					}
				}
				return os.Getenv(k)
			}})
			if err != nil {
				return fmt.Errorf("destination: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Copying objects from %s to %s\n", describeStore(srcURL, *dataDir), to)
			start := time.Now()
			st, err := blob.Copy(cmd.Context(), src, dst, blob.CopyOptions{Jobs: jobs, VerifyExisting: verifyExisting,
				Progress: func(s blob.CopyStats) {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %d objects: %d copied, %d already there\n", s.Seen, s.Copied, s.Skipped+s.Verified)
				}})
			if err != nil {
				return fmt.Errorf("copy stopped after %d objects (%d copied): %w\nRun the same command again to resume", st.Copied+st.Skipped+st.Verified, st.Copied, err)
			}
			fmt.Fprintf(out, "Done in %s: %d objects, %d copied (%s), %d already there", time.Since(start).Round(time.Second),
				st.Seen, st.Copied, humanBytes(st.Bytes), st.Skipped+st.Verified)
			if verifyExisting {
				fmt.Fprintf(out, " and checked")
			}
			fmt.Fprintln(out, ".")
			fmt.Fprintf(out, "Every object was read back from the destination. To switch, stop the server, run this again, then set VX_BLOB_URL=%s\n", to)
			return nil
		},
	}
	copyCmd.Flags().StringVar(&to, "to", "", "destination store URL (s3://, gs:// or file://)")
	copyCmd.Flags().StringVar(&from, "from", "", "source store URL (default: VX_BLOB_URL, else the data directory)")
	copyCmd.Flags().IntVar(&jobs, "jobs", 8, "objects copied at once")
	copyCmd.Flags().BoolVar(&verifyExisting, "verify-existing", false, "read back objects the destination already has instead of skipping them")
	cmd.AddCommand(copyCmd)
	return cmd
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
