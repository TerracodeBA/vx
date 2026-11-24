// Package hosttest fakes the pull request APIs of GitLab (REST v4), Gitea
// and Forgejo (/api/v1) and Bitbucket Cloud (2.0) for one repository, and
// optionally serves git over HTTP for it (git http-backend), so tests never
// call a real host.
//
//	h := hosttest.New(t, store.ProviderGitLab, "acme/shop", "Bearer gl-token")
//	h.ServeGit(t, bareRepoDir, "oauth2", "gl-token")
//	repo.BaseURL = h.URL
package hosttest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/TerracodeBA/vx/internal/store"
)

// PR is a pull request (GitLab: merge request) on the fake host.
type PR struct {
	Number      int
	Head, Base  string
	Title, Body string
	State       string // open, merged, closed
	Draft       bool
	Fork        bool // from another repository
}

// Host is the fake. Its API checks the Authorization header.
type Host struct {
	Kind   store.Provider
	Remote string // the repository's path on the host
	Auth   string // the Authorization header the API accepts
	URL    string // the fake's address: the repository's base URL

	t        *testing.T
	prefix   string
	mu       sync.Mutex
	prs      []*PR
	requests []string
	created  int
	updated  int
	git      http.Handler
	gitAuth  [2]string
	gitSeen  []string
}

// New starts a fake host for one repository.
func New(t *testing.T, kind store.Provider, remote, auth string) *Host {
	h := &Host{Kind: kind, Remote: remote, Auth: auth, t: t}
	switch kind {
	case store.ProviderGitLab:
		h.prefix = "/api/v4/projects/" + strings.ReplaceAll(url.PathEscape(remote), "/", "%2F") + "/merge_requests"
	case store.ProviderGitea, store.ProviderForgejo:
		h.prefix = "/api/v1/repos/" + remote + "/pulls"
	case store.ProviderBitbucket:
		h.prefix = "/2.0/repositories/" + remote + "/pullrequests"
	default:
		t.Fatalf("hosttest: no fake for %s", kind)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	h.URL = srv.URL
	return h
}

// ServeGit serves the git repository at dir (bare, or a work tree's .git)
// as REMOTE.git over smart HTTP, for Basic auth user:pass only. It needs
// the git binary; the test is skipped without one.
func (h *Host) ServeGit(t *testing.T, dir, user, pass string) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed: needed for git http-backend")
	}
	root := t.TempDir()
	link := filepath.Join(root, filepath.FromSlash(h.Remote)+".git")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
		dir = filepath.Join(dir, ".git")
	}
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gitAuth = [2]string{user, pass}
	h.git = &cgi.Handler{
		Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + user},
	}
}

// GitRequests lists the git requests as "PATH user:pass".
func (h *Host) GitRequests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.gitSeen...)
}

// Requests lists the API requests as "METHOD PATH".
func (h *Host) Requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

// Counts is how many pull requests were created and updated.
func (h *Host) Counts() (created, updated int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.created, h.updated
}

// PR returns a copy of pull request n, or nil.
func (h *Host) PR(n int) *PR {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.prs {
		if p.Number == n {
			c := *p
			return &c
		}
	}
	return nil
}

// Add puts a pull request on the host (one someone else opened).
func (h *Host) Add(p PR) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prs = append(h.prs, &p)
}

// SetState merges or closes pull request n, as a person on the host would.
func (h *Host) SetState(n int, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.prs {
		if p.Number == n {
			p.State = state
		}
	}
}

