// Package web is the vx web UI: efforts, who is working on what, the
// timeline of lands with their evidence, diffs of each change, and the
// shipping view. It is server-rendered HTML with a little JavaScript for
// live updates, and needs nothing from the internet.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/web/highlight"
)

//go:embed templates/*.html static/*
var assets embed.FS

// cookieBase is the start of the session cookie's name. Browsers share
// cookies between all ports of a host, so the name also carries the port:
// two vx servers on one machine then keep separate sessions.
const cookieBase = "vx_token"

// cookieName is the session cookie's name for the server r was sent to:
// vx_token_7070 for localhost:7070, vx_token when the host has no port.
func cookieName(r *http.Request) string {
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil || port == "" {
		return cookieBase
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return cookieBase
		}
	}
	return cookieBase + "_" + port
}

// secure reports whether the browser reached vx over HTTPS, directly or
// through a proxy that says so. Marking a cookie Secure by mistake only
// keeps it off plain HTTP, so the proxy header needs no trust.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSession(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName(r), Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: secure(r), MaxAge: 30 * 24 * 3600,
	})
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName(r), Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r),
	})
}

// sameOrigin reports why a state-changing form post must be refused, or ""
// when it came from a page of this server. It trusts the Origin header and
// falls back to Referer when a browser sends no Origin; a post with
// neither is refused, since no browser form sends that. Pages with forms
// must not use Referrer-Policy no-referrer: browsers then send
// "Origin: null", which is refused.
func sameOrigin(r *http.Request) string {
	src, header := r.Header.Get("Origin"), "Origin"
	if src == "" {
		src, header = r.Header.Get("Referer"), "Referer"
	}
	if src == "" {
		return "the request has no Origin or Referer header, so vx cannot tell it came from this site. Submit the form from the vx page again."
	}
	u, err := url.Parse(src)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return header + " " + strconv.Quote(src) + " is not a page of this site."
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return "cross-origin request refused: " + header + " is " + u.Scheme + "://" + u.Host + ", this site is " + r.Host + "."
	}
	return ""
}

// checkOrigin writes a 403 and returns false when sameOrigin refuses r.
func (u *UI) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	why := sameOrigin(r)
	if why == "" {
		return true
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = u.pages["login"].ExecuteTemplate(w, "error", map[string]any{"Status": http.StatusForbidden, "Message": why})
	return false
}

// UI serves the web interface.
type UI struct {
	svc   *service.Service
	log   *slog.Logger
	pages map[string]*template.Template
}

// Mount registers the UI routes on mux.
func Mount(mux *http.ServeMux, svc *service.Service, log *slog.Logger) error {
	u := &UI{svc: svc, log: log, pages: map[string]*template.Template{}}
	funcs := template.FuncMap{
		"short":     shortHash,
		"ago":       ago,
		"first":     firstLine,
		"rest":      restLines,
		"q":         url.QueryEscape,
		"principal": principalName,
		"pct":       func(a, b int) int { return pct(a, b) },
		"color":     func(m map[string]int, k string) int { return m[k] },
		"blocks":    diffBlocks,
		"maxLines": func(as []*agentStat) int {
			m := 0
			for _, a := range as {
				if n := a.Added + a.Deleted; n > m {
					m = n
				}
			}
			return m
		},
	}
	frag, err := template.New("partials.html").Funcs(funcs).ParseFS(assets, "templates/partials.html")
	if err != nil {
		return fmt.Errorf("template partials: %w", err)
	}
	u.pages["partials"] = frag
	for _, page := range []string{"login", "code", "home", "repo", "effort", "change", "ship", "settings", "revoke", "token"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/"+page+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", page, err)
		}
		u.pages[page] = t
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return err
	}
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui", http.StatusFound) })
	mux.HandleFunc("GET /ui/login", u.loginPage)
	mux.HandleFunc("POST /ui/login", u.loginPost)
	mux.HandleFunc("GET /ui/login/code", u.codePage)
	mux.HandleFunc("POST /ui/login/code", u.codePost)
	mux.HandleFunc("POST /ui/logout", u.logout)
	mux.HandleFunc("GET /ui", u.auth(u.home))
	mux.HandleFunc("GET /ui/repo", u.auth(u.repo))
	mux.HandleFunc("GET /ui/effort", u.auth(u.effort))
	mux.HandleFunc("GET /ui/effort/head", u.auth(u.effortHead))
	mux.HandleFunc("POST /ui/board/answer", u.auth(u.answerPost))
	mux.HandleFunc("GET /ui/change", u.auth(u.change))
	mux.HandleFunc("GET /ui/diff", u.auth(u.diff))
	mux.HandleFunc("GET /ui/ship", u.auth(u.ship))
	mux.HandleFunc("POST /ui/ship", u.auth(u.shipPost))
	mux.HandleFunc("GET /ui/settings", u.auth(u.settings))
	mux.HandleFunc("POST /ui/settings/revoke", u.auth(u.revokePost))
	mux.HandleFunc("POST /ui/settings/token", u.auth(u.createTokenPost))
	return nil
}

type handler func(w http.ResponseWriter, r *http.Request, actor service.Actor)

