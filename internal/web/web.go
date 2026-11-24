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
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/web/highlight"
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
	for _, page := range []string{"login", "home", "repo", "effort", "change", "ship"} {
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
	mux.HandleFunc("POST /ui/logout", u.logout)
	mux.HandleFunc("GET /ui", u.auth(u.home))
	mux.HandleFunc("GET /ui/repo", u.auth(u.repo))
	mux.HandleFunc("GET /ui/effort", u.auth(u.effort))
	mux.HandleFunc("GET /ui/effort/head", u.auth(u.effortHead))
	mux.HandleFunc("GET /ui/change", u.auth(u.change))
	mux.HandleFunc("GET /ui/diff", u.auth(u.diff))
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
	token := strings.TrimSpace(r.PostForm.Get("token"))
	next := r.PostForm.Get("next")
	if !strings.HasPrefix(next, "/ui") {
		next = "/ui"
	}
	if _, err := u.svc.Store.Authenticate(r.Context(), token); err != nil {
		u.render(w, r, "login", map[string]any{"Title": "Sign in", "Next": next, "Error": "That token is not valid. Use a token from vx-server user-add or vx agent add."})
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
	shipments, _ := u.svc.Store.ListShipments(ctx, v.Effort.ID)
	baseRow, _ := u.svc.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Base)
	changesView := param(r, "view") == "changes"
	var allFiles []changedFile
	if changesView {
		if allFiles, err = u.allChanges(r, v.Repo, v.Effort, entries, color); err != nil {
			u.fail(w, err)
			return
		}
	}
	u.render(w, r, "effort", map[string]any{
		"ChangesView": changesView, "AllFiles": allFiles,
		"Actor": actor, "Repo": v.Repo, "Effort": v.Effort, "Owner": u.ref(r, v.Effort.OwnerID),
		"Working": working, "WorkColor": workColor, "Lands": lands, "Agents": agents, "Shipments": shipments,
		"Added": totalAdded, "Deleted": totalDeleted, "Merged": merged, "Verified": verified,
		"BaseGit": baseRow.GitSHA, "Title": v.Effort.Name,
	})
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
func (u *UI) effortHead(w http.ResponseWriter, r *http.Request, _ service.Actor) {
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
