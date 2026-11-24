package gitbridge

// GitLab, Gitea, Forgejo and Bitbucket Cloud: where their repositories
// live, which token reaches them, and their pull request APIs (gitlab.go,
// gitea.go, bitbucket.go). Tokens go in Authorization headers only, never
// in URLs or logs.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/TerracodeBA/vx/internal/provider"
	"github.com/TerracodeBA/vx/internal/store"
)

// HostConfig is the server's configuration for one kind of host.
type HostConfig struct {
	// URL is the host's web address repositories default to, e.g.
	// https://gitlab.com; empty: every repository names its own.
	URL string
	// APIURL overrides the API address for repositories on URL; empty:
	// derived from the repository's address (GitLab /api/v4, Gitea and
	// Forgejo /api/v1, Bitbucket https://api.bitbucket.org/2.0).
	APIURL string
	// Token is the server's token for repositories on URL ("" for none).
	// It is never sent to another address: a repository elsewhere needs
	// its own token.
	Token func() string
}

// Default host addresses.
const (
	GitLabURL       = "https://gitlab.com"
	BitbucketURL    = "https://bitbucket.org"
	BitbucketAPIURL = "https://api.bitbucket.org/2.0"
)

func defaultHosts() map[store.Provider]HostConfig {
	return map[store.Provider]HostConfig{
		store.ProviderGitLab:    {URL: GitLabURL},
		store.ProviderBitbucket: {URL: BitbucketURL, APIURL: BitbucketAPIURL},
		store.ProviderGitea:     {},
		store.ProviderForgejo:   {},
	}
}

// SetHost configures one kind of host; empty fields keep the defaults.
func (b *Bridge) SetHost(p store.Provider, cfg HostConfig) {
	if b.Hosts == nil {
		b.Hosts = defaultHosts()
	}
	cur := b.Hosts[p]
	if cfg.URL != "" {
		cur.URL = strings.TrimRight(cfg.URL, "/")
		if p == store.ProviderBitbucket && cfg.APIURL == "" && cur.URL != BitbucketURL {
			cur.APIURL = ""
		}
	}
	if cfg.APIURL != "" {
		cur.APIURL = strings.TrimRight(cfg.APIURL, "/")
	}
	if cfg.Token != nil {
		cur.Token = cfg.Token
	}
	b.Hosts[p] = cur
}

// HostURL is the address repositories of p default to, or "".
func (b *Bridge) HostURL(p store.Provider) string {
	return b.Hosts[p].URL
}

// UseHostTokens sets HostToken; the server passes the tokens it keeps per
// repository this way.
func (b *Bridge) UseHostTokens(f func(ctx context.Context, repo store.Repo) (string, error)) {
	b.HostToken = f
}

// sameHost reports whether two web addresses are the same host (scheme,
// host and port), so a server token only goes where it was meant to.
func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil || ua.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}

// hostToken is the token for a GitLab, Gitea, Forgejo or Bitbucket
// repository: its own, else the server's for its host.
func (b *Bridge) hostToken(ctx context.Context, repo store.Repo) string {
	if b.HostToken != nil {
		tok, err := b.HostToken(ctx, repo)
		if err != nil {
			b.Log.Warn("repository token", "repo", repo.Name, "err", err)
		} else if tok != "" {
			return tok
		}
	}
	cfg := b.Hosts[repo.Provider]
	if cfg.Token != nil && cfg.URL != "" && sameHost(b.hostBase(repo), cfg.URL) {
		return strings.TrimSpace(cfg.Token())
	}
	return ""
}

// hostBase is the repository's web address on its host.
func (b *Bridge) hostBase(repo store.Repo) string {
	if repo.BaseURL != "" {
		return strings.TrimRight(repo.BaseURL, "/")
	}
	return b.Hosts[repo.Provider].URL
}

