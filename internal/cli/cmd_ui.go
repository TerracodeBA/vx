package cli

import (
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

func (a *App) uiCmd() *cobra.Command {
	var printOnly bool
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "ui [EFFORT]",
		Short: "Open the web UI in your browser, already signed in",
		Long: `Open the vx web UI signed in as you, without pasting a token.

vx asks the server for a one-time sign-in link (valid for two minutes, usable
once) and opens it in your browser. Inside a working copy it opens that
effort; pass an effort slug to open another one.

Use --print to print the link instead, for example on a remote machine.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			next := "/ui"
			if repo, slug, err := a.resolveEffort(repoFlag, args); err == nil {
				next = "/ui/effort?repo=" + url.QueryEscape(repo) + "&slug=" + url.QueryEscape(slug)
			} else if len(args) > 0 {
				return err
			}
			resp, err := c.Auth.CreateLoginCode(ctxOf(cmd), connect.NewRequest(&vxv1.CreateLoginCodeRequest{}))
			if err != nil {
				return err
			}
			link := strings.TrimRight(a.settings().Server, "/") + "/ui/login/code?c=" + url.QueryEscape(resp.Msg.Code) + "&next=" + url.QueryEscape(next)
			opened := false
			if !printOnly && !a.jsonOut {
				open := a.OpenBrowser
				if open == nil {
					open = openBrowser
				}
				opened = open(link) == nil
			}
			if opened {
				a.printf("Opened the vx web UI in your browser. Click Continue to sign in (the link works once, for two minutes).\n")
			} else {
				a.printf("Open this link to sign in to the vx web UI (works once, for two minutes):\n  %s\n", link)
			}
			a.emit(map[string]any{"url": link, "expires_at": resp.Msg.ExpiresAt, "opened": opened})
			return nil
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the sign-in link instead of opening a browser")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository of the effort to open")
	return cmd
}

// openBrowser opens url with the system's default browser.
func openBrowser(link string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", link)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return errors.New("no xdg-open")
		}
		c = exec.Command("xdg-open", link)
	}
	return c.Start()
}
