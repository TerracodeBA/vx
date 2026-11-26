package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// vx org agreement: the data processing agreement (#41). The CLI shows the
// status and the text; accepting happens in the web UI.

func (a *App) orgAgreementCmd() *cobra.Command {
	var text, download bool
	var version, acceptance, output string
	cmd := &cobra.Command{
		Use:   "agreement ORG",
		Short: "Show an organization's data processing agreement: status, acceptances, text",
		Long: `On servers that ask for it (vx Cloud), an organization can be used for
work only after an owner accepted the data processing agreement in effect.
This shows whether it did, who accepted which version and when, and
versions announced to take effect later. --text prints a version's Markdown,
--download saves it to a file; --acceptance ID gets the text exactly as that
acceptance accepted it.

Accepting is not possible from the CLI or for agents: it is a deliberate
act by a person, so an owner reads the text and accepts it in the web UI
(Organization > Agreement).`,
		Example: `  vx org agreement acme
  vx org agreement acme --text
  vx org agreement acme --download --version 2027-01
  vx org agreement acme --download --acceptance 2c0f...`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.GetOrgAgreement(ctxOf(cmd), connect.NewRequest(&vxv1.GetOrgAgreementRequest{
				Org: args[0], Text: text || download, Version: version, Acceptance: acceptance,
			}))
			if err != nil {
				return err
			}
			m := resp.Msg
			if !m.Required {
				a.printf("This server does not ask organizations to accept a data processing agreement.\n")
				a.emit(m)
				return nil
			}
			if download {
				name := output
				if name == "" {
					name = fmt.Sprintf("dpa-%s-%s.md", args[0], m.TextVersion.GetId())
					if acceptance != "" {
						name = fmt.Sprintf("dpa-%s-%s-accepted.md", args[0], m.TextVersion.GetId())
					}
				}
				if !filepath.IsAbs(name) {
					name = filepath.Join(a.dir(), name)
				}
				sum := sha256.Sum256(m.Text)
				if got := hex.EncodeToString(sum[:]); got != m.TextVersion.GetSha256() {
					return fmt.Errorf("the text the server sent has SHA-256 %s, not %s as it says; try again", got, m.TextVersion.GetSha256())
				}
				if err := os.WriteFile(name, m.Text, 0o644); err != nil {
					return err
				}
				a.printf("Saved version %s of the data processing agreement to %s (SHA-256 %s).\n", m.TextVersion.GetId(), name, m.TextVersion.GetSha256())
				a.emit(m)
				return nil
			}
			if text {
				if !a.jsonOut {
					_, _ = a.Out.Write(m.Text)
				}
				a.emit(m)
				return nil
			}
			if m.Current != nil {
				a.printf("In effect: version %s (%s), since %s\n", m.Current.Id, m.Current.Title, agreementDate(m.Current.Effective))
			}
			if acc := m.CurrentAccepted; acc != nil {
				a.printf("Accepted by %s on %s.\n", acc.AcceptedBy, agreementDate(acc.AcceptedAt))
			} else {
				a.printf("Not accepted: %s\n", m.Missing)
			}
			for _, v := range m.Announced {
				state := "not accepted yet"
				for _, id := range m.AnnouncedAccepted {
					if id == v.Id {
						state = "accepted"
					}
				}
				a.printf("Announced: version %s, published %s, takes effect %s (%s). The earlier acceptance counts until then.\n", v.Id, agreementDate(v.Published), agreementDate(v.Effective), state)
			}
			if len(m.History) > 0 {
				a.printf("\nAcceptances (%d):\n", len(m.History))
				for _, h := range m.History {
					a.printf("  %-12s %-28s %s  from %-15s %s  id %s\n", h.Version, h.AcceptedBy, agreementDate(h.AcceptedAt), h.Ip, short(h.Sha256), h.Id)
				}
			}
			if m.Pending != nil {
				who := "An owner (" + m.Owners + ") accepts"
				if m.CanAccept {
					who = "As an owner, you accept"
				}
				a.printf("\n%s version %s in the web UI, signed in as a person: %s\n", who, m.Pending.Id, m.AcceptUrl)
				a.printf("Read it here first: vx org agreement %s --text --version %s\n", args[0], m.Pending.Id)
			}
			a.emit(m)
			return nil
		},
	}
	cmd.Flags().BoolVar(&text, "text", false, "print the Markdown text of the version (default: the one in effect)")
	cmd.Flags().BoolVar(&download, "download", false, "save the text to a file (dpa-ORG-VERSION.md, or -o)")
	cmd.Flags().StringVar(&version, "version", "", "the version for --text and --download")
	cmd.Flags().StringVar(&acceptance, "acceptance", "", "with --text or --download: the text exactly as this acceptance (id) accepted it")
	cmd.Flags().StringVarP(&output, "output", "o", "", "with --download: the file to write")
	return cmd
}

// agreementDate shows an RFC 3339 time as a date, or as a time when it is not
// midnight UTC.
func agreementDate(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	if t.Equal(t.Truncate(24 * time.Hour)) {
		return t.UTC().Format(time.DateOnly)
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
