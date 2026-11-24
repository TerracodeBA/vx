package gitbridge

// Repository sizes from the git host's API, so quotas can refuse an
// import before vx downloads anything (#38).

import (
	"context"
	"net/http"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// RepoSize asks the host how big repo is, in bytes. known is false when
// the host does not say: plain git remotes, a GitLab project whose
// statistics the token may not read, or an answer that is not 200.
// GitHub and Gitea report kilobytes; GitLab and Bitbucket bytes.
func (b *Bridge) RepoSize(ctx context.Context, repo store.Repo) (int64, bool, error) {
	switch repo.Provider {
	case store.ProviderGitHub:
		var r struct {
			Size *int64 `json:"size"`
		}
		status, _, err := b.github(ctx, repo, http.MethodGet, "/repos/"+strings.Trim(repo.Remote, "/"), nil, &r)
		if err != nil || status != http.StatusOK || r.Size == nil {
			return 0, false, err
		}
		return *r.Size * 1024, true, nil
	case store.ProviderGitLab, store.ProviderGitea, store.ProviderForgejo, store.ProviderBitbucket:
	default:
		return 0, false, nil
	}
	c, err := b.hostClient(ctx, repo)
	if err != nil {
		return 0, false, err
	}
	switch repo.Provider {
	case store.ProviderGitLab:
		var r struct {
			Statistics *struct {
				RepositorySize *int64 `json:"repository_size"`
			} `json:"statistics"`
		}
		status, _, err := c.do(ctx, http.MethodGet, gitlabHost{c}.project()+"?statistics=true", nil, &r)
		if err != nil || status != http.StatusOK || r.Statistics == nil || r.Statistics.RepositorySize == nil {
			return 0, false, err
		}
		return *r.Statistics.RepositorySize, true, nil
	case store.ProviderBitbucket:
		var r struct {
			Size *int64 `json:"size"`
		}
		status, _, err := c.do(ctx, http.MethodGet, bitbucketHost{c}.repoPath(), nil, &r)
		if err != nil || status != http.StatusOK || r.Size == nil {
			return 0, false, err
		}
		return *r.Size, true, nil
	default: // Gitea, Forgejo
		var r struct {
			Size *int64 `json:"size"`
		}
		status, _, err := c.do(ctx, http.MethodGet, giteaHost{c}.repoPath(), nil, &r)
		if err != nil || status != http.StatusOK || r.Size == nil {
			return 0, false, err
		}
		return *r.Size * 1024, true, nil
	}
}
