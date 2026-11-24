package web

// Operations pages (#10, #38): Admin > Audit (the operation log, filtered,
// with JSONL and CSV downloads), the quota panel of the organization page
// and the disk-space warning of the admin pages.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// AuditExportPath is where the audit log downloads. The CLI (vx admin
// audit) calls it with its token; the web UI with the session.
const AuditExportPath = "/ui/admin/audit/export"

func (u *UI) mountOps(mux *http.ServeMux, funcs template.FuncMap) error {
	t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/audit.html")
	if err != nil {
		return fmt.Errorf("template audit: %w", err)
	}
	u.pages["audit"] = t
	mux.HandleFunc("GET /ui/admin/audit", u.auth(u.auditPage))
	mux.HandleFunc("GET "+AuditExportPath, u.auditExport)
	mux.HandleFunc("POST /ui/orgs/{org}/quota", u.auth(u.admin(u.orgQuotaPost)))
	return nil
}

// auditQuery reads the filters of the page and the download.
func auditQuery(r *http.Request) (service.AuditQuery, error) {
	q := service.AuditQuery{Org: param(r, "org"), Repo: param(r, "repo"), Actor: param(r, "actor")}
	var err error
	if q.Since, err = parseWhen(param(r, "since"), false); err != nil {
		return q, err
	}
	if q.Until, err = parseWhen(param(r, "until"), true); err != nil {
		return q, err
	}
	return q, nil
}

// parseWhen reads a date (2026-11-24), a time (RFC 3339) or an age (7d,
// 12h) before now. A date as the end of a range includes that whole day.
func parseWhen(v string, end bool) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		if end {
			t = t.Add(24 * time.Hour)
		}
		return t, nil
	}
	if d, ok := parseAge(v); ok {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, &service.Error{Code: service.CodeInvalid, Msg: fmt.Sprintf("time %q: write a date (2026-11-24), a time (2026-11-24T15:04:05Z) or an age (7d, 12h)", v)}
}

// parseAge reads 7d, 2w, 1y or a Go duration like 12h.
func parseAge(v string) (time.Duration, bool) {
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(v, suffix); ok {
			var k int
			if _, err := fmt.Sscan(n, &k); err == nil && k > 0 && fmt.Sprint(k) == n {
				return time.Duration(k) * unit, true
			}
			return 0, false
		}
	}
	d, err := time.ParseDuration(v)
	return d, err == nil && d > 0
}

// auditOrgs lists the organizations the caller can export.
func (u *UI) auditOrgs(r *http.Request, actor service.Actor) []string {
	orgs, roles, err := u.svc.ListOrgs(r.Context(), actor, false)
	if err != nil {
		return nil
	}
	var out []string
	for i, o := range orgs {
		if roles[i] == service.OrgOwner || roles[i] == service.OrgAdmin {
			out = append(out, o.Handle)
		}
	}
	return out
}

// auditPreview is how many entries the page shows; the downloads have
// all of them.
const auditPreview = 200

func (u *UI) auditPage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	super := actor.Kind == store.KindHuman && u.svc.IsSuperadmin(ctx, actor)
	orgs := u.auditOrgs(r, actor)
	if !super && len(orgs) == 0 {
		u.failPage(w, &service.Error{Code: service.CodePermission, Msg: "the audit log is for superadmins and for owners and admins of an organization"})
		return
	}
	q, err := auditQuery(r)
	if q.Org == "" && !super && len(orgs) > 0 {
		q.Org = orgs[0]
	}
	if q.Since.IsZero() && param(r, "since") == "" {
		q.Since = time.Now().Add(-7 * 24 * time.Hour)
	}
	data := map[string]any{
		"Actor": actor, "Title": "Audit", "Section": "admin", "Super": super, "Orgs": orgs,
		"Q": q, "SinceText": param(r, "since"), "UntilText": param(r, "until"), "Preview": auditPreview,
		"Retention": retentionText(u.svc.Audit.Retention),
	}
	if d, ok := u.svc.DataDisk(); ok && super {
		data["Disk"] = d
	}
	// Objects whose stored size is not counted yet, when the server
	// counts sizes (object storage): stored-data quotas lag behind them.
	if p, ok := any(u.svc).(interface {
		ObjectSizesPending(context.Context) (int64, error)
	}); ok && super {
		if n, err := p.ObjectSizesPending(ctx); err == nil && n > 0 {
			data["SizesPending"] = n
		}
	}
	if param(r, "since") == "" {
		data["SinceText"] = "7d"
	}
	var rows []service.AuditRow
	if err == nil {
		pq := q
		pq.Newest = auditPreview
		err = u.svc.ExportAudit(ctx, actor, pq, func(row service.AuditRow) error {
			rows = append(rows, row)
			return nil
		})
	}
	status := http.StatusOK
	if err != nil {
		status = errStatus(err)
		data["Err"] = err.Error()
	}
	data["Rows"] = rows
	v := url.Values{}
	for _, k := range []string{"org", "repo", "actor"} {
		if s := param(r, k); s != "" {
			v.Set(k, s)
		}
	}
	if q.Org != "" {
		v.Set("org", q.Org)
	}
	v.Set("since", data["SinceText"].(string))
	if s := param(r, "until"); s != "" {
		v.Set("until", s)
	}
	v.Set("format", "jsonl")
	data["JSONL"] = AuditExportPath + "?" + v.Encode()
	v.Set("format", "csv")
	data["CSV"] = AuditExportPath + "?" + v.Encode()
	u.renderStatus(w, r, status, "audit", data)
}