// apiBase is the host's REST API address for the repository.
func (b *Bridge) apiBase(repo store.Repo) (string, error) {
	base := b.hostBase(repo)
	if base == "" {
		return "", fmt.Errorf("repo %s has no %s address: set one with --%s-url", repo.Name, repo.HostName(), repo.Provider)
	}
	cfg := b.Hosts[repo.Provider]
	if cfg.APIURL != "" && cfg.URL != "" && sameHost(base, cfg.URL) {
		return cfg.APIURL, nil
	}
	switch repo.Provider {
	case store.ProviderGitLab:
		return base + "/api/v4", nil
	case store.ProviderGitea, store.ProviderForgejo:
		return base + "/api/v1", nil
	case store.ProviderBitbucket:
		if sameHost(base, BitbucketURL) {
			return BitbucketAPIURL, nil
		}
		return base + "/2.0", nil
	}
	return "", fmt.Errorf("repo %s: %s has no pull request API", repo.Name, repo.Provider)
}

// hostGitURL is the HTTPS clone address of a hosted repository.
func (b *Bridge) hostGitURL(repo store.Repo) (string, error) {
	base := b.hostBase(repo)
	if base == "" {
		return "", fmt.Errorf("repo %s has no %s address: set one with --%s-url", repo.Name, repo.HostName(), repo.Provider)
	}
	return base + "/" + strings.Trim(repo.Remote, "/") + ".git", nil
}

// splitUserToken reads "user:secret" (a Bitbucket app password or API
// token with its username); ok is false for a plain token.
func splitUserToken(tok string) (user, secret string, ok bool) {
	user, secret, ok = strings.Cut(tok, ":")
	if !ok || user == "" || secret == "" {
		return "", "", false
	}
	return user, secret, true
}

// hostGitAuth is the HTTPS credential for git: GitLab takes any token as
// the password of user "oauth2", Gitea and Forgejo take a token as the
// password of any user, Bitbucket takes "user:app-password" or an access
// token as the password of "x-token-auth".
func (b *Bridge) hostGitAuth(ctx context.Context, repo store.Repo) transport.AuthMethod {
	tok := b.hostToken(ctx, repo)
	if tok == "" {
		return nil
	}
	if repo.Provider == store.ProviderBitbucket {
		if u, p, ok := splitUserToken(tok); ok {
			return &githttp.BasicAuth{Username: u, Password: p}
		}
		return &githttp.BasicAuth{Username: "x-token-auth", Password: tok}
	}
	return &githttp.BasicAuth{Username: "oauth2", Password: tok}
}

// hostAPIAuth is the Authorization header for the host's API.
func hostAPIAuth(p store.Provider, tok string) string {
	switch p {
	case store.ProviderGitea, store.ProviderForgejo:
		return "token " + tok
	case store.ProviderBitbucket:
		if u, s, ok := splitUserToken(tok); ok {
			return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+s))
		}
	}
	return "Bearer " + tok
}

// hostClient calls one repository's host API with its token.
type hostClient struct {
	b    *Bridge
	repo store.Repo
	api  string
	auth string
}

func (b *Bridge) hostClient(ctx context.Context, repo store.Repo) (*hostClient, error) {
	api, err := b.apiBase(repo)
	if err != nil {
		return nil, err
	}
	c := &hostClient{b: b, repo: repo, api: api}
	if tok := b.hostToken(ctx, repo); tok != "" {
		c.auth = hostAPIAuth(repo.Provider, tok)
	}
	return c, nil
}

// do sends a JSON request; out is filled from a 2xx answer. It returns
// the status and the body for error messages.
func (c *hostClient) do(ctx context.Context, method, path string, in, out any) (int, string, error) {
	var rd io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, "", err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, rd)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vx")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	host := c.repo.HostName()
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("%s: %w", host, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, "", err
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, string(data), fmt.Errorf("%s: decode response: %w", host, err)
		}
	}
	return resp.StatusCode, string(data), nil
}

// fail is the error for an unexpected answer, with a hint for the usual
// causes.
func (c *hostClient) fail(what string, status int, body string) error {
	hint := ""
	switch status {
	case http.StatusUnauthorized:
		hint = "; the token was refused: check it and its scopes (vx repo host " + c.repo.Name + " --token-file FILE)"
	case http.StatusForbidden:
		hint = "; the token may not do this: it needs write access to the repository and its " + c.repo.PRName() + "s"
	case http.StatusNotFound:
		hint = "; check the repository path " + c.repo.Remote + " and that the token can see it"
	}
	return fmt.Errorf("%s: %s: %d %s%s", c.repo.HostName(), what, status, trimBody(body), hint)
}

