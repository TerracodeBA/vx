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
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

const cookieName = "vx_token"

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
	for _, page := range []string{"login", "home", "repo", "effort", "change", "ship"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+page+".html")
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
	mux.HandleFunc("POST /ui/logout", u.logout)
	mux.HandleFunc("GET /ui", u.auth(u.home))
	mux.HandleFunc("GET /ui/repo", u.auth(u.repo))
	mux.HandleFunc("GET /ui/effort", u.auth(u.effort))
	mux.HandleFunc("GET /ui/effort/head", u.auth(u.effortHead))
	mux.HandleFunc("GET /ui/change", u.auth(u.change))
	mux.HandleFunc("GET /ui/ship", u.auth(u.ship))
	mux.HandleFunc("POST /ui/ship", u.auth(u.shipPost))
	return nil
}

type handler func(w http.ResponseWriter, r *http.Request, actor service.Actor)

func (u *UI) auth(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			http.Redirect(w, r, "/ui/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		p, err := u.svc.Store.Authenticate(r.Context(), c.Value)
		if err != nil {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
			http.Redirect(w, r, "/ui/login?expired=1", http.StatusFound)
			return
		}
		actor, err := u.svc.ResolveActor(r.Context(), p)
		if err != nil {
			u.fail(w, err)
			return
		}
		next(w, r, actor)
	}
}

func (u *UI) render(w http.ResponseWriter, page string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:")
	w.Header().Set("X-Frame-Options", "DENY")
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
	u.render(w, "login", map[string]any{"Title": "Sign in", "Next": r.URL.Query().Get("next"), "Expired": r.URL.Query().Get("expired") != ""})
}

func (u *UI) loginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	token := strings.TrimSpace(r.PostForm.Get("token"))
	next := r.PostForm.Get("next")
	if !strings.HasPrefix(next, "/ui") {
		next = "/ui"
	}
	if _, err := u.svc.Store.Authenticate(r.Context(), token); err != nil {
		u.render(w, "login", map[string]any{"Title": "Sign in", "Next": next, "Error": "That token is not valid. Use a token from vx-server user-add or vx agent add."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil, MaxAge: 30 * 24 * 3600,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// ---------- home and repo ----------

type effortRow struct {
	store.Effort
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
	u.render(w, "home", map[string]any{"Actor": actor, "Repos": rows, "Title": "Efforts"})
}

func (u *UI) effortRow(r *http.Request, repo store.Repo, e store.Effort) effortRow {
	ctx := r.Context()
	row := effortRow{Effort: e, Owner: u.ref(r, e.OwnerID)}
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
	repo, err := u.svc.Store.RepoByName(ctx, r.URL.Query().Get("name"))
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
	u.render(w, "repo", map[string]any{"Actor": actor, "Repo": repo, "Branches": branches, "Efforts": rows, "Title": repo.Name})
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
	repoName, slug := r.URL.Query().Get("repo"), r.URL.Query().Get("slug")
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
	shipments, _ := u.svc.Store.ListShipments(ctx, v.Effort.ID)
	baseRow, _ := u.svc.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Base)
	u.render(w, "effort", map[string]any{
		"Actor": actor, "Repo": v.Repo, "Effort": v.Effort, "Owner": u.ref(r, v.Effort.OwnerID),
		"Working": working, "WorkColor": workColor, "Lands": lands, "Agents": agents, "Shipments": shipments,
		"Added": totalAdded, "Deleted": totalDeleted, "Merged": merged, "Verified": verified,
		"BaseGit": baseRow.GitSHA, "Title": v.Effort.Name,
	})
}

// effortHead lets the page poll cheaply for changes.
func (u *UI) effortHead(w http.ResponseWriter, r *http.Request, _ service.Actor) {
	v, err := u.svc.GetEffort(r.Context(), r.URL.Query().Get("repo"), r.URL.Query().Get("slug"))
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
}

// ---------- change ----------

type diffLine struct {
	Kind string // ctx, add, del, hunk
	Text string
	Old  int
	New  int
}

type fileDiff struct {
	store.FileChange
	Binary bool
	Lines  []diffLine
	Large  bool
}

func (u *UI) change(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	repoName, ref := r.URL.Query().Get("repo"), r.URL.Query().Get("ref")
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
	u.render(w, "change", map[string]any{
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
	return fd, nil
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
			out = append(out, diffLine{Kind: "hunk", Text: fmt.Sprintf("%d unchanged lines", skipped)})
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
	repoName, slug, target := r.URL.Query().Get("repo"), r.URL.Query().Get("slug"), r.URL.Query().Get("to")
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
	u.render(w, "ship", data)
}

func (u *UI) shipPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, err)
		return
	}
	// Same-origin check for this state-changing form.
	if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
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

// ---------- helpers ----------

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

// principalName shows "admir/limiter" for "agent:admir/limiter".
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
