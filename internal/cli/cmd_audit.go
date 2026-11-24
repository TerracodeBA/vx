package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/version"
)

// auditExportPath is web.AuditExportPath.
const auditExportPath = "/ui/admin/audit/export"

// adminCmd groups commands for people who run a server or an
// organization. Add further admin subcommands here.
func (a *App) adminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Server and organization administration: the audit log"}
	cmd.AddCommand(a.auditCmd())
	return cmd
}

func (a *App) auditCmd() *cobra.Command {
	var org, repo, actor, since, until, format, out string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Export the audit log (operation log) as JSONL or CSV",
		Long: `Export the operation log: who did what, when, in which organization and
repository. Owners and admins of an organization export their organization
(--org); superadmins export the whole server, with the metadata of
repository operations but never their content.

Times are dates (2026-11-01), RFC 3339 times or ages before now (7d, 12h).
--until with a date includes that day.

  vx admin audit --org acme --since 30d > acme-audit.jsonl
  vx admin audit --org acme --actor agent:alice/claude --format csv -o audit.csv
  vx admin audit --since 2026-11-01 --until 2026-11-30      # superadmins`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := a.settings()
			if s.Server == "" || s.Token == "" {
				return errors.New("not logged in: run `vx login --server URL` (it opens your browser), or set VX_SERVER and VX_TOKEN")
			}
			if a.jsonOut {
				format = "jsonl" // JSON already: one object per line
			}
			if format != "jsonl" && format != "csv" {
				return fmt.Errorf("--format %q: use jsonl or csv", format)
			}
			q := url.Values{"format": {format}}
			for k, v := range map[string]string{"org": org, "repo": repo, "actor": actor, "since": since, "until": until} {
				if v != "" {
					q.Set(k, v)
				}
			}
			req, err := http.NewRequestWithContext(ctxOf(cmd), http.MethodGet, strings.TrimRight(s.Server, "/")+auditExportPath+"?"+q.Encode(), nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+s.Token)
			req.Header.Set(version.Header, version.Version)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("could not reach %s: %w", s.Server, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				text := strings.TrimSpace(string(msg))
				if resp.StatusCode == http.StatusNotFound && !strings.Contains(text, "not found") {
					text = "this server has no audit export yet (it needs vx-server 0.5 or later)"
				}
				return fmt.Errorf("audit export refused (%s): %s", resp.Status, text)
			}
			w := a.Out
			if out != "" {
				path := out
				if !filepath.IsAbs(path) {
					path = filepath.Join(a.dir(), path)
				}
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			n, err := io.Copy(w, resp.Body)
			if err != nil {
				return fmt.Errorf("audit export cut short after %d bytes: %w", n, err)
			}
			if out != "" {
				fmt.Fprintf(a.Err, "Wrote the audit log to %s (%d bytes).\n", out, n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "organization (required unless you are a superadmin)")
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository (OWNER/NAME)")
	cmd.Flags().StringVar(&actor, "actor", "", "only what this person or agent did (alice, agent:alice/claude)")
	cmd.Flags().StringVar(&since, "since", "", "from this time on (2026-11-01, 7d)")
	cmd.Flags().StringVar(&until, "until", "", "before this time (2026-11-30, 1d)")
	cmd.Flags().StringVar(&format, "format", "jsonl", "jsonl (one JSON object per line) or csv")
	cmd.Flags().StringVarP(&out, "output", "o", "", "write to this file (mode 600) instead of stdout")
	return cmd
}