func (u *UI) auth(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName(r))
		if err != nil {
			http.Redirect(w, r, "/ui/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		p, tokenID, err := u.svc.Store.AuthenticateToken(r.Context(), c.Value)
		if err != nil {
			clearSession(w, r)
			http.Redirect(w, r, "/ui/login?expired=1", http.StatusFound)
			return
		}
		actor, err := u.svc.ResolveActor(r.Context(), p)
		if err != nil {
			u.fail(w, err)
			return
		}
		actor.TokenID = tokenID
		next(w, r, actor)
	}
}

// navRepo and navEffort feed the sidebar on every signed-in page.
type navRepo struct {
	Name     string
	Provider string
	Efforts  []navEffort
}

type navEffort struct {
	Slug  string
	Name  string
	State string
	Open  int
}

func (u *UI) nav(r *http.Request) []navRepo {
	ctx := r.Context()
	repos, err := u.svc.Store.ListRepos(ctx)
	if err != nil {
		return nil
	}
	out := make([]navRepo, 0, len(repos))
	for _, repo := range repos {
		nr := navRepo{Name: repo.Name, Provider: string(repo.Provider)}
		effs, _ := u.svc.Store.ListEfforts(ctx, repo.ID, "")
		for _, e := range effs {
			ne := navEffort{Slug: e.Slug, Name: e.Name, State: string(e.State)}
			if ws, err := u.svc.Store.ListWorkspaces(ctx, e.ID, store.WorkspaceOpen); err == nil {
				ne.Open = len(ws)
			}
			nr.Efforts = append(nr.Efforts, ne)
		}
		out = append(out, nr)
	}
	return out
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	if _, signedIn := data["Actor"]; signedIn {
		data["Nav"] = u.nav(r)
		if _, ok := data["ActiveRepo"]; !ok {
			data["ActiveRepo"] = param(r, "repo")
			if data["ActiveRepo"] == "" {
				data["ActiveRepo"] = param(r, "name")
			}
		}
		data["ActiveSlug"] = param(r, "slug")
		data["Path"] = r.URL.Path
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:")
	w.Header().Set("X-Frame-Options", "DENY")
	// Links to other sites (GitHub, docs) do not carry vx URLs with repo and
	// effort names. same-origin, not no-referrer: form posts must keep their
	// Origin header for checkOrigin.
	if w.Header().Get("Referrer-Policy") == "" {
		w.Header().Set("Referrer-Policy", "same-origin")
	}
	if err := u.pages[page].Execute(w, data); err != nil {
		u.log.Error("render", "page", page, "err", err)
	}
}

func (u *UI) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var se *service.Error
	if errors.As(service.Classify(err), &se) {
		switch se.Code {
		case service.CodeNotFound:
			status = http.StatusNotFound
		case service.CodeInvalid, service.CodeFailedPrecondition:
			status = http.StatusBadRequest
		case service.CodePermission:
			status = http.StatusForbidden
		}
	}
	if status == http.StatusInternalServerError {
		u.log.Error("web", "err", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = u.pages["login"].ExecuteTemplate(w, "error", map[string]any{"Status": status, "Message": err.Error()})
}

// ---------- login ----------

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "login", map[string]any{"Title": "Sign in", "Next": r.URL.Query().Get("next"), "Expired": r.URL.Query().Get("expired") != ""})
}

