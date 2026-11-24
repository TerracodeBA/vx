package service

// GitLab, Gitea, Forgejo and Bitbucket Cloud repositories: their address,
// the token vx keeps for each (sealed with VX_SECRET_KEY) and the secret
// their webhooks sign deliveries with. Roles on these repositories are vx's
// own (grants and organization defaults), never the host's permissions.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/secret"
	"github.com/TerracodeBA/vx/internal/store"
)

// HostSettings configure GitLab, Gitea, Forgejo and Bitbucket on a server.
type HostSettings struct {
	// WebhookSecrets are server-wide webhook secrets per provider
	// (VX_GITLAB_WEBHOOK_SECRET, VX_GITEA_WEBHOOK_SECRET,
	// VX_FORGEJO_WEBHOOK_SECRET, VX_BITBUCKET_WEBHOOK_SECRET). A delivery
	// signed with one is accepted for every repository of that provider
	// that has no secret of its own.
	WebhookSecrets map[store.Provider]string
}

// hostURLs is what the git bridge knows about default host addresses.
type hostURLs interface {
	HostURL(p store.Provider) string
}

// Webhook paths of the hosts.
const (
	GitLabWebhookPath    = "/hooks/gitlab"
	GiteaWebhookPath     = "/hooks/gitea" // Gitea and Forgejo
	ForgejoWebhookPath   = "/hooks/forgejo"
	BitbucketWebhookPath = "/hooks/bitbucket"
)

func tokenPurpose(r store.Repo) string   { return "upstream.token:" + r.ID.String() }
func webhookPurpose(r store.Repo) string { return "upstream.webhook_secret:" + r.ID.String() }

// normalizeHostRemote checks the path of a repository on its host:
// "group/project" (GitLab, subgroups allowed) or "owner/repo".
func normalizeHostRemote(p store.Provider, remote string) (string, error) {
	r := strings.Trim(strings.TrimSpace(remote), "/")
	r = strings.TrimSuffix(r, ".git")
	parts := strings.Split(r, "/")
	ok := len(parts) >= 2
	if p != store.ProviderGitLab && len(parts) != 2 {
		ok = false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, " ?#%\\") {
			ok = false
		}
	}
	if !ok {
		want := "owner/repo"
		switch p {
		case store.ProviderGitLab:
			want = "group/project (subgroups allowed: group/sub/project)"
		case store.ProviderBitbucket:
			want = "workspace/repo"
		}
		return "", errf(CodeInvalid, "%s repository %q: give its path on the host as %s", p.HostName(), remote, want)
	}
	return r, nil
}

// hostBaseURL is the normalized web address of a host: the given one, or
// the server's default for the provider.
func (s *Service) hostBaseURL(p store.Provider, given string) (string, error) {
	v := strings.TrimRight(strings.TrimSpace(given), "/")
	if v == "" {
		if h, ok := s.Upstream.(hostURLs); ok {
			v = h.HostURL(p)
		}
	}
	if v == "" {
		return "", errf(CodeInvalid, "a %s repository needs the host's address: --%s-url https://%s.example.com (or VX_%s_URL on the server)",
			p.HostName(), p, p, strings.ToUpper(string(p)))
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errf(CodeInvalid, "%s address %q must be an http(s) URL like https://%s.example.com, without credentials or a query", p.HostName(), given, p)
	}
	return v, nil
}

type hostTokenKey struct{}

// withHostToken makes HostRepoToken return tok for the repository being
// created, before its token is stored.
func withHostToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, hostTokenKey{}, tok)
}

// HostRepoToken returns the token vx keeps for a GitLab, Gitea, Forgejo or
// Bitbucket repository, or "" when it has none (the server's token for
// its host applies). The git bridge calls it; nothing logs the result.
func (s *Service) HostRepoToken(ctx context.Context, repo store.Repo) (string, error) {
	if !repo.Provider.Hosted() {
		return "", nil
	}
	if tok, ok := ctx.Value(hostTokenKey{}).(string); ok && tok != "" {
		return tok, nil
	}
	if repo.ID == uuid.Nil {
		return "", nil
	}
	sec, err := s.Store.GetUpstreamSecrets(ctx, repo.ID)
	if err != nil || len(sec.TokenEnc) == 0 {
		return "", err
	}
	key, err := s.secretKey()
	if err != nil {
		return "", err
	}
	return secret.OpenString(key, tokenPurpose(repo), sec.TokenEnc)
}

// CanSealSecrets reports whether the server has a key to keep secrets.
func (s *Service) CanSealSecrets() bool {
	_, err := s.secretKey()
	return err == nil
}