// prHost is one host's pull request API for one repository.
type prHost interface {
	// open creates a pull request from pr.Head to pr.Base, or updates the
	// open one from pr.Head (title, body and base).
	open(ctx context.Context, pr provider.PullRequest) (provider.OpenedPR, error)
	// find returns the newest open pull request from head, or
	// provider.ErrNotFound.
	find(ctx context.Context, head string) (provider.PRInfo, error)
	// get reads one pull request by its number (GitLab: iid).
	get(ctx context.Context, number int) (provider.PRInfo, error)
}

// prHostFor returns the pull request API of a hosted repository. ok is
// false without a token: then vx can push (to public repositories) but not
// open pull requests.
func (b *Bridge) prHostFor(ctx context.Context, repo store.Repo) (prHost, bool, error) {
	c, err := b.hostClient(ctx, repo)
	if err != nil {
		return nil, false, err
	}
	var h prHost
	switch repo.Provider {
	case store.ProviderGitLab:
		h = gitlabHost{c}
	case store.ProviderGitea, store.ProviderForgejo:
		h = giteaHost{c}
	case store.ProviderBitbucket:
		h = bitbucketHost{c}
	default:
		return nil, false, provider.ErrNoPullRequests
	}
	return h, c.auth != "", nil
}

// newPRLink is the host's page where a person opens the pull request by
// hand, for when vx has no token.
func (b *Bridge) newPRLink(repo store.Repo, pr provider.PullRequest) string {
	web := b.hostBase(repo) + "/" + repo.Remote
	switch repo.Provider {
	case store.ProviderGitLab:
		q := url.Values{"merge_request[source_branch]": {pr.Head}, "merge_request[target_branch]": {pr.Base}}
		return web + "/-/merge_requests/new?" + q.Encode()
	case store.ProviderGitea, store.ProviderForgejo:
		return web + "/compare/" + compareRef(pr.Base) + "..." + compareRef(pr.Head)
	case store.ProviderBitbucket:
		q := url.Values{"source": {pr.Head}, "dest": {pr.Base}}
		return web + "/pull-requests/new?" + q.Encode()
	}
	return ""
}

// openHostPR is OpenPullRequest for GitLab, Gitea, Forgejo and Bitbucket.
func (b *Bridge) openHostPR(ctx context.Context, repo store.Repo, pr provider.PullRequest) (provider.OpenedPR, error) {
	if !strings.HasPrefix(pr.Head, BranchPrefix) {
		return provider.OpenedPR{}, fmt.Errorf("refusing to open a %s from %q", repo.PRName(), pr.Head)
	}
	h, ok, err := b.prHostFor(ctx, repo)
	if err != nil {
		return provider.OpenedPR{}, err
	}
	if !ok {
		return provider.OpenedPR{URL: b.newPRLink(repo, pr)}, provider.ErrNoPullRequests
	}
	return h.open(ctx, pr)
}

// FindPullRequest returns the newest open pull request (GitLab: merge
// request) from head, or provider.ErrNotFound.
func (b *Bridge) FindPullRequest(ctx context.Context, repo store.Repo, head string) (provider.PRInfo, error) {
	if !repo.Provider.Hosted() {
		return provider.PRInfo{}, provider.ErrNoPullRequests
	}
	h, ok, err := b.prHostFor(ctx, repo)
	if err != nil {
		return provider.PRInfo{}, err
	}
	if !ok {
		return provider.PRInfo{}, fmt.Errorf("%s: vx has no token for %s", repo.HostName(), repo.Name)
	}
	return h.find(ctx, head)
}

// PullRequest reads one pull request (GitLab: merge request iid).
func (b *Bridge) PullRequest(ctx context.Context, repo store.Repo, number int) (provider.PRInfo, error) {
	if !repo.Provider.Hosted() {
		return provider.PRInfo{}, provider.ErrNoPullRequests
	}
	h, ok, err := b.prHostFor(ctx, repo)
	if err != nil {
		return provider.PRInfo{}, err
	}
	if !ok {
		return provider.PRInfo{}, fmt.Errorf("%s: vx has no token for %s", repo.HostName(), repo.Name)
	}
	return h.get(ctx, number)
}