func retentionText(d time.Duration) string {
	if d <= 0 {
		return "kept forever"
	}
	return fmt.Sprintf("kept for %d days", int(d.Hours()/24))
}

// auditExport streams the export. It takes the session cookie or, for the
// CLI, an Authorization: Bearer token.
func (u *UI) auditExport(w http.ResponseWriter, r *http.Request) {
	actor, err := u.exportActor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	q, err := auditQuery(r)
	if err == nil {
		err = u.svc.CheckAuditQuery(r.Context(), actor, q)
	}
	format := param(r, "format")
	if format == "" {
		format = "jsonl"
	}
	if err == nil && format != "jsonl" && format != "csv" {
		err = &service.Error{Code: service.CodeInvalid, Msg: fmt.Sprintf("format %q: use jsonl or csv", format)}
	}
	if err != nil {
		http.Error(w, err.Error(), errStatus(err))
		return
	}
	ctype := "application/x-ndjson"
	if format == "csv" {
		ctype = "text/csv; charset=utf-8"
	}
	name := "vx-audit"
	if q.Org != "" {
		name += "-" + strings.ToLower(q.Org)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+"-"+time.Now().UTC().Format("20060102")+"."+format))
	w.Header().Set("Cache-Control", "no-store")
	bw := bufio.NewWriterSize(w, 64<<10)
	if err := u.svc.WriteAudit(r.Context(), actor, q, format, bw); err != nil {
		// The status is sent already: end the body with the error, so a
		// download is visibly cut short.
		_ = bw.Flush()
		fmt.Fprintf(w, "\nexport stopped: %v\n", err)
		u.log.Warn("audit export stopped", "err", err)
		return
	}
	_ = bw.Flush()
}

func (u *UI) exportActor(r *http.Request) (service.Actor, error) {
	var tok string
	if h, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		tok = strings.TrimSpace(h)
	} else if c, err := r.Cookie(cookieName(r)); err == nil {
		tok = c.Value
	}
	if tok == "" {
		return service.Actor{}, errors.New("sign in first: open the web UI, or send your token (vx admin audit does)")
	}
	return u.actorForToken(r.Context(), tok)
}

func (u *UI) actorForToken(ctx context.Context, tok string) (service.Actor, error) {
	p, tokenID, err := u.svc.Store.AuthenticateToken(ctx, tok)
	if err != nil {
		return service.Actor{}, errors.New("token not valid or revoked: run vx login")
	}
	a, err := u.svc.ResolveActor(ctx, p)
	if err != nil {
		return service.Actor{}, err
	}
	a.TokenID = tokenID
	return a, nil
}

// ---------- quotas on the organization page ----------

// orgQuota is the quota panel's data; nil hides the panel.
type orgQuota struct {
	Rows  []service.QuotaRow
	Super bool // may change the quotas
	Form  map[string]string
}

func (u *UI) orgQuota(r *http.Request, actor service.Actor, org string) *orgQuota {
	usage, err := u.svc.OrgQuota(r.Context(), actor, org)
	if err != nil {
		return nil
	}
	q := &orgQuota{Rows: usage.Rows(), Super: actor.Kind == store.KindHuman && u.svc.IsSuperadmin(r.Context(), actor), Form: map[string]string{}}
	vals := map[string]*int64{"repos": usage.Override.Repos, "stored": usage.Override.StoredBytes, "import": usage.Override.ImportBytes,
		"push": usage.Override.PushBytes, "open_efforts": usage.Override.OpenEfforts}
	for _, n := range service.QuotaNames {
		v := vals[n]
		switch {
		case v == nil:
			q.Form[n] = ""
		case n == "repos" || n == "open_efforts":
			q.Form[n] = fmt.Sprint(*v)
		case *v == 0:
			q.Form[n] = "0"
		default:
			q.Form[n] = strings.ReplaceAll(service.FormatBytes(*v), " ", "")
		}
	}
	return q
}

// orgQuotaPost saves a superadmin's override. An empty field goes back
// to the server's default; 0 means no limit.
func (u *UI) orgQuotaPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	var o store.OrgQuota
	fields := map[string]**int64{"repos": &o.Repos, "stored": &o.StoredBytes, "import": &o.ImportBytes, "push": &o.PushBytes, "open_efforts": &o.OpenEfforts}
	for _, n := range service.QuotaNames {
		v := strings.TrimSpace(r.PostForm.Get(n))
		if v == "" {
			continue
		}
		var x int64
		var err error
		if n == "repos" || n == "open_efforts" {
			_, err = fmt.Sscan(v, &x)
			if err == nil && x < 0 {
				err = errors.New("negative")
			}
		} else {
			x, err = service.ParseBytes(v)
		}
		if err != nil {
			u.orgDone(w, r, actor, &service.Error{Code: service.CodeInvalid, Msg: fmt.Sprintf("quota %s: %q is not valid; give a count or a size like 10GB, 0 for no limit, or leave it empty for the default", n, v)}, "", "")
			return
		}
		*fields[n] = &x
	}
	_, err := u.svc.SetOrgQuota(r.Context(), actor, r.PathValue("org"), o)
	u.orgDone(w, r, actor, err, "quota", "quotas")
}