func (u *UI) loginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	token := strings.TrimSpace(r.PostForm.Get("token"))
	next := r.PostForm.Get("next")
	if !strings.HasPrefix(next, "/ui") {
		next = "/ui"
	}
	if _, err := u.svc.Store.Authenticate(r.Context(), token); err != nil {
		u.render(w, r, "login", map[string]any{"Title": "Sign in", "Next": next, "Error": "That token is not valid. Use a token from vx-server user-add or vx agent add."})
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	if !u.checkOrigin(w, r) {
		return
	}
	clearSession(w, r)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// ---------- home and repo ----------

type effortRow struct {
	store.Effort
	Repo    string
	Owner   string
	Open    int
	Changes int
	Agents  int
}

func (u *UI) home(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	repos, err := u.svc.Store.ListRepos(ctx)
	if err != nil {
		u.fail(w, err)
		return
	}
	type repoRow struct {
		store.Repo
		Efforts []effortRow
	}
	var rows []repoRow
	for _, repo := range repos {
		rr := repoRow{Repo: repo}
		effs, err := u.svc.Store.ListEfforts(ctx, repo.ID, "")
		if err != nil {
			u.fail(w, err)
			return
		}
		for _, e := range effs {
			rr.Efforts = append(rr.Efforts, u.effortRow(r, repo, e))
		}
		rows = append(rows, rr)
	}
	var all []effortRow
	working, changes := 0, 0
	for _, rr := range rows {
		for _, e := range rr.Efforts {
			all = append(all, e)
			working += e.Open
			changes += e.Changes
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
	u.render(w, r, "home", map[string]any{"Actor": actor, "Repos": rows, "All": all, "Working": working, "Changes": changes, "Title": "Efforts"})
}

func (u *UI) effortRow(r *http.Request, repo store.Repo, e store.Effort) effortRow {
	ctx := r.Context()
	row := effortRow{Effort: e, Repo: repo.Name, Owner: u.ref(r, e.OwnerID)}
	if ws, err := u.svc.Store.ListWorkspaces(ctx, e.ID, store.WorkspaceOpen); err == nil {
		row.Open = len(ws)
	}
	if chain, err := u.svc.Store.Chain(ctx, repo.ID, e.Head, e.Base, 1000); err == nil {
		row.Changes = len(chain)
		agents := map[string]bool{}
		for _, c := range chain {
			agents[c.Author.Principal] = true
		}
		row.Agents = len(agents)
	}
	return row
}

func (u *UI) ref(r *http.Request, id uuid.UUID) string {
	p, err := u.svc.Store.PrincipalByID(r.Context(), id)
	if err != nil {
		return ""
	}
	return p.Ref()
}

func (u *UI) repo(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	repo, err := u.svc.Store.RepoByName(ctx, param(r, "name"))
	if err != nil {
		u.fail(w, err)
		return
	}
	branches, err := u.svc.Store.ListBranches(ctx, repo.ID)
	if err != nil {
		u.fail(w, err)
		return
	}
	effs, err := u.svc.Store.ListEfforts(ctx, repo.ID, "")
	if err != nil {
		u.fail(w, err)
		return
	}
	var rows []effortRow
	for _, e := range effs {
		rows = append(rows, u.effortRow(r, repo, e))
	}
	u.render(w, r, "repo", map[string]any{"Actor": actor, "Repo": repo, "Branches": branches, "Efforts": rows, "Title": repo.Name})
}

// ---------- effort ----------

type landRow struct {
	service.LogEntry
	Added, Deleted int
	EffortSlug     string
	Repo           string
	Color          int
	VerifiedByVX   bool
}

type agentStat struct {
	Color      int
	Principal  string
	OnBehalfOf string
	Lands      int
	Added      int
	Deleted    int
	Open       []string
}

func (u *UI) effort(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	repoName, slug := param(r, "repo"), param(r, "slug")
	v, err := u.svc.GetEffort(ctx, repoName, slug)
	if err != nil {
		u.fail(w, err)
		return
	}
	entries, err := u.svc.Log(ctx, repoName, slug, 500, true)
	if err != nil {
		u.fail(w, err)
		return
	}
	stats := map[string]*agentStat{}
	stat := func(principal, behalf string) *agentStat {
		s, ok := stats[principal]
		if !ok {
			s = &agentStat{Principal: principal, OnBehalfOf: behalf}
			stats[principal] = s
		}
		return s
	}
	var lands []landRow
	var totalAdded, totalDeleted, merged, verified int
	for _, e := range entries {
		row := landRow{LogEntry: e, EffortSlug: slug, Repo: repoName}
		for _, f := range e.Files {
			row.Added += f.Added
			row.Deleted += f.Deleted
		}
		s := stat(e.Author.Principal, e.Author.OnBehalfOf)
		s.Lands++
		s.Added += row.Added
		s.Deleted += row.Deleted
		totalAdded += row.Added
		totalDeleted += row.Deleted
		if e.Merged() {
			merged++
		}
		if e.Evidence != nil {
			for _, ch := range e.Evidence.Checks {
				if ch.RanByVX {
					verified++
					row.VerifiedByVX = true
					break
				}
			}
		}
		lands = append(lands, row)
	}
	type workRow struct {
		store.Workspace
		Owner string
	}
	var working []workRow
	for _, ws := range v.Workspaces {
		owner := u.ref(r, ws.OwnerID)
		working = append(working, workRow{Workspace: ws, Owner: owner})
		s := stat(owner, "")
		s.Open = append(s.Open, ws.Intent)
	}
	var agents []*agentStat
	for _, s := range stats {
		agents = append(agents, s)
	}
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Lands != agents[j].Lands {
			return agents[i].Lands > agents[j].Lands
		}
		return agents[i].Principal < agents[j].Principal
	})
	// Each agent keeps one color across the page, assigned by name so it is
	// stable between reloads.
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Principal)
	}
	sort.Strings(names)
	color := map[string]int{}
	for i, n := range names {
		color[n] = i%6 + 1
	}
	for _, a := range agents {
		a.Color = color[a.Principal]
	}
	for i := range lands {
		lands[i].Color = color[lands[i].Author.Principal]
	}
	workColor := map[string]int{}
	for _, wr := range working {
		workColor[wr.Owner] = color[wr.Owner]
	}
	claims := u.claimRows(r, actor, repoName, slug, color)
	shipments, _ := u.svc.Store.ListShipments(ctx, v.Effort.ID)
	baseRow, _ := u.svc.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Base)
	changesView := param(r, "view") == "changes"
	boardView := param(r, "view") == "board"
	board := u.board(r, actor, repoName, slug, boardView)
	var allFiles []changedFile
	if changesView {
		if allFiles, err = u.allChanges(r, v.Repo, v.Effort, entries, color); err != nil {
			u.fail(w, err)
			return
		}
	}
	u.render(w, r, "effort", map[string]any{
		"ChangesView": changesView, "AllFiles": allFiles, "BoardView": boardView, "Board": board,
		"Actor": actor, "Repo": v.Repo, "Effort": v.Effort, "Owner": u.ref(r, v.Effort.OwnerID),
		"Working": working, "WorkColor": workColor, "Lands": lands, "Agents": agents, "Shipments": shipments,
		"Added": totalAdded, "Deleted": totalDeleted, "Merged": merged, "Verified": verified,
		"BaseGit": baseRow.GitSHA, "Title": v.Effort.Name,
		"Claims": claims,
	})
}

// claimRow is an active claim in the effort page's Working now panel.
type claimRow struct {
	service.Claim
	Owner     string // principal ref
	Trust     string // self, yours or teammate, for the viewer
	ExpiresIn string
	Color     int
}

