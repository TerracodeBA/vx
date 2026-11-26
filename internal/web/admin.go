package web

// Admin pages: organizations (members, teams, repositories, settings),
// repository access (direct grants, support access and effective roles)
// and, for superadmins, the people on the server. Every change is a plain POST form
// checked with checkOrigin; agents may look but never change anything
// here. Service errors are shown on the page as messages; things the
// caller cannot read are 404.

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func (u *UI) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/orgs", u.auth(u.orgsPage))
	mux.HandleFunc("POST /ui/orgs", u.auth(u.admin(u.orgCreatePost)))
	mux.HandleFunc("GET /ui/orgs/{org}", u.auth(u.orgPage))
	mux.HandleFunc("POST /ui/orgs/{org}/settings", u.auth(u.admin(u.orgSettingsPost)))
	mux.HandleFunc("POST /ui/orgs/{org}/members", u.auth(u.admin(u.orgMemberPost)))
	mux.HandleFunc("POST /ui/orgs/{org}/members/remove", u.auth(u.admin(u.orgMemberRemovePost)))
	mux.HandleFunc("POST /ui/orgs/{org}/teams", u.auth(u.admin(u.teamCreatePost)))
	mux.HandleFunc("POST /ui/orgs/{org}/teams/delete", u.auth(u.admin(u.teamDeletePost)))
	mux.HandleFunc("POST /ui/orgs/{org}/teams/members", u.auth(u.admin(u.teamMemberPost)))
	mux.HandleFunc("POST /ui/orgs/{org}/teams/members/remove", u.auth(u.admin(u.teamMemberRemovePost)))
	// Repository names contain a slash (acme/shop-api), so the access page
	// matches the rest of the path and checks its suffix.
	mux.HandleFunc("GET /ui/repos/{path...}", u.auth(u.accessPage))
	mux.HandleFunc("POST /ui/repos/{path...}", u.auth(u.admin(u.accessPost)))
	mux.HandleFunc("GET /ui/admin/people", u.auth(u.peoplePage))
	mux.HandleFunc("POST /ui/admin/people", u.auth(u.admin(u.personCreatePost)))
	mux.HandleFunc("POST /ui/admin/invites", u.auth(u.admin(u.invitePost)))
	mux.HandleFunc("POST /ui/admin/invites/withdraw", u.auth(u.admin(u.inviteWithdrawPost)))
	mux.HandleFunc("POST /ui/admin/review", u.auth(u.admin(u.superadminReviewPost)))
}

// admin wraps every admin form handler: the form must come from this site
// and from a person's session. Agents get 403 even when the person they act
// for could make the change.
func (u *UI) admin(next handler) handler {
	return func(w http.ResponseWriter, r *http.Request, actor service.Actor) {
		if err := r.ParseForm(); err != nil {
			u.fail(w, &service.Error{Code: service.CodeInvalid, Msg: "could not read the form: " + err.Error()})
			return
		}
		if !u.checkOrigin(w, r) {
			return
		}
		if actor.Kind != store.KindHuman {
			u.fail(w, &service.Error{Code: service.CodePermission, Msg: "agents cannot change organizations, access or people; sign in to the web UI as the person instead"})
			return
		}
		next(w, r, actor)
	}
}

// errStatus maps a service error to the HTTP status of the page that shows
// it.
func errStatus(err error) int {
	var se *service.Error
	if errors.As(service.Classify(err), &se) {
		switch se.Code {
		case service.CodeNotFound:
			return http.StatusNotFound
		case service.CodeInvalid, service.CodeFailedPrecondition:
			return http.StatusBadRequest
		case service.CodeExists:
			return http.StatusConflict
		case service.CodePermission:
			return http.StatusForbidden
		case service.CodeUnavailable:
			return http.StatusServiceUnavailable
		case service.CodeTooMany:
			return http.StatusTooManyRequests
		}
	}
	return http.StatusInternalServerError
}