// PRURL is the page of pull request n.
func (h *Host) PRURL(n int) string {
	if h.Kind == store.ProviderGitLab {
		return fmt.Sprintf("%s/%s/-/merge_requests/%d", h.URL, h.Remote, n)
	}
	return fmt.Sprintf("%s/%s/pulls/%d", h.URL, h.Remote, n)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Host) render(p *PR) map[string]any {
	repo := h.Remote
	if p.Fork {
		repo = "mallory/fork"
	}
	switch h.Kind {
	case store.ProviderGitLab:
		st := map[string]string{"open": "opened", "merged": "merged", "closed": "closed"}[p.State]
		return map[string]any{"iid": p.Number, "web_url": h.PRURL(p.Number), "state": st, "source_branch": p.Head, "target_branch": p.Base, "title": p.Title}
	case store.ProviderBitbucket:
		st := map[string]string{"open": "OPEN", "merged": "MERGED", "closed": "DECLINED"}[p.State]
		return map[string]any{"id": p.Number, "state": st, "links": map[string]any{"html": map[string]any{"href": h.PRURL(p.Number)}},
			"source":      map[string]any{"branch": map[string]any{"name": p.Head}, "repository": map[string]any{"full_name": repo}},
			"destination": map[string]any{"branch": map[string]any{"name": p.Base}}}
	}
	st := "open"
	if p.State != "open" {
		st = "closed"
	}
	return map[string]any{"number": p.Number, "html_url": h.PRURL(p.Number), "state": st, "merged": p.State == "merged",
		"head": map[string]any{"ref": p.Head, "repo": map[string]any{"full_name": repo}}, "base": map[string]any{"ref": p.Base}}
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	h.mu.Lock()
	git := h.git
	if git != nil && strings.HasPrefix(path, "/"+h.Remote+".git/") {
		u, p, _ := r.BasicAuth()
		h.gitSeen = append(h.gitSeen, r.URL.Path+" "+u+":"+p)
		ok := u == h.gitAuth[0] && p == h.gitAuth[1]
		h.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "HTTP Basic: Access denied", http.StatusUnauthorized)
			return
		}
		git.ServeHTTP(w, r)
		return
	}
	defer h.mu.Unlock()
	h.requests = append(h.requests, r.Method+" "+path)
	if r.Header.Get("Authorization") != h.Auth {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "401 Unauthorized"})
		return
	}
	if !strings.HasPrefix(path, h.prefix) {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Project Not Found"})
		return
	}
	rest := strings.TrimPrefix(path, h.prefix)
	var in map[string]any
	if body, _ := io.ReadAll(r.Body); len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			return
		}
	}
	str := func(keys ...string) string {
		var v any = in
		for _, k := range keys {
			m, _ := v.(map[string]any)
			v = m[k]
		}
		s, _ := v.(string)
		return s
	}
	switch {
	case r.Method == http.MethodGet && rest == "":
		h.list(w, r)
	case r.Method == http.MethodPost && rest == "":
		p := &PR{Number: 100 + len(h.prs) + 1, State: "open"}
		switch h.Kind {
		case store.ProviderGitLab:
			p.Head, p.Base, p.Title, p.Body = str("source_branch"), str("target_branch"), str("title"), str("description")
		case store.ProviderBitbucket:
			p.Head, p.Base, p.Title, p.Body = str("source", "branch", "name"), str("destination", "branch", "name"), str("title"), str("description")
			p.Draft, _ = in["draft"].(bool)
		default:
			p.Head, p.Base, p.Title, p.Body = str("head"), str("base"), str("title"), str("body")
		}
		for _, o := range h.prs {
			if o.Head == p.Head && o.State == "open" && !o.Fork {
				writeJSON(w, http.StatusConflict, map[string]any{"message": "Another open merge request already exists for this source branch"})
				return
			}
		}
		h.created++
		h.prs = append(h.prs, p)
		writeJSON(w, http.StatusCreated, h.render(p))
	case strings.HasPrefix(rest, "/"):
		n, _ := strconv.Atoi(rest[1:])
		var p *PR
		for _, o := range h.prs {
			if o.Number == n {
				p = o
			}
		}
		if p == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not found"})
			return
		}
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, h.render(p))
			return
		}
		h.updated++
		switch h.Kind {
		case store.ProviderGitLab:
			p.Base, p.Title, p.Body = str("target_branch"), str("title"), str("description")
		case store.ProviderBitbucket:
			p.Base, p.Title, p.Body = str("destination", "branch", "name"), str("title"), str("description")
		default:
			p.Base, p.Title, p.Body = str("base"), str("title"), str("body")
		}
		status := http.StatusOK
		if h.Kind == store.ProviderGitea || h.Kind == store.ProviderForgejo {
			status = http.StatusCreated // as Gitea answers an edit
		}
		writeJSON(w, status, h.render(p))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "405 Method Not Allowed"})
	}
}

func (h *Host) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out := []map[string]any{}
	for i := len(h.prs) - 1; i >= 0; i-- { // newest first
		p := h.prs[i]
		switch h.Kind {
		case store.ProviderGitLab:
			if q.Get("state") != "opened" || q.Get("source_branch") == "" {
				h.t.Errorf("hosttest: GitLab merge requests listed without filters: %s", r.URL.RawQuery)
			}
			if p.State != "open" || p.Head != q.Get("source_branch") {
				continue
			}
		case store.ProviderBitbucket:
			if !strings.Contains(q.Get("q"), `source.branch.name = "`+p.Head+`"`) || p.State != "open" {
				continue
			}
		default:
			if q.Get("state") == "open" && p.State != "open" {
				continue
			}
		}
		out = append(out, h.render(p))
	}
	if h.Kind == store.ProviderBitbucket {
		writeJSON(w, http.StatusOK, map[string]any{"values": out})
		return
	}
	writeJSON(w, http.StatusOK, out)
}
