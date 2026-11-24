package gitbridge

// Gitea and Forgejo pull requests (one API, /api/v1). Forgejo is a fork of
// Gitea and keeps its API; Codeberg runs Forgejo.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/TerracodeBA/vx/internal/provider"
)

type giteaHost struct{ c *hostClient }

type giteaPR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"` // open, closed
	Merged  bool   `json:"merged"`
	Head    struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p giteaPR) info() provider.PRInfo {
	st := provider.PRStateOpen
	switch {
	case p.Merged:
		st = provider.PRStateMerged
	case p.State == "closed":
		st = provider.PRStateClosed
	}
	return provider.PRInfo{Number: p.Number, URL: p.HTMLURL, State: st, Head: p.Head.Ref, Base: p.Base.Ref}
}

func (g giteaHost) repoPath() string {
	owner, name, _ := strings.Cut(strings.Trim(g.c.repo.Remote, "/"), "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (g giteaHost) open(ctx context.Context, pr provider.PullRequest) (provider.OpenedPR, error) {
	title := draftTitle(pr.Title, pr.Draft, "WIP: ")
	cur, err := g.find(ctx, pr.Head)
	if err == provider.ErrNotFound {
		var created giteaPR
		status, body, err := g.c.do(ctx, http.MethodPost, g.repoPath()+"/pulls", map[string]any{
			"head": pr.Head, "base": pr.Base, "title": title, "body": pr.Body,
		}, &created)
		if err != nil {
			return provider.OpenedPR{}, err
		}
		if status == http.StatusCreated || status == http.StatusOK {
			return provider.OpenedPR{Number: created.Number, URL: created.HTMLURL}, nil
		}
		if status != http.StatusConflict {
			return provider.OpenedPR{}, g.c.fail("open pull request", status, body)
		}
		if cur, err = g.find(ctx, pr.Head); err != nil {
			return provider.OpenedPR{}, err
		}
	} else if err != nil {
		return provider.OpenedPR{}, err
	}
	var upd giteaPR
	status, body, err := g.c.do(ctx, http.MethodPatch, g.repoPath()+"/pulls/"+strconv.Itoa(cur.Number), map[string]any{
		"title": title, "body": pr.Body, "base": pr.Base,
	}, &upd)
	if err != nil {
		return provider.OpenedPR{}, err
	}
	if status != http.StatusOK && status != http.StatusCreated { // Gitea answers an edit with 201
		return provider.OpenedPR{}, g.c.fail(fmt.Sprintf("update pull request #%d", cur.Number), status, body)
	}
	u := upd.HTMLURL
	if u == "" {
		u = cur.URL
	}
	return provider.OpenedPR{Number: cur.Number, URL: u, Updated: true}, nil
}

// find pages through the open pull requests: the API has no filter by
// head branch on every version vx supports.
func (g giteaHost) find(ctx context.Context, head string) (provider.PRInfo, error) {
	var best *giteaPR
	for page := 1; page <= 20; page++ {
		q := url.Values{"state": {"open"}, "limit": {"50"}, "page": {strconv.Itoa(page)}}
		var prs []giteaPR
		status, body, err := g.c.do(ctx, http.MethodGet, g.repoPath()+"/pulls?"+q.Encode(), nil, &prs)
		if err != nil {
			return provider.PRInfo{}, err
		}
		if status != http.StatusOK {
			return provider.PRInfo{}, g.c.fail("find pull request", status, body)
		}
		for i := range prs {
			p := prs[i]
			if p.Head.Ref != head {
				continue
			}
			if p.Head.Repo != nil && p.Head.Repo.FullName != "" && !strings.EqualFold(p.Head.Repo.FullName, g.c.repo.Remote) {
				continue // from a fork
			}
			if best == nil || p.Number > best.Number {
				best = &p
			}
		}
		if len(prs) < 50 {
			break
		}
	}
	if best == nil {
		return provider.PRInfo{}, provider.ErrNotFound
	}
	return best.info(), nil
}

func (g giteaHost) get(ctx context.Context, number int) (provider.PRInfo, error) {
	var p giteaPR
	status, body, err := g.c.do(ctx, http.MethodGet, g.repoPath()+"/pulls/"+strconv.Itoa(number), nil, &p)
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