// failPage is u.fail with errStatus: an unavailable service is a 503
// with its message, not a 500.
func (u *UI) failPage(w http.ResponseWriter, err error) {
	if u.ssoRequiredFrom(w, err) {
		return
	}
	status := errStatus(err)
	if status == http.StatusInternalServerError {
		u.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = u.pages["login"].ExecuteTemplate(w, "error", map[string]any{"Status": status, "Message": err.Error()})
}

// isNotFound reports whether err means the thing does not exist or the
// caller may not know it does.
func isNotFound(err error) bool { return errStatus(err) == http.StatusNotFound }

// flash is the outcome of a form shown on the page it returns to.
type flash struct {
	Status int // HTTP status of the page; 0 means 200
	OK     string
	Err    string
}

// okMessages are the success notes a redirect may name with ?ok=. Only
// these fixed texts are shown, so a link cannot put words on the page.
var okMessages = map[string]string{
	"created":        "Organization created.",
	"settings":       "Settings saved.",
	"member":         "Membership saved.",
	"member-removed": "Member removed.",
	"team":           "Team created.",
	"team-deleted":   "Team deleted.",
	"team-member":    "Team member added.",
	"team-removed":   "Team member removed.",
	"granted":        "Access granted.",
	"revoked":        "Access revoked.",
	"invited":        "Invite saved. They can sign in with GitHub now.",
	"uninvited":      "Invite withdrawn.",
	"support":        "Support access granted.",
	"support-ended":  "Support access ended.",
	"reviewed":       "Marked as reviewed.",
	"quota":          "Quotas saved.",
	"checks":         "Organization checks saved as a new version. They apply to every repository of the organization from its next land.",
	"sso":            "Single sign-on saved.",
	"sso-removed":    "Single sign-on turned off.",
	"agreement":      "Data processing agreement accepted. The acceptance is recorded below.",
	"deleted":        "Organization deleted. It is hidden now; you can restore it until its purge.",
	"restored":       "Organization restored.",
	"merged":         "Accounts merged. The merged account is disabled; its tokens, agents and memberships now belong to the account you kept.",
}

func flashFrom(r *http.Request) flash { return flash{OK: okMessages[r.URL.Query().Get("ok")]} }

func failed(err error) flash { return flash{Status: errStatus(err), Err: err.Error()} }

// renderStatus renders a page with a status other than 200.
func (u *UI) renderStatus(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	if status != 0 && status != http.StatusOK {
		u.render(&statusWriter{ResponseWriter: w, status: status}, r, page, data)
		return
	}
	u.render(w, r, page, data)
}

// statusWriter sends status instead of 200 on the first write, so render
// can set its headers first.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.wrote = true
		s.ResponseWriter.WriteHeader(code)
	}
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(s.status)
	}
	return s.ResponseWriter.Write(b)
}

// canManageOrg reports whether the page offers changes. Superadmins see
// every organization but change only those they own or administer.
func canManageOrg(actor service.Actor, role service.OrgRole) bool {
	if actor.Kind != store.KindHuman {
		return false
	}
	return role == service.OrgOwner || role == service.OrgAdmin
}

var orgRoles = []service.OrgRole{service.OrgMember, service.OrgAdmin, service.OrgOwner}

// defaultRoles are the choices for an organization's default repository
// role; RoleNone shows as "none".
var defaultRoles = []service.RepoRole{service.RoleNone, service.RoleRead, service.RoleWrite}

var grantRoles = []service.RepoRole{service.RoleRead, service.RoleWrite, service.RoleAdmin}

// ---------- organizations ----------

type orgRow struct {
	service.Org
	Role  service.OrgRole
	Repos int
	Users int
}

func (u *UI) orgsPage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	u.renderOrgs(w, r, actor, flashFrom(r), nil)
}

func (u *UI) renderOrgs(w http.ResponseWriter, r *http.Request, actor service.Actor, f flash, form url.Values) {
	ctx := r.Context()
	data := map[string]any{"Actor": actor, "Title": "Organizations", "Section": "orgs", "Flash": f, "Form": form,
		"IsHuman": actor.Kind == store.KindHuman}
	siteAdmin := u.svc.IsSuperadmin(ctx, actor)
	all := siteAdmin && param(r, "all") != ""
	data["SiteAdmin"], data["All"] = siteAdmin, all
	orgs, roles, err := u.svc.ListOrgs(ctx, actor, all)
	if err != nil {
		data["ListErr"] = err.Error()
	}
	rows := make([]orgRow, 0, len(orgs))
	owned, repos := 0, 0
	for i, o := range orgs {
		row := orgRow{Org: o}
		if i < len(roles) {
			row.Role = roles[i]
		}
		if d, err := u.svc.GetOrg(ctx, actor, o.Handle); err == nil {
			row.Repos, row.Users = len(d.Repos), len(d.Members)
			if row.Role == "" {
				row.Role = d.MyRole
			}
		}
		if row.Role == service.OrgOwner {
			owned++
		}
		repos += row.Repos
		rows = append(rows, row)
	}
	data["Orgs"], data["Owned"], data["RepoTotal"] = rows, owned, repos
	data["Deleted"] = u.deletedOrgRows(r, actor)
	u.renderStatus(w, r, f.Status, "orgs", data)
}