// claimRows lists the effort's active claims for the viewer. Claims are
// extra information: when they cannot be read the panel just has none.
func (u *UI) claimRows(r *http.Request, actor service.Actor, repo, slug string, color map[string]int) []claimRow {
	cs, err := u.svc.ListClaims(r.Context(), actor, repo, slug, nil)
	if err != nil {
		return nil
	}
	now := u.svc.Now()
	out := make([]claimRow, 0, len(cs))
	for _, c := range cs {
		owner := c.Owner.Ref()
		out = append(out, claimRow{Claim: c, Owner: owner, Trust: service.ClaimTrust(actor, c), ExpiresIn: untilText(c.ExpiresAt.Sub(now)), Color: color[owner]})
	}
	return out
}

// untilText says how long until something ends: "ends in 25 min".
func untilText(d time.Duration) string {
	if d >= time.Minute {
		d = d.Round(time.Minute)
	}
	switch {
	case d <= 0:
		return "ended"
	case d < time.Minute:
		return "ends in under a minute"
	case d < time.Hour:
		return fmt.Sprintf("ends in %d min", int(d.Minutes()))
	}
	return fmt.Sprintf("ends in %dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// changedFile is one file of the effort's combined diff.
type changedFile struct {
	store.FileChange
	Agents []*agentStat // who touched it, in landing order
}

// allChanges is the effort's combined diff from its base to its head.
func (u *UI) allChanges(r *http.Request, repo store.Repo, e store.Effort, entries []service.LogEntry, color map[string]int) ([]changedFile, error) {
	ctx := r.Context()
	base, err := u.svc.Store.GetChangeset(ctx, repo.ID, e.Base)
	if err != nil {
		return nil, err
	}
	head, err := u.svc.Store.GetChangeset(ctx, repo.ID, e.Head)
	if err != nil {
		return nil, err
	}
	changes, err := object.DiffTrees(ctx, u.svc.Blobs, base.Tree, head.Tree)
	if err != nil {
		return nil, err
	}
	touched := map[string][]*agentStat{}
	seen := map[string]bool{}
	for i := len(entries) - 1; i >= 0; i-- {
		for _, f := range entries[i].Files {
			who := entries[i].Author.Principal
			if seen[f.Path+"\x00"+who] {
				continue
			}
			seen[f.Path+"\x00"+who] = true
			touched[f.Path] = append(touched[f.Path], &agentStat{Principal: who, Color: color[who]})
		}
	}
	out := make([]changedFile, 0, len(changes))
	for _, c := range changes {
		cf := changedFile{FileChange: store.FileChange{Change: c}, Agents: touched[c.Path]}
		if len(changes) <= 300 {
			a, _ := u.readBlob(r, c.Old.Hash)
			b, _ := u.readBlob(r, c.New.Hash)
			cf.Added, cf.Deleted = merge.LineStats(a, b)
		}
		out = append(out, cf)
	}
	return out, nil
}

func (u *UI) readBlob(r *http.Request, h hash.Hash) ([]byte, error) {
	if h.IsZero() {
		return nil, nil
	}
	return blob.GetKind(r.Context(), u.svc.Blobs, hash.KindBlob, h)
}

// diff renders one file's diff as an HTML fragment, either for one change
// (ref) or for the effort as a whole (slug).
func (u *UI) diff(w http.ResponseWriter, r *http.Request, _ service.Actor) {
	ctx := r.Context()
	repoName, path := param(r, "repo"), param(r, "path")
	var fc store.FileChange
	switch {
	case param(r, "ref") != "":
		_, e, err := u.svc.GetChangeset(ctx, repoName, param(r, "ref"))
		if err != nil {
			u.fail(w, err)
			return
		}
		found := false
		for _, f := range e.Files {
			if f.Path == path {
				fc, found = f, true
				break
			}
		}
		if !found {
			http.Error(w, "file not in this change", http.StatusNotFound)
			return
		}
	default:
		v, err := u.svc.GetEffort(ctx, repoName, param(r, "slug"))
		if err != nil {
			u.fail(w, err)
			return
		}
		base, err := u.svc.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Base)
		if err != nil {
			u.fail(w, err)
			return
		}
		head, err := u.svc.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Head)
		if err != nil {
			u.fail(w, err)
			return
		}
		old, okOld, err := object.Lookup(ctx, u.svc.Blobs, base.Tree, path)
		if err != nil {
			u.fail(w, err)
			return
		}
		cur, okNew, err := object.Lookup(ctx, u.svc.Blobs, head.Tree, path)
		if err != nil {
			u.fail(w, err)
			return
		}
		fc.Path = path
		switch {
		case okOld && okNew:
			fc.Op = object.OpModify
		case okNew:
			fc.Op = object.OpAdd
		case okOld:
			fc.Op = object.OpDelete
		default:
			http.Error(w, "file not changed in this effort", http.StatusNotFound)
			return
		}
		if okOld {
			fc.Old = object.File{Hash: old.Hash, Mode: old.Mode}
		}
		if okNew {
			fc.New = object.File{Hash: cur.Hash, Mode: cur.Mode}
		}
	}
	fd, err := u.fileDiff(r, fc)
	if err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=300")
	if err := u.pages["partials"].ExecuteTemplate(w, "diffbody", fd); err != nil {
		u.log.Error("render diff", "err", err)
	}
}

