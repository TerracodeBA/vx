package gitbridge

// Bitbucket Cloud pull requests through the REST API 2.0. Tokens are
// repository, project or workspace access tokens (Bearer), or
// "username:app-password" (Basic).

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/TerracodeBA/vx/internal/provider"
)

type bitbucketHost struct{ c *hostClient }

type bitbucketBranchRef struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository,omitempty"`
}

type bitbucketPR struct {
	ID    int    `json:"id"`
	State string `json:"state"` // OPEN, MERGED, DECLINED, SUPERSEDED
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	Source      bitbucketBranchRef `json:"source"`
	Destination bitbucketBranchRef `json:"destination"`
}

func (p bitbucketPR) info() provider.PRInfo {
	st := provider.PRStateOpen
	switch p.State {
	case "MERGED":
		st = provider.PRStateMerged
	case "DECLINED", "SUPERSEDED":
		st = provider.PRStateClosed
	}
	return provider.PRInfo{Number: p.ID, URL: p.Links.HTML.Href, State: st, Head: p.Source.Branch.Name, Base: p.Destination.Branch.Name}
}

func (g bitbucketHost) repoPath() string {
	ws, slug, _ := strings.Cut(strings.Trim(g.c.repo.Remote, "/"), "/")
	return "/repositories/" + url.PathEscape(ws) + "/" + url.PathEscape(slug)
}

func branchRef(name string) map[string]any {
	return map[string]any{"branch": map[string]any{"name": name}}
}

func (g bitbucketHost) open(ctx context.Context, pr provider.PullRequest) (provider.OpenedPR, error) {
	cur, err := g.find(ctx, pr.Head)
	if err == provider.ErrNotFound {
		var created bitbucketPR
		status, body, err := g.c.do(ctx, http.MethodPost, g.repoPath()+"/pullrequests", map[string]any{
			"title": pr.Title, "description": pr.Body, "source": branchRef(pr.Head), "destination": branchRef(pr.Base),
			"close_source_branch": false, "draft": pr.Draft,
		}, &created)
		if err != nil {
			return provider.OpenedPR{}, err
		}
		if status == http.StatusCreated || status == http.StatusOK {
			return provider.OpenedPR{Number: created.ID, URL: created.Links.HTML.Href}, nil
		}
		return provider.OpenedPR{}, g.c.fail("open pull request", status, body)
	} else if err != nil {
		return provider.OpenedPR{}, err
	}
	var upd bitbucketPR
	status, body, err := g.c.do(ctx, http.MethodPut, g.repoPath()+"/pullrequests/"+strconv.Itoa(cur.Number), map[string]any{
		"title": pr.Title, "description": pr.Body, "destination": branchRef(pr.Base),
	}, &upd)
	if err != nil {
		return provider.OpenedPR{}, err
	}
	if status != http.StatusOK {
		return provider.OpenedPR{}, g.c.fail(fmt.Sprintf("update pull request #%d", cur.Number), status, body)
	}
	u := upd.Links.HTML.Href
	if u == "" {
		u = cur.URL
	}
	return provider.OpenedPR{Number: cur.Number, URL: u, Updated: true}, nil
}

// bbqlString quotes a value for a Bitbucket query.
func bbqlString(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func (g bitbucketHost) find(ctx context.Context, head string) (provider.PRInfo, error) {
	q := url.Values{
		"q":    {"source.branch.name = " + bbqlString(head) + ` AND state = "OPEN"`},
		"sort": {"-created_on"},
	}
	var page struct {
		Values []bitbucketPR `json:"values"`
	}
	status, body, err := g.c.do(ctx, http.MethodGet, g.repoPath()+"/pullrequests?"+q.Encode(), nil, &page)
	if err != nil {
		return provider.PRInfo{}, err
	}
	if status != http.StatusOK {
		return provider.PRInfo{}, g.c.fail("find pull request", status, body)
	}
	for _, p := range page.Values {
		if p.Source.Branch.Name != head || p.State != "OPEN" {
			continue
		}
		if r := p.Source.Repository; r != nil && r.FullName != "" && !strings.EqualFold(r.FullName, g.c.repo.Remote) {
			continue // from a fork
		}
		return p.info(), nil
	}
	return provider.PRInfo{}, provider.ErrNotFound
}

func (g bitbucketHost) get(ctx context.Context, number int) (provider.PRInfo, error) {
	var p bitbucketPR
	status, body, err := g.c.do(ctx, http.MethodGet, g.repoPath()+"/pullrequests/"+strconv.Itoa(number), nil, &p)
	if err != nil {
		return provider.PRInfo{}, err
	}
	if status == http.StatusNotFound {
		return provider.PRInfo{}, provider.ErrNotFound
	}
	if status != http.StatusOK {
		return provider.PRInfo{}, g.c.fail(fmt.Sprintf("read pull request #%d", number), status, body)
	}
	return p.info(), nil
}