func (u *UI) orgCreatePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	handle := strings.TrimSpace(r.PostForm.Get("handle"))
	o, err := u.svc.CreateOrg(r.Context(), actor, handle, strings.TrimSpace(r.PostForm.Get("display_name")))
	if err != nil {
		u.renderOrgs(w, r, actor, failed(err), r.PostForm)
		return
	}
	http.Redirect(w, r, "/ui/orgs/"+url.PathEscape(o.Handle)+"?ok=created", http.StatusSeeOther)
}

type memberRow struct {
	service.Membership
	Self bool
}

type orgRepoRow struct {
	store.Repo
	MyRole service.RepoRole
	Admin  bool // the caller may open its access page
}

func (u *UI) orgPage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	u.renderOrg(w, r, actor, flashFrom(r))
}

func (u *UI) renderOrg(w http.ResponseWriter, r *http.Request, actor service.Actor, f flash) {
	ctx := r.Context()
	d, err := u.svc.GetOrg(ctx, actor, r.PathValue("org"))
	if err != nil {
		if !isNotFound(err) || !u.renderDeletedOrg(w, r, actor, f) {
			u.failPage(w, err)
		}
		return
	}
	members := make([]memberRow, 0, len(d.Members))
	owners := 0
	for _, m := range d.Members {
		members = append(members, memberRow{Membership: m, Self: m.Person.ID == actor.ID || (actor.Parent != nil && m.Person.ID == actor.Parent.ID)})
		if m.Role == service.OrgOwner {
			owners++
		}
	}
	repos := make([]orgRepoRow, 0, len(d.Repos))
	for _, repo := range d.Repos {
		role := u.svc.RepoRoleOf(ctx, actor, repo)
		repos = append(repos, orgRepoRow{Repo: repo, MyRole: role, Admin: role.Includes(service.RoleAdmin)})
	}
	manage := canManageOrg(actor, d.MyRole)
	data := map[string]any{
		"Actor": actor, "Title": d.Org.Handle, "Section": "orgs", "Flash": f,
		"O": d.Org, "MyRole": d.MyRole, "Members": members, "Teams": d.Teams, "Repos": repos,
		"Owners": owners, "Manage": manage, "IsOwner": d.MyRole == service.OrgOwner,
		"IsHuman": actor.Kind == store.KindHuman, "OrgRoles": orgRoles, "DefaultRoles": defaultRoles,
		"Quota": u.orgQuota(r, actor, d.Org.Handle), "GraceDays": u.graceDays(),
	}
	u.orgSSO(r, actor, d.MyRole, data)
	u.orgAgreement(r, actor, d.Org.Handle, data)
	u.orgChecks(r, actor, d.Org.Handle, data)
	u.renderStatus(w, r, f.Status, "org", data)
}

// orgDone redirects back to the organization page after a change, or shows
// the page again with the error.
func (u *UI) orgDone(w http.ResponseWriter, r *http.Request, actor service.Actor, err error, ok, anchor string) {
	if err != nil {
		u.renderOrg(w, r, actor, failed(err))
		return
	}
	http.Redirect(w, r, "/ui/orgs/"+url.PathEscape(r.PathValue("org"))+"?ok="+ok+"#"+anchor, http.StatusSeeOther)
}

func (u *UI) orgSettingsPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	var def *service.RepoRole
	if r.PostForm.Has("default_role") {
		role := service.RepoRole(r.PostForm.Get("default_role"))
		if role != service.RoleNone && role != service.RoleRead && role != service.RoleWrite {
			u.orgDone(w, r, actor, &service.Error{Code: service.CodeInvalid, Msg: "default repository role must be none, read or write"}, "", "")
			return
		}
		def = &role
	}
	_, err := u.svc.UpdateOrg(r.Context(), actor, r.PathValue("org"), strings.TrimSpace(r.PostForm.Get("display_name")), def)
	u.orgDone(w, r, actor, err, "settings", "settings")
}

