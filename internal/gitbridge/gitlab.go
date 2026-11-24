package gitbridge

// GitLab merge requests through the REST API v4 (gitlab.com and
// self-managed). Projects are addressed by their URL-encoded path, merge
// requests by their iid (the number people see).

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/TerracodeBA/vx/internal/provider"
)

type gitlabHost struct{ c *hostClient }

type gitlabMR struct {
	IID          int    `json:"iid"`
	WebURL       string `json:"web_url"`
	State        string `json:"state"` // opened, closed, locked, merged
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
}

func (m gitlabMR) info() provider.PRInfo {
	st := provider.PRStateOpen
	switch m.State {
	case "merged":
		st = provider.PRStateMerged
	case "closed":
		st = provider.PRStateClosed
	}
	return provider.PRInfo{Number: m.IID, URL: m.WebURL, State: st, Head: m.SourceBranch, Base: m.TargetBranch}
}

// project is the API path of the repository's project.
func (g gitlabHost) project() string {
	return "/projects/" + strings.ReplaceAll(url.PathEscape(strings.Trim(g.c.repo.Remote, "/")), "/", "%2F")
}

// draftTitle marks a draft the way GitLab does: a "Draft: " prefix.
func draftTitle(title string, draft bool, prefix string) string {
	if draft && !strings.HasPrefix(title, prefix) {
		return prefix + title
	}
	return title
}

func (g gitlabHost) open(ctx context.Context, pr provider.PullRequest) (provider.OpenedPR, error) {
	title := draftTitle(pr.Title, pr.Draft, "Draft: ")
	cur, err := g.find(ctx, pr.Head)
	if err == provider.ErrNotFound {
		var mr gitlabMR
		status, body, err := g.c.do(ctx, http.MethodPost, g.project()+"/merge_requests", map[string]any{
			"source_branch": pr.Head, "target_branch": pr.Base, "title": title, "description": pr.Body,
			"remove_source_branch": false,
		}, &mr)
		if err != nil {
			return provider.OpenedPR{}, err
		}
		if status == http.StatusCreated || status == http.StatusOK {
			return provider.OpenedPR{Number: mr.IID, URL: mr.WebURL}, nil
		}
		if status != http.StatusConflict {
			return provider.OpenedPR{}, g.c.fail("open merge request", status, body)
		}
		// Opened meanwhile: update that one.
		if cur, err = g.find(ctx, pr.Head); err != nil {
			return provider.OpenedPR{}, err
		}
	} else if err != nil {
		return provider.OpenedPR{}, err
	}
	var mr gitlabMR
	status, body, err := g.c.do(ctx, http.MethodPut, g.project()+"/merge_requests/"+strconv.Itoa(cur.Number), map[string]any{
		"title": title, "description": pr.Body, "target_branch": pr.Base,
	}, &mr)
	if err != nil {
		return provider.OpenedPR{}, err
	}
	if status != http.StatusOK {
		return provider.OpenedPR{}, g.c.fail(fmt.Sprintf("update merge request !%d", cur.Number), status, body)
	}
	u := mr.WebURL
	if u == "" {
		u = cur.URL
	}
	return provider.OpenedPR{Number: cur.Number, URL: u, Updated: true}, nil
}

func (g gitlabHost) find(ctx context.Context, head string) (provider.PRInfo, error) {
	q := url.Values{"source_branch": {head}, "state": {"opened"}, "order_by": {"created_at"}, "sort": {"desc"}}
	var mrs []gitlabMR
	status, body, err := g.c.do(ctx, http.MethodGet, g.project()+"/merge_requests?"+q.Encode(), nil, &mrs)
	if err != nil {
		return provider.PRInfo{}, err
	}
	if status != http.StatusOK {
		return provider.PRInfo{}, g.c.fail("find merge request", status, body)
	}
	for _, m := range mrs {
		if m.SourceBranch == head {
			return m.info(), nil
		}
	}
	return provider.PRInfo{}, provider.ErrNotFound
}

func (g gitlabHost) get(ctx context.Context, number int) (provider.PRInfo, error) {
	var mr gitlabMR
	status, body, err := g.c.do(ctx, http.MethodGet, g.project()+"/merge_requests/"+strconv.Itoa(number), nil, &mr)
	if err != nil {
		return provider.PRInfo{}, err
	}
	if status == http.StatusNotFound {
		return provider.PRInfo{}, provider.ErrNotFound
	}
	if status != http.StatusOK {
		return provider.PRInfo{}, g.c.fail(fmt.Sprintf("read merge request !%d", number), status, body)
	}
	return mr.info(), nil
}
