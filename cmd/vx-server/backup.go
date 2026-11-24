package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/blob"
)

// backupName is what a backup file may be called: no directories.
var backupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const checksumSuffix = ".sha256"

func backupCmd() *cobra.Command {
	var bucketURL string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Copy backup files to and from an off-site bucket (VX_BACKUP_URL)",
		Long: `Move backup files between this machine and an S3-compatible bucket (AWS S3,
Cloudflare R2, MinIO, Google Cloud Storage with HMAC keys). deploy/production
backup.sh and restore.sh use these; you rarely need them by hand.

The bucket is VX_BACKUP_URL (or --url), for example
  s3://vx-backups/app.example.com?region=auto&endpoint=https://ACCOUNT.r2.cloudflarestorage.com
with credentials in VX_BACKUP_ACCESS_KEY_ID and VX_BACKUP_SECRET_ACCESS_KEY
(each also as ..._FILE, a file holding the value; AWS_* is the fallback).

Every file is stored with a .sha256 next to it; get checks it.`,
	}
	cmd.PersistentFlags().StringVar(&bucketURL, "url", "", "bucket URL (default: VX_BACKUP_URL)")
	client := func() (*blob.S3Client, error) {
		u := bucketURL
		if u == "" {
			u = os.Getenv("VX_BACKUP_URL")
		}
		if u == "" {
			return nil, errors.New("no backup bucket: set VX_BACKUP_URL (s3://BUCKET/PREFIX?region=...&endpoint=...)")
		}
		cfg, _, err := blob.ParseBucketURL(u)
		if err != nil {
			return nil, err
		}
		if cfg.Credentials, err = blob.CredentialsFromEnv("VX_BACKUP", nil); err != nil {
			return nil, err
		}
		return blob.NewS3Client(cfg)
	}
	checkName := func(name string) error {
		if !backupName.MatchString(name) || strings.HasSuffix(name, checksumSuffix) {
			return fmt.Errorf("backup name %q: use letters, digits, '.', '-' and '_' only", name)
		}
		return nil
	}

	put := &cobra.Command{
		Use:   "put NAME",
		Short: "Upload standard input as NAME (with NAME.sha256)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := checkName(name); err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			sum := sha256.New()
			n, err := c.PutStream(cmd.Context(), name, io.TeeReader(bufio.NewReaderSize(cmd.InOrStdin(), 1<<20), sum), 0, "application/octet-stream")
			if err != nil {
				return err
			}
			digest := hex.EncodeToString(sum.Sum(nil))
			if err := c.Put(cmd.Context(), name+checksumSuffix, []byte(digest+"  "+name+"\n"), "text/plain"); err != nil {
				return err
			}
			size, ok, err := c.Head(cmd.Context(), name)
			if err != nil {
				return err
			}
			if !ok || size != n {
				return fmt.Errorf("uploaded %d bytes but the bucket holds %d for %s", n, size, name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Uploaded %s (%s, sha256 %s) to %s\n", name, humanBytes(n), digest[:12], c)
			return nil
		},
	}

	get := &cobra.Command{
		Use:   "get NAME",
		Short: "Download NAME to standard output, checking its .sha256",
		Long: `Download NAME to standard output. When the download does not match the
.sha256 stored with it, the command fails (exit 1) after writing: write to a
file and keep it only when the command succeeds.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := checkName(name); err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			var want string
			if b, err := c.GetBytes(cmd.Context(), name+checksumSuffix); err == nil {
				want, _, _ = strings.Cut(strings.TrimSpace(string(b)), " ")
			} else if !blob.IsNotFound(err) {
				return err
			}
			rc, _, err := c.Open(cmd.Context(), name)
			if blob.IsNotFound(err) {
				return fmt.Errorf("no backup %s in %s: vx-server backup list shows what there is", name, c)
			}
			if err != nil {
				return err
			}
			defer rc.Close()
			sum := sha256.New()
			w := bufio.NewWriterSize(cmd.OutOrStdout(), 1<<20)
			if _, err := io.Copy(io.MultiWriter(w, sum), rc); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return err
			}
			got := hex.EncodeToString(sum.Sum(nil))
			if want == "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s has no %s to check it against\n", name, checksumSuffix)
			} else if got != want {
				return fmt.Errorf("%s is damaged: its sha256 is %s, the backup recorded %s", name, got, want)
			}
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List the backups in the bucket, oldest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			entries, err := listBackups(cmd, c)
			if err != nil {
				return err
			}
			for _, e := range entries {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%d\t%s\n", e.Key, e.Size, e.LastModified.UTC().Format(time.RFC3339))
			}
			return nil
		},
	}

	var keepDays int
	prune := &cobra.Command{
		Use:   "prune --keep-days N",
		Short: "Delete backups older than N days from the bucket",
		Long: `Delete backups (and their .sha256) uploaded more than N days ago. The newest
backup of each kind (vx-db-*, vx-data-*) is always kept. A bucket lifecycle
rule does the same without delete permission for this key, which is safer:
then skip prune.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if keepDays < 1 {
				return errors.New("--keep-days must be at least 1")
			}
			c, err := client()
			if err != nil {
				return err
			}
			entries, err := listBackups(cmd, c)
			if err != nil {
				return err
			}
			newest := map[string]string{}
			for _, e := range entries {
				newest[backupKind(e.Key)] = e.Key
			}
			cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour)
			deleted := 0
			for _, e := range entries {
				if !e.LastModified.Before(cutoff) || newest[backupKind(e.Key)] == e.Key {
					continue
				}
				if err := c.Delete(cmd.Context(), e.Key); err != nil {
					return err
				}
				if err := c.Delete(cmd.Context(), e.Key+checksumSuffix); err != nil {
					return err
				}
				deleted++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %d backups older than %d days from %s.\n", deleted, keepDays, c)
			return nil
		},
	}
	prune.Flags().IntVar(&keepDays, "keep-days", 0, "keep backups uploaded within this many days")
	createBucket := &cobra.Command{
		Use:   "create-bucket",
		Short: "Create the backup bucket (MinIO; elsewhere create it in the provider's console)",
		Long: `Create the bucket VX_BACKUP_URL names. Meant for MinIO you run yourself. On AWS
S3, R2 or Google Cloud Storage create the bucket in the console instead, where
you can also turn on versioning or object lock and a lifecycle rule.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.CreateBucket(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Bucket ready: %s\n", c)
			return nil
		},
	}
	cmd.AddCommand(put, get, list, prune, createBucket)
	return cmd
}

// listBackups lists backup files (not their checksums), oldest name first;
// names carry their date, so that is also oldest first.
func listBackups(cmd *cobra.Command, c *blob.S3Client) ([]blob.ListEntry, error) {
	var out []blob.ListEntry
	err := c.List(cmd.Context(), "", func(e blob.ListEntry) error {
		if !strings.Contains(e.Key, "/") && !strings.HasSuffix(e.Key, checksumSuffix) {
			out = append(out, e)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

// backupKind groups backups by what they hold: "vx-db-2026-11-24-0315.dump"
// is "vx-db".
func backupKind(name string) string {
	parts := strings.SplitN(name, "-", 3)
	if len(parts) < 3 {
		return name
	}
	return parts[0] + "-" + parts[1]
}