func (u *UI) orgMemberPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	person := handleOf(r.PostForm.Get("person"))
	role := service.OrgRole(r.PostForm.Get("role"))
	if role == "" {
		role = service.OrgMember
	}
	if person == "" {
		u.orgDone(w, r, actor, &service.Error{Code: service.CodeInvalid, Msg: "enter the handle of the person to add, for example alice"}, "", "")
		return
	}
	_, err := u.svc.SetMember(r.Context(), actor, r.PathValue("org"), person, role)
	u.orgDone(w, r, actor, err, "member", "members")
}

func (u *UI) orgMemberRemovePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	err := u.svc.RemoveMember(r.Context(), actor, r.PathValue("org"), handleOf(r.PostForm.Get("person")))
	u.orgDone(w, r, actor, err, "member-removed", "members")
}

func (u *UI) teamCreatePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	_, err := u.svc.CreateTeam(r.Context(), actor, r.PathValue("org"), strings.TrimSpace(r.PostForm.Get("name")))
	u.orgDone(w, r, actor, err, "team", "teams")
}

func (u *UI) teamDeletePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	err := u.svc.DeleteTeam(r.Context(), actor, r.PathValue("org"), r.PostForm.Get("team"))
	u.orgDone(w, r, actor, err, "team-deleted", "teams")
}

func (u *UI) teamMemberPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	person := handleOf(r.PostForm.Get("person"))
	if person == "" {
		u.orgDone(w, r, actor, &service.Error{Code: service.CodeInvalid, Msg: "enter the handle of the member to add to the team"}, "", "")
		return
	}
	_, err := u.svc.SetTeamMember(r.Context(), actor, r.PathValue("org"), r.PostForm.Get("team"), person)
	u.orgDone(w, r, actor, err, "team-member", "teams")
}

func (u *UI) teamMemberRemovePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	_, err := u.svc.RemoveTeamMember(r.Context(), actor, r.PathValue("org"), r.PostForm.Get("team"), handleOf(r.PostForm.Get("person")))
	u.orgDone(w, r, actor, err, "team-removed", "teams")
}

// handleOf accepts "alice", "@alice" or "user:alice" and returns "alice".
func handleOf(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	s = strings.TrimPrefix(s, "user:")
	return s
}

// ---------- repository access ----------

// accessRepo returns the repository named by a /ui/repos/NAME/SUFFIX path,
// and whether the path ends in suffix.
func accessRepo(r *http.Request, suffix string) (string, bool) {
	p := r.PathValue("path")
	name, ok := strings.CutSuffix(p, suffix)
	return name, ok && name != ""
}

type grantRow struct {
	service.RepoGrant
	Team bool
}

func (u *UI) accessPage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	repo, ok := accessRepo(r, "/access")
	if !ok {
		http.NotFound(w, r)
		return
	}
	u.renderAccess(w, r, actor, repo, flashFrom(r), nil)
}

func (u *UI) renderAccess(w http.ResponseWriter, r *http.Request, actor service.Actor, repoName string, f flash, form url.Values) {
	ctx := r.Context()
	repo, _, err := u.svc.Authorize(ctx, actor, repoName, service.RoleAdmin)
	if err != nil {
		u.failPage(w, err)
		return
	}
	support, _ := u.svc.ListSupport(ctx, actor, repo.Name)
	grants, entries, err := u.svc.RepoAccess(ctx, actor, repo.Name)
	if err != nil {
		if isNotFound(err) || errStatus(err) == http.StatusForbidden {
			u.failPage(w, err)
			return
		}
		if f.Err == "" {
			f = failed(err)
		}
	}
	rows := make([]grantRow, 0, len(grants))
	for _, g := range grants {
		rows = append(rows, grantRow{RepoGrant: g, Team: strings.HasPrefix(g.Subject, "team:")})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Role != entries[j].Role {
			return entries[i].Role.Includes(entries[j].Role)
		}
		return entries[i].Person.Handle < entries[j].Person.Handle
	})
	counts := map[service.RepoRole]int{}
	for _, e := range entries {
		counts[e.Role]++
	}
	kind := "user"
	if form != nil && form.Get("kind") == "team" {
		kind = "team"
	}
	u.renderStatus(w, r, f.Status, "access", map[string]any{
		"Actor": actor, "Title": "Access · " + repo.Name, "ActiveRepo": repo.Name, "Flash": f, "Form": form, "Kind": kind,
		"Repo": repo, "Grants": rows, "Entries": entries, "GrantRoles": grantRoles,
		"Admins": counts[service.RoleAdmin], "Writers": counts[service.RoleWrite], "Readers": counts[service.RoleRead],
		"IsHuman":        actor.Kind == store.KindHuman,
		"GitHubSettings": u.svc.GitHubSettingsURL(repo), "GitHubTTL": service.GitHubRoleTTL.String(),
		"Support": support, "SupportDurations": supportDurations,
	})
}

