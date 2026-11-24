package gitbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/provider"
	"github.com/TerracodeBA/vx/internal/store"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// OpenPullRequest opens a PR (GitLab: merge request) for head, or updates
// the open one: title, body and base, so a stacked PR can be retargeted.
func (b *Bridge) OpenPullRequest(ctx context.Context, repo store.Repo, pr provider.PullRequest) (provider.OpenedPR, error) {
	if repo.Provider.Hosted() {
		return b.openHostPR(ctx, repo, pr)
	}
	if repo.Provider != store.ProviderGitHub {
		return provider.OpenedPR{}, provider.ErrNoPullRequests
	}
	if b.tokenFor(ctx, repo) == "" {
		// Without an API token vx can push but not open the PR. Hand back
		// the page where a person opens it with one click.
		return provider.OpenedPR{URL: fmt.Sprintf("https://github.com/%s/compare/%s...%s?expand=1", repo.Remote, compareRef(pr.Base), compareRef(pr.Head))},
			provider.ErrNoPullRequests
	}
	if !strings.HasPrefix(pr.Head, BranchPrefix) {
		return provider.OpenedPR{}, fmt.Errorf("refusing to open a PR from %q", pr.Head)
	}
	owner, _, _ := strings.Cut(repo.Remote, "/")
	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	status, body, err := b.github(ctx, repo, http.MethodPost, "/repos/"+repo.Remote+"/pulls", map[string]any{
		"title": pr.Title, "head": pr.Head, "base": pr.Base, "body": pr.Body, "draft": pr.Draft,
	}, &created)
	if err != nil {
		return provider.OpenedPR{}, err
	}
	if status == http.StatusCreated {
		return provider.OpenedPR{Number: created.Number, URL: created.HTMLURL}, nil
	}
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "already exists") {
		return provider.OpenedPR{}, fmt.Errorf("github: open pull request: %d %s", status, trimBody(body))
	}
	// A PR for this branch is open: update it.
	var open []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	q := url.Values{"head": {owner + ":" + pr.Head}, "state": {"open"}}
	status, body, err = b.github(ctx, repo, http.MethodGet, "/repos/"+repo.Remote+"/pulls?"+q.Encode(), nil, &open)
	if err != nil {
		return provider.OpenedPR{}, err
	}
	if status != http.StatusOK || len(open) == 0 {
		return provider.OpenedPR{}, fmt.Errorf("github: find open pull request: %d %s", status, trimBody(body))
	}
	n := open[0].Number
	status, body, err = b.github(ctx, repo, http.MethodPatch, "/repos/"+repo.Remote+"/pulls/"+strconv.Itoa(n), map[string]any{
		"title": pr.Title, "body": pr.Body, "base": pr.Base,
	}, nil)
	if err != nil {
		return provider.OpenedPR{}, err
	}
	if status != http.StatusOK {
		return provider.OpenedPR{}, fmt.Errorf("github: update pull request #%d: %d %s", n, status, trimBody(body))
	}
	return provider.OpenedPR{Number: n, URL: open[0].HTMLURL, Updated: true}, nil
}

func (b *Bridge) github(ctx context.Context, repo store.Repo, method, path string, in, out any) (int, string, error) {
	var rd io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, "", err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.GitHubAPI, "/")+path, rd)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "vx")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := b.tokenFor(ctx, repo); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, "", err
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, string(data), fmt.Errorf("github: decode response: %w", err)
		}
	}
	return resp.StatusCode, string(data), nil
}

func trimBody(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// compareRef escapes a branch name for a GitHub compare URL, keeping the
// slashes GitHub expects in names like vx/rate-limiting.
func compareRef(branch string) string {
	return strings.ReplaceAll(url.PathEscape(branch), "%2F", "/")
}
