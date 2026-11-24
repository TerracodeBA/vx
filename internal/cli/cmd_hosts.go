package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// hostFlags are the --gitlab, --gitea, --forgejo and --bitbucket flags of
// vx repo create, each with its -url flag.
type hostFlags struct {
	gitlab, gitlabURL       string
	gitea, giteaURL         string
	forgejo, forgejoURL     string
	bitbucket, bitbucketURL string
}

type hostFlag struct {
	provider    string
	remote, url *string
}

func (h *hostFlags) all() []hostFlag {
	return []hostFlag{
		{"gitlab", &h.gitlab, &h.gitlabURL},
		{"gitea", &h.gitea, &h.giteaURL},
		{"forgejo", &h.forgejo, &h.forgejoURL},
		{"bitbucket", &h.bitbucket, &h.bitbucketURL},
	}
}

func (h *hostFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&h.gitlab, "gitlab", "", "GitLab project as group/project (subgroups allowed)")
	f.StringVar(&h.gitlabURL, "gitlab-url", "", "self-managed GitLab address (default: the server's, else https://gitlab.com)")
	f.StringVar(&h.gitea, "gitea", "", "Gitea repository as owner/repo")
	f.StringVar(&h.giteaURL, "gitea-url", "", "Gitea address, e.g. https://gitea.example.com (default: the server's)")
	f.StringVar(&h.forgejo, "forgejo", "", "Forgejo repository as owner/repo")
	f.StringVar(&h.forgejoURL, "forgejo-url", "", "Forgejo address, e.g. https://codeberg.org (default: the server's)")
	f.StringVar(&h.bitbucket, "bitbucket", "", "Bitbucket Cloud repository as workspace/repo")
	f.StringVar(&h.bitbucketURL, "bitbucket-url", "", "Bitbucket address (default: https://bitbucket.org)")
	_ = f.MarkHidden("bitbucket-url")
}

// readToken reads a token from a file, or stdin for "-". Tokens never go
// on the command line, where shell history and ps would see them.
func (a *App) readToken(path string) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read the token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("the token file %s is empty", path)
	}
	return tok, nil
}

// printHostHook says how to register the repository's webhook on its host.
func (a *App) printHostHook(provider, repo string, h *vxv1.HostHook) {
	if h.Url == "" {
		return
	}
	var where, secretField, events string
	switch provider {
	case "gitlab":
		where, secretField, events = "GitLab: Settings > Webhooks > Add new webhook", "Secret token", "Push events, Merge request events"
	case "gitea", "forgejo":
		where, secretField, events = "the repository: Settings > Webhooks > Add webhook > "+map[string]string{"gitea": "Gitea", "forgejo": "Forgejo"}[provider]+" (POST, application/json)", "Secret", "Push events, Pull request events"
	case "bitbucket":
		where, secretField, events = "Bitbucket: Repository settings > Webhooks > Add webhook", "Secret", "Repository push, Pull request merged"
	default:
		return
	}
	a.printf("\nRegister the webhook on %s:\n  URL:     %s\n", where, h.Url)
	switch {
	case h.Secret != "":
		a.printf("  %s: %s\n  (shown once; a new one: vx repo host %s --rotate-webhook-secret)\n", secretField, h.Secret, repo)
	case h.SecretSource == "repository":
		a.printf("  %s: the repository's webhook secret, shown when it was made (a new one: vx repo host %s --rotate-webhook-secret)\n", secretField, repo)
	case h.SecretSource == "server":
		a.printf("  %s: the server's webhook secret for %s (VX_%s_WEBHOOK_SECRET)\n", secretField, provider, strings.ToUpper(provider))
	default:
		a.printf("  %s: none yet; the server needs VX_SECRET_KEY to make one (vx repo host %s --rotate-webhook-secret)\n", secretField, repo)
	}
	a.printf("  Events:  %s\n", events)
}

func (a *App) repoHostCmd() *cobra.Command {
	var tokenFile string
	var clearToken, rotate bool
	cmd := &cobra.Command{
		Use:   "host NAME",
		Short: "Set the token of a GitLab, Gitea, Forgejo or Bitbucket repository, or make a new webhook secret",
		Long: `Change how vx reaches a GitLab, Gitea, Forgejo or Bitbucket repository
(admin). Without flags it prints the webhook to register.

  vx repo host acme/shop --token-file ~/gitlab-token
  vx repo host acme/shop --clear-token            # use the server's token again
  vx repo host acme/shop --rotate-webhook-secret  # prints the new secret once`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tokenFile != "" && clearToken {
				return errors.New("use either --token-file or --clear-token")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			req := &vxv1.SetRepoHostRequest{Repo: args[0], RotateWebhookSecret: rotate}
			if tokenFile != "" {
				if req.Token, err = a.readToken(tokenFile); err != nil {
					return err
				}
				req.SetToken = true
			}
			if clearToken {
				req.SetToken = true
			}
			resp, err := c.Repos.SetRepoHost(ctxOf(cmd), connect.NewRequest(req))
			if err != nil {
				return err
			}
			switch {
			case tokenFile != "":
				a.printf("Stored the token for %s (sealed; vx never shows it).\n", args[0])
			case clearToken:
				a.printf("Removed the token of %s: the server's token for its host applies.\n", args[0])
			}
			rr, err := c.Repos.GetRepo(ctxOf(cmd), connect.NewRequest(&vxv1.GetRepoRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			a.printHostHook(rr.Msg.Repo.Provider, args[0], resp.Msg.Hook)
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file with the repository's token (- reads stdin)")
	cmd.Flags().BoolVar(&clearToken, "clear-token", false, "remove the repository's token")
	cmd.Flags().BoolVar(&rotate, "rotate-webhook-secret", false, "make a new webhook secret (the old one stops working)")
	return cmd
}