// supportDurations are the choices for how long support access lasts.
var supportDurations = []struct {
	Value, Label string
}{{"4h", "4 hours"}, {"24h", "1 day"}, {"72h", "3 days"}, {"168h", "7 days"}, {"720h", "30 days"}}

// supportPost grants support access from the access page.
func (u *UI) supportPost(w http.ResponseWriter, r *http.Request, actor service.Actor, repo string) {
	d, err := time.ParseDuration(r.PostForm.Get("for"))
	if err != nil {
		u.accessDone(w, r, actor, repo, &service.Error{Code: service.CodeInvalid, Msg: "choose how long the support access lasts"}, "")
		return
	}
	_, err = u.svc.GrantSupport(r.Context(), actor, service.SupportInput{Repo: repo, Person: handleOf(r.PostForm.Get("person")),
		Role: service.RepoRole(r.PostForm.Get("role")), For: d, Reason: r.PostForm.Get("reason")})
	u.accessDone(w, r, actor, repo, err, "support")
}

// superadminReviewPost marks the upgrade review done (superadmins).
func (u *UI) superadminReviewPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if !u.svc.IsSuperadmin(r.Context(), actor) {
		http.NotFound(w, r)
		return
	}
	if err := u.svc.ResolveSuperadminReview(r.Context(), actor); err != nil {
		u.failPage(w, err)
		return
	}
	http.Redirect(w, r, "/ui/admin/people?ok=reviewed", http.StatusSeeOther)
}

func (u *UI) accessPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	if repo, ok := accessRepo(r, "/access/grant"); ok {
		org := ""
		if rp, _, err := u.svc.Authorize(ctx, actor, repo, service.RoleRead); err == nil {
			org = rp.Org
		}
		subject, err := grantSubject(r.PostForm.Get("kind"), r.PostForm.Get("subject"), org)
		if err == nil {
			_, err = u.svc.GrantRepo(ctx, actor, repo, subject, service.RepoRole(r.PostForm.Get("role")))
		}
		u.accessDone(w, r, actor, repo, err, "granted")
		return
	}
	if repo, ok := accessRepo(r, "/access/support"); ok {
		u.supportPost(w, r, actor, repo)
		return
	}
	if repo, ok := accessRepo(r, "/access/support/revoke"); ok {
		u.accessDone(w, r, actor, repo, u.svc.RevokeSupport(ctx, actor, repo, r.PostForm.Get("person")), "support-ended")
		return
	}
	if repo, ok := accessRepo(r, "/access/revoke"); ok {
		u.accessDone(w, r, actor, repo, u.svc.RevokeRepo(ctx, actor, repo, r.PostForm.Get("subject")), "revoked")
		return
	}
	http.NotFound(w, r)
}

func (u *UI) accessDone(w http.ResponseWriter, r *http.Request, actor service.Actor, repo string, err error, ok string) {
	if err != nil {
		if isNotFound(err) {
			// The repository itself may be unreadable: let the page decide.
			if _, _, aerr := u.svc.Authorize(r.Context(), actor, repo, service.RoleAdmin); aerr != nil {
				u.failPage(w, aerr)
				return
			}
		}
		u.renderAccess(w, r, actor, repo, failed(err), r.PostForm)
		return
	}
	http.Redirect(w, r, "/ui/repos/"+repo+"/access?ok="+ok, http.StatusSeeOther)
}