// diffBlocks gives the five GitHub-style squares for a file's change size.
func diffBlocks(added, deleted int) []string {
	total := added + deleted
	out := make([]string, 5)
	if total == 0 {
		for i := range out {
			out[i] = "n"
		}
		return out
	}
	adds := (added*5 + total/2) / total
	if added > 0 && adds == 0 {
		adds = 1
	}
	dels := 5 - adds
	if deleted == 0 {
		dels = 0
	} else if dels == 0 {
		dels, adds = 1, 4
	}
	for i := range out {
		switch {
		case i < adds:
			out[i] = "a"
		case i < adds+dels:
			out[i] = "d"
		default:
			out[i] = "n"
		}
	}
	return out
}

// effortHead lets the page poll cheaply for changes.
func (u *UI) effortHead(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	v, err := u.svc.GetEffort(r.Context(), param(r, "repo"), param(r, "slug"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	// Head plus open workspaces: either changing means the page is stale.
	ids := make([]string, 0, len(v.Workspaces))
	for _, ws := range v.Workspaces {
		ids = append(ids, ws.ID.String())
	}
	fmt.Fprintf(w, "%s %s", v.Effort.Head.String(), strings.Join(ids, ","))
	if param(r, "board") != "" {
		// The Board tab also goes stale when a note is posted or a question
		// is answered or escalates.
		if notes, err := u.svc.ListNotes(r.Context(), actor, service.ListNotesInput{Repo: param(r, "repo"), Effort: param(r, "slug")}); err == nil {
			for _, n := range notes {
				fmt.Fprintf(w, " %s:%s", n.ID.String()[:8], n.State)
			}
		}
	}
}

// ---------- board ----------

// boardNote is a note as the Board tab shows it.
type boardNote struct {
	service.Note
	Trust  string // relative to the viewer
	Author string
	For    string // the person an agent author acts for
	To     string // a question's addressee; empty: anyone
}

type boardThread struct {
	boardNote
	Replies   []boardNote // answers, oldest first
	CanAnswer bool        // an escalated question and the viewer is a person
}

// boardView is the Board tab: escalated questions first, then threads
// newest first.
type boardView struct {
	Threads   []boardThread
	Open      int // open questions, escalated ones included
	Escalated int
	Err       string
}

func (u *UI) board(r *http.Request, actor service.Actor, repoName, slug string, full bool) boardView {
	var bv boardView
	in := service.ListNotesInput{Repo: repoName, Effort: slug}
	if !full {
		in.Kinds = []service.NoteKind{service.NoteQuestion}
	}
	notes, err := u.svc.ListNotes(r.Context(), actor, in)
	if err != nil {
		bv.Err = err.Error()
		return bv
	}
	for _, n := range notes {
		if n.Kind != service.NoteQuestion {
			continue
		}
		switch n.State {
		case service.NoteOpen:
			bv.Open++
		case service.NoteEscalated:
			bv.Open++
			bv.Escalated++
		}
	}
	if !full {
		return bv
	}
	show := func(n service.Note) boardNote {
		bn := boardNote{Note: n, Trust: service.TrustOf(actor, n), Author: n.Author.Ref()}
		if n.AuthorOwner != nil {
			bn.For = n.AuthorOwner.Ref()
		}
		if n.To != nil {
			bn.To = n.To.Ref()
		}
		return bn
	}
	roots := map[uuid.UUID]bool{}
	for _, n := range notes {
		if n.ThreadID == n.ID || n.ThreadID == uuid.Nil {
			roots[n.ID] = true
		}
	}
	replies := map[uuid.UUID][]boardNote{}
	for i := len(notes) - 1; i >= 0; i-- {
		if n := notes[i]; n.ThreadID != n.ID && roots[n.ThreadID] {
			replies[n.ThreadID] = append(replies[n.ThreadID], show(n))
		}
	}
	for _, n := range notes {
		if n.ThreadID != n.ID && n.ThreadID != uuid.Nil && roots[n.ThreadID] {
			continue
		}
		bv.Threads = append(bv.Threads, boardThread{
			boardNote: show(n), Replies: replies[n.ID],
			CanAnswer: n.Kind == service.NoteQuestion && n.State == service.NoteEscalated && actor.Kind == store.KindHuman,
		})
	}
	sort.SliceStable(bv.Threads, func(i, j int) bool {
		return bv.Threads[i].State == service.NoteEscalated && bv.Threads[j].State != service.NoteEscalated
	})
	return bv
}

// answerPost answers an escalated question from the Board tab.
func (u *UI) answerPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	if actor.Kind != store.KindHuman {
		u.fail(w, &service.Error{Code: service.CodePermission, Msg: "only people answer questions in the web UI; agents answer with vx answer"})
		return
	}
	_, q, err := u.svc.AnswerNote(r.Context(), actor, r.PostForm.Get("note"), r.PostForm.Get("body"))
	if err != nil {
		u.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/effort?repo="+url.QueryEscape(q.RepoName)+"&slug="+url.QueryEscape(q.EffortSlug)+"&view=board#n-"+q.ID.String(), http.StatusSeeOther)
}

// ---------- change ----------

// seg is a piece of a diff line: its syntax class, and whether it is the
// changed part of a line that was edited (Hi).
type seg = highlight.Span

type diffLine struct {
	Kind string // ctx, add, del, hunk
	Text string
	Old  int
	New  int
	Segs []seg // set for edited lines; otherwise Text is shown whole
}

// splitRow is one row of the side-by-side view.
type splitRow struct {
	Kind        string // ctx, chg, hunk
	Left, Right *diffLine
	Text        string // hunk header
}

type fileDiff struct {
	store.FileChange
	Binary bool
	Lines  []diffLine
	Rows   []splitRow
	Large  bool
	Key    string // identifies this file version for the Viewed checkbox
}

func (u *UI) change(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	repoName, ref := param(r, "repo"), param(r, "ref")
	repo, e, err := u.svc.GetChangeset(ctx, repoName, ref)
	if err != nil {
		u.fail(w, err)
		return
	}
	var effort *store.Effort
	if e.EffortID != nil {
		if eff, err := u.svc.Store.EffortByID(ctx, *e.EffortID); err == nil {
			effort = &eff
		}
	}
	var diffs []fileDiff
	var added, deleted int
	for _, f := range e.Files {
		fd, err := u.fileDiff(r, f)
		if err != nil {
			u.fail(w, err)
			return
		}
		added += f.Added
		deleted += f.Deleted
		diffs = append(diffs, fd)
	}
	u.render(w, r, "change", map[string]any{
		"Actor": actor, "Repo": repo, "C": e, "Effort": effort, "Diffs": diffs,
		"Added": added, "Deleted": deleted, "Title": e.Title(),
	})
}

func (u *UI) fileDiff(r *http.Request, f store.FileChange) (fileDiff, error) {
	fd := fileDiff{FileChange: f}
	read := func(h hash.Hash) ([]byte, error) {
		if h.IsZero() {
			return nil, nil
		}
		return blob.GetKind(r.Context(), u.svc.Blobs, hash.KindBlob, h)
	}
	a, err := read(f.Old.Hash)
	if err != nil {
		return fd, err
	}
	b, err := read(f.New.Hash)
	if err != nil {
		return fd, err
	}
	if merge.IsBinary(a) || merge.IsBinary(b) {
		fd.Binary = true
		return fd, nil
	}
	if len(a)+len(b) > 2<<20 {
		fd.Large = true
		return fd, nil
	}
	la, lb := merge.SplitLines(a), merge.SplitLines(b)
	edits := merge.Diff(la, lb)
	fd.Lines = renderHunks(edits, la, lb, 3)
	markWords(fd.Lines)
	colorLines(fd.Lines, f.Path, a, b)
	fd.Rows = splitRows(fd.Lines)
	fd.Key = f.Path + "@" + f.New.Hash.Short()
	return fd, nil
}

// markWords pairs each run of deleted lines with the run of added lines
// after it and highlights the part of each pair that differs, the way code
// review tools show an edit inside a line.
func markWords(lines []diffLine) {
	for i := 0; i < len(lines); {
		if lines[i].Kind != "del" {
			i++
			continue
		}
		d0 := i
		for i < len(lines) && lines[i].Kind == "del" {
			i++
		}
		a0 := i
		for i < len(lines) && lines[i].Kind == "add" {
			i++
		}
		n := min(a0-d0, i-a0)
		for k := 0; k < n; k++ {
			del, add := &lines[d0+k], &lines[a0+k]
			if len(del.Text) > 1000 || len(add.Text) > 1000 {
				continue
			}
			del.Segs, add.Segs = wordSegs(del.Text, add.Text)
		}
	}
}

// colorLines adds syntax colours to the lines of a diff, keeping the
// word-level change marks. Old and new files are each tokenised whole, up
// to the last line the diff shows, so multi-line strings and comments
// colour right; lines without tokens stay plain.
func colorLines(lines []diffLine, path string, a, b []byte) {
	lastOld, lastNew := 0, 0
	for _, l := range lines {
		if l.Kind == "del" {
			lastOld = max(lastOld, l.Old)
		}
		lastNew = max(lastNew, l.New)
	}
	var ta, tb [][]highlight.Token
	if lastOld > 0 {
		ta = highlight.File(path, a, lastOld)
	}
	if lastNew > 0 {
		tb = highlight.File(path, b, lastNew)
	}
	for i := range lines {
		l := &lines[i]
		toks, n := tb, l.New
		switch l.Kind {
		case "del":
			toks, n = ta, l.Old
		case "hunk":
			continue
		}
		if n < 1 || n > len(toks) {
			continue
		}
		if spans := highlight.Overlay(toks[n-1], l.Text, l.Segs); spans != nil {
			l.Segs = spans
		}
	}
}

// wordSegs splits two lines into a common prefix, the changed middle and a
// common suffix. Lines with nothing in common are left unhighlighted.
func wordSegs(a, b string) ([]seg, []seg) {
	ra, rb := []rune(a), []rune(b)
	p := 0
	for p < len(ra) && p < len(rb) && ra[p] == rb[p] {
		p++
	}
	q := 0
	for q < len(ra)-p && q < len(rb)-p && ra[len(ra)-1-q] == rb[len(rb)-1-q] {
		q++
	}
	if p+q == 0 {
		return nil, nil
	}
	mk := func(r []rune) []seg {
		out := []seg{{Text: string(r[:p])}}
		if mid := string(r[p : len(r)-q]); mid != "" {
			out = append(out, seg{Text: mid, Hi: true})
		}
		return append(out, seg{Text: string(r[len(r)-q:])})
	}
	return mk(ra), mk(rb)
}

// splitRows lays unified lines side by side: context on both sides, runs of
// deletions and additions paired row by row.
func splitRows(lines []diffLine) []splitRow {
	var rows []splitRow
	for i := 0; i < len(lines); {
		l := &lines[i]
		switch l.Kind {
		case "hunk":
			rows = append(rows, splitRow{Kind: "hunk", Text: l.Text})
			i++
		case "ctx":
			rows = append(rows, splitRow{Kind: "ctx", Left: l, Right: l})
			i++
		default:
			d0 := i
			for i < len(lines) && lines[i].Kind == "del" {
				i++
			}
			a0 := i
			for i < len(lines) && lines[i].Kind == "add" {
				i++
			}
			dels, adds := lines[d0:a0], lines[a0:i]
			for k := 0; k < max(len(dels), len(adds)); k++ {
				r := splitRow{Kind: "chg"}
				if k < len(dels) {
					r.Left = &dels[k]
				}
				if k < len(adds) {
					r.Right = &adds[k]
				}
				rows = append(rows, r)
			}
		}
	}
	return rows
}

// renderHunks lays out a diff as lines with numbers, collapsing long
// unchanged stretches.
func renderHunks(edits []merge.Edit, a, b []string, context int) []diffLine {
	var out []diffLine
	trim := func(s string) string { return strings.TrimSuffix(s, "\n") }
	for i, e := range edits {
		switch e.Kind {
		case merge.Delete:
			for k := e.A0; k < e.A1; k++ {
				out = append(out, diffLine{Kind: "del", Text: trim(a[k]), Old: k + 1})
			}
		case merge.Insert:
			for k := e.B0; k < e.B1; k++ {
				out = append(out, diffLine{Kind: "add", Text: trim(b[k]), New: k + 1})
			}
		case merge.Equal:
			n := e.A1 - e.A0
			first, last := i == 0, i == len(edits)-1
			keepHead, keepTail := context, context
			if first {
				keepHead = 0
			}
			if last {
				keepTail = 0
			}
			if n <= keepHead+keepTail+1 {
				for k := 0; k < n; k++ {
					out = append(out, diffLine{Kind: "ctx", Text: trim(a[e.A0+k]), Old: e.A0 + k + 1, New: e.B0 + k + 1})
				}
				continue
			}
			for k := 0; k < keepHead; k++ {
				out = append(out, diffLine{Kind: "ctx", Text: trim(a[e.A0+k]), Old: e.A0 + k + 1, New: e.B0 + k + 1})
			}
			skipped := n - keepHead - keepTail
			text := fmt.Sprintf("@@ -%d +%d @@  %d unchanged lines", e.A0+n-keepTail+1, e.B0+n-keepTail+1, skipped)
			if last {
				text = fmt.Sprintf("%d unchanged lines to the end of the file", skipped)
			}
			out = append(out, diffLine{Kind: "hunk", Text: text})
			for k := n - keepTail; k < n; k++ {
				out = append(out, diffLine{Kind: "ctx", Text: trim(a[e.A0+k]), Old: e.A0 + k + 1, New: e.B0 + k + 1})
			}
		}
	}
	return out
}

// ---------- ship ----------

func (u *UI) ship(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	repoName, slug, target := param(r, "repo"), param(r, "slug"), param(r, "to")
	v, err := u.svc.GetEffort(ctx, repoName, slug)
	if err != nil {
		u.fail(w, err)
		return
	}
	data := map[string]any{"Actor": actor, "Repo": v.Repo, "Effort": v.Effort, "Title": "Ship " + v.Effort.Name}
	plan, err := u.svc.PlanShipment(ctx, actor, repoName, slug, target)
	if err != nil {
		data["PlanError"] = err.Error()
	} else {
		data["Plan"] = plan
		data["Commits"] = len(plan.Commits())
	}
	data["Shipments"], _ = u.svc.Store.ListShipments(ctx, v.Effort.ID)
	data["Shipped"] = r.URL.Query().Get("shipped") != ""
	u.render(w, r, "ship", data)
}

func (u *UI) shipPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	repoName, slug := r.PostForm.Get("repo"), r.PostForm.Get("slug")
	_, err := u.svc.OpenShipment(r.Context(), actor, service.ShipInput{
		Repo: repoName, Effort: slug, Target: r.PostForm.Get("to"),
		Draft: r.PostForm.Get("draft") == "on",
	})
	if err != nil {
		u.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/ship?repo="+url.QueryEscape(repoName)+"&slug="+url.QueryEscape(slug)+"&shipped=1", http.StatusSeeOther)
}

// ---------- settings ----------

// tokenRow is a token as the settings page shows it.
type tokenRow struct {
	store.Token
	Short   string
	Current bool // the token this browser is signed in with
}

type agentPanel struct {
	Agent           store.Principal
	Name            string
	Tokens          []tokenRow
	Active, Revoked int
}

func tokenRows(tokens []store.Token, current uuid.UUID) []tokenRow {
	out := make([]tokenRow, len(tokens))
	for i, t := range tokens {
		out[i] = tokenRow{Token: t, Short: t.ID.String()[:service.MinTokenPrefix], Current: current != uuid.Nil && t.ID == current}
	}
	return out
}

func (u *UI) settings(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	_, own, err := u.svc.ListTokens(ctx, actor, "")
	if err != nil {
		u.fail(w, err)
		return
	}
	data := map[string]any{"Actor": actor, "Title": "Settings", "Own": tokenRows(own, actor.TokenID), "IsHuman": actor.Kind == store.KindHuman}
	if actor.Kind == store.KindHuman {
		agents, err := u.svc.Store.ListAgents(ctx, actor.ID)
		if err != nil {
			u.fail(w, err)
			return
		}
		panels := make([]agentPanel, 0, len(agents))
		for _, a := range agents {
			_, toks, err := u.svc.ListTokens(ctx, actor, a.Handle)
			if err != nil {
				u.fail(w, err)
				return
			}
			p := agentPanel{Agent: a, Name: a.Handle[strings.LastIndex(a.Handle, "/")+1:], Tokens: tokenRows(toks, actor.TokenID)}
			for _, t := range toks {
				if t.Active() {
					p.Active++
				} else {
					p.Revoked++
				}
			}
			panels = append(panels, p)
		}
		data["Agents"] = panels
	}
	if id, err := uuid.Parse(r.URL.Query().Get("revoked")); err == nil {
		// Only tokens the caller may manage are named back to them.
		if tok, owner, err := u.svc.Token(ctx, actor, id.String()); err == nil {
			data["Revoked"] = tokenRows([]store.Token{tok}, uuid.Nil)[0]
			data["RevokedOwner"] = owner.Ref()
		}
	}
	data["AlreadyRevoked"] = r.URL.Query().Get("already") != ""
	u.render(w, r, "settings", data)
}

// revokePost is a two-step form: without confirm=yes it shows what would be
// revoked and asks again; with it, it revokes.
func (u *UI) revokePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	id, err := uuid.Parse(r.PostForm.Get("id"))
	if err != nil {
		u.fail(w, &service.Error{Code: service.CodeInvalid, Msg: "no token id in the form; go back to Settings and use a Revoke button"})
		return
	}
	if r.PostForm.Get("confirm") != "yes" {
		tok, owner, err := u.svc.Token(r.Context(), actor, id.String())
		if err != nil {
			u.fail(w, err)
			return
		}
		if !tok.Active() {
			http.Redirect(w, r, "/ui/settings?already=1", http.StatusSeeOther)
			return
		}
		u.render(w, r, "revoke", map[string]any{"Actor": actor, "Title": "Revoke token",
			"Token": tokenRows([]store.Token{tok}, actor.TokenID)[0], "Owner": owner})
		return
	}
	tok, _, already, err := u.svc.RevokeToken(r.Context(), actor, service.RevokeInput{
		ID: id.String(), AllowCurrent: r.PostForm.Get("current") == "yes",
	})
	if err != nil {
		u.fail(w, err)
		return
	}
	if tok.ID == actor.TokenID {
		clearSession(w, r)
		http.Redirect(w, r, "/ui/login?expired=1", http.StatusSeeOther)
		return
	}
	if already {
		http.Redirect(w, r, "/ui/settings?already=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/settings?revoked="+tok.ID.String(), http.StatusSeeOther)
}

// createTokenPost is a two-step form like revokePost: without confirm=yes it
// asks for a label; with it, it creates the token and shows the secret on
// the response page, once. The secret never goes into a URL.
func (u *UI) createTokenPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	agent := strings.TrimSpace(r.PostForm.Get("agent"))
	if r.PostForm.Get("confirm") != "yes" {
		// Check the permission now, so the form is not shown in vain.
		owner, err := u.svc.TokenCreateTarget(r.Context(), actor, agent)
		if err != nil {
			u.fail(w, err)
			return
		}
		u.render(w, r, "token", map[string]any{"Actor": actor, "Title": "Create token", "Owner": owner, "Agent": agent})
		return
	}
	tok, owner, secret, err := u.svc.CreateToken(r.Context(), actor, agent, r.PostForm.Get("name"))
	if err != nil {
		u.fail(w, err)
		return
	}
	// The page holds a secret: keep it out of caches and history restores.
	w.Header().Set("Cache-Control", "no-store")
	u.render(w, r, "token", map[string]any{"Actor": actor, "Title": "New token", "Owner": owner,
		"Token": tokenRows([]store.Token{tok}, uuid.Nil)[0], "Secret": secret})
}

// ---------- helpers ----------

// param reads a query parameter. Names never contain "%", so a value that
// still has one was encoded twice (for example by a copied link) and is
// decoded once more.
func param(r *http.Request, key string) string {
	v := r.URL.Query().Get(key)
	if strings.Contains(v, "%") {
		if d, err := url.QueryUnescape(v); err == nil {
			return d
		}
	}
	return v
}

func shortHash(v any) string {
	var s string
	switch h := v.(type) {
	case hash.Hash:
		return h.Short()
	case string:
		s = strings.TrimPrefix(h, "b3:")
	default:
		s = fmt.Sprint(v)
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Format("2 Jan 2006")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func restLines(s string) string {
	_, r, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(r)
}

// principalName shows "alice/backend" for "agent:alice/backend".
func principalName(ref string) string {
	if _, rest, ok := strings.Cut(ref, ":"); ok {
		return rest
	}
	return ref
}

func pct(a, b int) int {
	if b <= 0 {
		return 0
	}
	return a * 100 / b
}