// HostHook is how to register a repository's webhook on its host.
type HostHook struct {
	URL string // where the host delivers, e.g. https://vx.example.com/hooks/gitlab
	// Secret is the repository's webhook secret, only right after it was
	// made (vx keeps it sealed and never shows it again).
	Secret string
	// SecretSource says which secret verifies deliveries: "repository",
	// "server" (VX_<HOST>_WEBHOOK_SECRET) or "" (none yet: deliveries are
	// refused).
	SecretSource string
}

// HostWebhookPath is the path a provider's webhooks go to, or "".
func HostWebhookPath(p store.Provider) string {
	switch p {
	case store.ProviderGitLab:
		return GitLabWebhookPath
	case store.ProviderGitea:
		return GiteaWebhookPath
	case store.ProviderForgejo:
		return ForgejoWebhookPath
	case store.ProviderBitbucket:
		return BitbucketWebhookPath
	}
	return ""
}

func (s *Service) hostHook(ctx context.Context, repo store.Repo) HostHook {
	h := HostHook{URL: strings.TrimRight(s.GitHub.PublicURL, "/") + HostWebhookPath(repo.Provider)}
	if sec, err := s.Store.GetUpstreamSecrets(ctx, repo.ID); err == nil && len(sec.WebhookSecretEnc) > 0 {
		h.SecretSource = "repository"
	} else if s.Hosts.WebhookSecrets[repo.Provider] != "" {
		h.SecretSource = "server"
	}
	return h
}

// SetRepoHostInput changes how vx reaches a repository's host.
type SetRepoHostInput struct {
	Repo string
	// Token replaces the repository's token; "" removes it (the server's
	// token for the host applies again). nil: unchanged.
	Token *string
	// RotateWebhookSecret makes a new webhook secret and returns it once.
	RotateWebhookSecret bool
}

// SetRepoHost sets the token of a GitLab, Gitea, Forgejo or Bitbucket
// repository or makes a new webhook secret for it (admin), and says how to
// register the webhook.
func (s *Service) SetRepoHost(ctx context.Context, actor Actor, in SetRepoHostInput) (HostHook, error) {
	repo, _, err := s.Authorize(ctx, actor, in.Repo, RoleAdmin)
	if err != nil {
		return HostHook{}, err
	}
	if !repo.Provider.Hosted() {
		return HostHook{}, errf(CodeFailedPrecondition, "repo %s follows %s: tokens and webhook secrets here are for GitLab, Gitea, Forgejo and Bitbucket repositories", repo.Name, repo.HostName())
	}
	var changed []string
	var hookSecret string
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		if in.Token != nil {
			var sealed []byte
			if tok := strings.TrimSpace(*in.Token); tok != "" {
				key, err := s.secretKey()
				if err != nil {
					return err
				}
				if sealed, err = secret.SealString(key, tokenPurpose(repo), tok); err != nil {
					return err
				}
				changed = append(changed, "token set")
			} else {
				changed = append(changed, "token removed")
			}
			if err := tx.SetUpstreamToken(ctx, repo.ID, sealed); err != nil {
				return err
			}
		}
		if in.RotateWebhookSecret {
			key, err := s.secretKey()
			if err != nil {
				return err
			}
			raw := make([]byte, 24)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			hookSecret = hex.EncodeToString(raw)
			sealed, err := secret.SealString(key, webhookPurpose(repo), hookSecret)
			if err != nil {
				return err
			}
			if err := tx.SetUpstreamWebhookSecret(ctx, repo.ID, sealed); err != nil {
				return err
			}
			changed = append(changed, "new webhook secret")
		}
		if len(changed) == 0 {
			return nil
		}
		// What changed, never the values.
		return tx.LogOp(ctx, &repo.ID, &actor.ID, "repo.host", repo.Name, nil, map[string]any{"changed": changed})
	})
	if err != nil {
		return HostHook{}, classify(err)
	}
	h := s.hostHook(ctx, repo)
	h.Secret = hookSecret
	return h, nil
}

// repoWebhookSecret opens a repository's own webhook secret; "" if none.
func (s *Service) repoWebhookSecret(ctx context.Context, repo store.Repo) (string, error) {
	sec, err := s.Store.GetUpstreamSecrets(ctx, repo.ID)
	if err != nil || len(sec.WebhookSecretEnc) == 0 {
		return "", err
	}
	key, err := s.secretKey()
	if err != nil {
		return "", err
	}
	return secret.OpenString(key, webhookPurpose(repo), sec.WebhookSecretEnc)
}