// grantSubject builds "user:alice" or "team:acme/backend" from the form. A
// team named without its organization is taken to be in org, the
// repository's organization (the only one whose teams can be granted).
func grantSubject(kind, name, org string) (string, error) {
	name = strings.TrimSpace(name)
	if k, rest, ok := strings.Cut(name, ":"); ok && (k == "user" || k == "team") {
		return k + ":" + rest, nil
	}
	if name == "" {
		return "", &service.Error{Code: service.CodeInvalid, Msg: "enter a person's handle (alice) or a team (acme/backend) to grant access to"}
	}
	switch kind {
	case "team":
		if !strings.Contains(name, "/") && org != "" {
			name = org + "/" + name
		}
		return "team:" + name, nil
	default:
		return "user:" + strings.TrimPrefix(name, "@"), nil
	}
}

// ---------- people (superadmins) ----------

type personRow struct {
	store.Principal
	GitHub string // GitHub login, when linked
	Agents int
	Orgs   []string // "acme (owner)"; personal organizations left out
	// MergedInto is the handle of the account this one was merged into;
	// such accounts are disabled.
	MergedInto string
}

func (u *UI) peoplePage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if !u.svc.IsSuperadmin(r.Context(), actor) {
		http.NotFound(w, r)
		return
	}
	u.renderPeople(w, r, actor, flashFrom(r), nil)
}

func (u *UI) renderPeople(w http.ResponseWriter, r *http.Request, actor service.Actor, f flash, form url.Values) {
	ctx := r.Context()
	data := map[string]any{"Actor": actor, "Title": "People", "Section": "admin", "Flash": f, "Form": form,
		"IsHuman": actor.Kind == store.KindHuman}
	people, err := u.svc.ListPeople(ctx, actor)
	if err != nil {
		data["ListErr"] = err.Error()
	}
	orgs, _, err := u.svc.ListOrgs(ctx, actor, true)
	if err != nil && data["ListErr"] == nil {
		data["ListErr"] = err.Error()
	}
	memberOf := map[string][]string{}
	var shared []service.Org // organizations a new person can be added to
	for _, o := range orgs {
		if o.Personal {
			continue
		}
		shared = append(shared, o)
		d, err := u.svc.GetOrg(ctx, actor, o.Handle)
		if err != nil {
			continue
		}
		for _, m := range d.Members {
			memberOf[m.Person.Handle] = append(memberOf[m.Person.Handle], o.Handle+" ("+string(m.Role)+")")
		}
	}
	rows := make([]personRow, 0, len(people))
	admins, agents := 0, 0
	for _, p := range people {
		row := personRow{Principal: p, Orgs: memberOf[p.Handle]}
		if as, err := u.svc.Store.ListAgents(ctx, p.ID); err == nil {
			row.Agents = len(as)
			agents += len(as)
		}
		if p.SiteAdmin {
			admins++
		}
		rows = append(rows, row)
	}
	data["People"], data["Agents"], data["Admins"] = rows, agents, admins
	data["SuperadminReview"] = u.svc.SuperadminReviewPending(ctx, actor)
	data["OrgCount"], data["OrgChoices"] = len(shared), shared
	u.peopleGitHub(r, actor, rows, data)
	if merged, err := u.svc.Store.MergedPeople(ctx); err == nil {
		byID := map[uuid.UUID]string{}
		for _, p := range people {
			byID[p.ID] = p.Handle
		}
		for i := range rows {
			if into, ok := merged[rows[i].ID]; ok {
				rows[i].MergedInto = byID[into]
				if rows[i].MergedInto == "" {
					rows[i].MergedInto = "another account"
				}
			}
		}
	}
	u.renderStatus(w, r, f.Status, "people", data)
}

func (u *UI) personCreatePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	ctx := r.Context()
	if !u.svc.IsSuperadmin(ctx, actor) {
		http.NotFound(w, r)
		return
	}
	f := r.PostForm
	p, secret, err := u.svc.CreatePerson(ctx, actor, strings.TrimSpace(f.Get("handle")), strings.TrimSpace(f.Get("display_name")),
		strings.TrimSpace(f.Get("email")), f.Get("site_admin") == "on", strings.TrimSpace(f.Get("org")))
	if err != nil {
		u.renderPeople(w, r, actor, failed(err), f)
		return
	}
	// The page holds a secret: keep it out of caches and history restores.
	// It is shown on this response only, never in a URL.
	w.Header().Set("Cache-Control", "no-store")
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	u.render(w, r, "people", map[string]any{"Actor": actor, "Title": "New person", "Section": "admin",
		"Created": p, "Secret": secret, "Server": scheme + "://" + r.Host, "Org": f.Get("org"), "MadeAdmin": f.Get("site_admin") == "on"})
}
