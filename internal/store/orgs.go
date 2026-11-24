package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Org is an organization. Every repository belongs to one.
type Org struct {
	ID              uuid.UUID
	Handle          string
	DisplayName     string
	DefaultRepoRole string // none, read or write
	Personal        bool
	CreatedAt       time.Time
	// GitHubOrgID and GitHubLogin are set for an organization linked to a
	// GitHub organization: its membership follows GitHub.
	GitHubOrgID *int64
	GitHubLogin string
}

// GitHubLinked reports whether o follows a GitHub organization.
func (o Org) GitHubLinked() bool { return o.GitHubOrgID != nil }

const orgCols = `o.id, o.handle, o.display_name, o.default_repo_role, o.personal, o.created_at, o.github_org_id, coalesce(o.github_login, '')`

func scanOrg(row interface{ Scan(...any) error }, extra ...any) (Org, error) {
	var o Org
	err := row.Scan(append([]any{&o.ID, &o.Handle, &o.DisplayName, &o.DefaultRepoRole, &o.Personal, &o.CreatedAt, &o.GitHubOrgID, &o.GitHubLogin}, extra...)...)
	return o, err
}

// CreateOrg inserts an organization and makes owner its owner.
func (q Q) CreateOrg(ctx context.Context, handle, displayName string, owner uuid.UUID) (Org, error) {
	if err := ValidateHandle(KindHuman, handle); err != nil {
		return Org{}, fmt.Errorf("organization %s", err)
	}
	if displayName == "" {
		displayName = handle
	}
	o, err := scanOrg(q.db.QueryRow(ctx, `with o as (
			insert into orgs (handle, display_name) values ($1, $2) returning *
		), m as (
			insert into org_members (org_id, person_id, role) select id, $3, 'owner' from o
		) select `+orgCols+` from o`, handle, displayName, owner))
	if isUniqueViolation(err) {
		return Org{}, fmt.Errorf("organization %q: %w", handle, ErrExists)
	}
	return o, err
}

// OrgByHandle looks an organization up by handle.
func (q Q) OrgByHandle(ctx context.Context, handle string) (Org, error) {
	o, err := scanOrg(q.db.QueryRow(ctx, `select `+orgCols+` from orgs o where o.handle = $1`, handle))
	return o, notFound(err, "organization "+handle)
}

// LockOrg looks an organization up and locks it until the transaction
// ends, so membership checks such as "the last owner stays" cannot race.
func (q Q) LockOrg(ctx context.Context, handle string) (Org, error) {
	o, err := scanOrg(q.db.QueryRow(ctx, `select `+orgCols+` from orgs o where o.handle = $1 for update`, handle))
	return o, notFound(err, "organization "+handle)
}

// OrgWithRole is an organization and a person's role in it ("" when not a
// member).
type OrgWithRole struct {
	Org
	Role string
}

// ListOrgs returns every organization (all) or those person is a member
// of, with person's role, by handle.
func (q Q) ListOrgs(ctx context.Context, person uuid.UUID, all bool) ([]OrgWithRole, error) {
	join := `join`
	if all {
		join = `left join`
	}
	rows, err := q.db.Query(ctx, `select `+orgCols+`, coalesce(m.role, '') from orgs o `+join+
		` org_members m on m.org_id = o.id and m.person_id = $1 order by o.handle`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgWithRole
	for rows.Next() {
		var ow OrgWithRole
		ow.Org, err = scanOrg(rows, &ow.Role)
		if err != nil {
			return nil, err
		}
		out = append(out, ow)
	}
	return out, rows.Err()
}

// UpdateOrg changes the display name and default repository role; nil
// values stay.
func (q Q) UpdateOrg(ctx context.Context, id uuid.UUID, displayName, defaultRole *string) (Org, error) {
	o, err := scanOrg(q.db.QueryRow(ctx, `update orgs o set display_name = coalesce($2, display_name),
		default_repo_role = coalesce($3, default_repo_role) where id = $1 returning `+orgCols, id, displayName, defaultRole))
	return o, notFound(err, "organization")
}

// OrgRole returns person's role in org, or "" when they are not a member.
func (q Q) OrgRole(ctx context.Context, org, person uuid.UUID) (string, error) {
	var role string
	err := q.db.QueryRow(ctx, `select role from org_members where org_id = $1 and person_id = $2`, org, person).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// CountOwners returns how many owners org has.
func (q Q) CountOwners(ctx context.Context, org uuid.UUID) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `select count(*) from org_members where org_id = $1 and role = 'owner'`, org).Scan(&n)
	return n, err
}

// SetOrgMember adds person to org with role, or changes their role.
func (q Q) SetOrgMember(ctx context.Context, org, person uuid.UUID, role string) error {
	_, err := q.db.Exec(ctx, `insert into org_members (org_id, person_id, role) values ($1, $2, $3)
		on conflict (org_id, person_id) do update set role = excluded.role`, org, person, role)
	return err
}

// RemoveOrgMember removes person from org and its teams. It reports whether
// they were a member.
func (q Q) RemoveOrgMember(ctx context.Context, org, person uuid.UUID) (bool, error) {
	tag, err := q.db.Exec(ctx, `delete from org_members where org_id = $1 and person_id = $2`, org, person)
	return tag.RowsAffected() > 0, err
}

// OrgMember is a person in an organization, with their teams.
type OrgMember struct {
	Person Principal
	Role   string
	Teams  []string
}

// ListOrgMembers returns org's members by handle.
func (q Q) ListOrgMembers(ctx context.Context, org uuid.UUID) ([]OrgMember, error) {
	rows, err := q.db.Query(ctx, `select `+prefixed("p", principalCols)+`, m.role,
			coalesce((select array_agg(t.name order by t.name) from team_members tm join teams t on t.id = tm.team_id
				where tm.org_id = m.org_id and tm.person_id = m.person_id), '{}')
		from org_members m join principals p on p.id = m.person_id
		where m.org_id = $1 order by p.handle`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgMember
	for rows.Next() {
		var m OrgMember
		var p Principal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Handle, &p.ParentID, &p.DisplayName, &p.Email, &p.SiteAdmin, &p.CreatedAt, &m.Role, &m.Teams); err != nil {
			return nil, err
		}
		m.Person = p
		out = append(out, m)
	}
	return out, rows.Err()
}

// Team is a named group of an organization's members.
type Team struct {
	ID      uuid.UUID
	OrgID   uuid.UUID
	Org     string
	Name    string
	Members []Principal
}

// CreateTeam inserts a team.
func (q Q) CreateTeam(ctx context.Context, org Org, name string) (Team, error) {
	if !handlePart.MatchString(name) {
		return Team{}, fmt.Errorf("team name %q: use lowercase letters, digits, dot, dash or underscore", name)
	}
	t := Team{OrgID: org.ID, Org: org.Handle, Name: name}
	err := q.db.QueryRow(ctx, `insert into teams (org_id, name) values ($1, $2) returning id`, org.ID, name).Scan(&t.ID)
	if isUniqueViolation(err) {
		return Team{}, fmt.Errorf("team %s/%s: %w", org.Handle, name, ErrExists)
	}
	return t, err
}

// TeamByName looks up a team of org, with its members.
func (q Q) TeamByName(ctx context.Context, org Org, name string) (Team, error) {
	t := Team{OrgID: org.ID, Org: org.Handle, Name: name}
	err := q.db.QueryRow(ctx, `select id from teams where org_id = $1 and name = $2`, org.ID, name).Scan(&t.ID)
	if err != nil {
		return Team{}, notFound(err, "team "+org.Handle+"/"+name)
	}
	t.Members, err = q.teamMembers(ctx, t.ID)
	return t, err
}

func (q Q) teamMembers(ctx context.Context, team uuid.UUID) ([]Principal, error) {
	rows, err := q.db.Query(ctx, `select `+prefixed("p", principalCols)+` from team_members tm
		join principals p on p.id = tm.person_id where tm.team_id = $1 order by p.handle`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListTeams returns org's teams with their members, by name.
func (q Q) ListTeams(ctx context.Context, org Org) ([]Team, error) {
	rows, err := q.db.Query(ctx, `select id, name from teams where org_id = $1 order by name`, org.ID)
	if err != nil {
		return nil, err
	}
	var out []Team
	for rows.Next() {
		t := Team{OrgID: org.ID, Org: org.Handle}
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Members, err = q.teamMembers(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteTeam deletes a team, its members and its grants.
func (q Q) DeleteTeam(ctx context.Context, team uuid.UUID) error {
	_, err := q.db.Exec(ctx, `delete from teams where id = $1`, team)
	return err
}

// AddTeamMember adds person to team. The person must be a member of the
// team's organization (ErrNotFound otherwise).
func (q Q) AddTeamMember(ctx context.Context, t Team, person uuid.UUID) error {
	_, err := q.db.Exec(ctx, `insert into team_members (team_id, org_id, person_id) values ($1, $2, $3)
		on conflict do nothing`, t.ID, t.OrgID, person)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" {
		return fmt.Errorf("member of %s: %w", t.Org, ErrNotFound)
	}
	return err
}

// RemoveTeamMember removes person from team and reports whether they were
// in it.
func (q Q) RemoveTeamMember(ctx context.Context, team, person uuid.UUID) (bool, error) {
	tag, err := q.db.Exec(ctx, `delete from team_members where team_id = $1 and person_id = $2`, team, person)
	return tag.RowsAffected() > 0, err
}

// RepoGrant is a direct grant on a repository, to a principal or a team.
type RepoGrant struct {
	RepoID    uuid.UUID
	Principal *Principal // set for grants to a person or service
	Team      *Team      // set for grants to a team (without members)
	Role      string
}

// SetRepoGrant grants principal or team (exactly one non-nil) role on repo,
// replacing an earlier grant to the same subject. It returns the previous
// role, or "".
func (q Q) SetRepoGrant(ctx context.Context, repo uuid.UUID, principal, team *uuid.UUID, role string) (string, error) {
	var prev string
	err := q.db.QueryRow(ctx, `select role from repo_grants where repo_id = $1
		and person_id is not distinct from $2 and team_id is not distinct from $3`, repo, principal, team).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if prev != "" {
		_, err = q.db.Exec(ctx, `update repo_grants set role = $4 where repo_id = $1
			and person_id is not distinct from $2 and team_id is not distinct from $3`, repo, principal, team, role)
		return prev, err
	}
	_, err = q.db.Exec(ctx, `insert into repo_grants (repo_id, person_id, team_id, role) values ($1, $2, $3, $4)`,
		repo, principal, team, role)
	if isUniqueViolation(err) { // a concurrent grant to the same subject
		_, err = q.db.Exec(ctx, `update repo_grants set role = $4 where repo_id = $1
			and person_id is not distinct from $2 and team_id is not distinct from $3`, repo, principal, team, role)
	}
	return "", err
}

// DeleteRepoGrant removes the grant to principal or team on repo and
// returns the role it had ("" when there was none).
func (q Q) DeleteRepoGrant(ctx context.Context, repo uuid.UUID, principal, team *uuid.UUID) (string, error) {
	var role string
	err := q.db.QueryRow(ctx, `delete from repo_grants where repo_id = $1
		and person_id is not distinct from $2 and team_id is not distinct from $3 returning role`, repo, principal, team).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// ListRepoGrants returns repo's direct grants: principals first by handle,
// then teams by name.
func (q Q) ListRepoGrants(ctx context.Context, repo uuid.UUID) ([]RepoGrant, error) {
	rows, err := q.db.Query(ctx, `select g.role, `+prefixed("p", principalCols)+`, null::uuid, null::uuid, null, null
		from repo_grants g join principals p on p.id = g.person_id where g.repo_id = $1
		union all
		select g.role, null, null, null, null, null, null, null, null, t.id, t.org_id, o.handle, t.name
		from repo_grants g join teams t on t.id = g.team_id join orgs o on o.id = t.org_id where g.repo_id = $1
		order by 4, 13`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoGrant
	for rows.Next() {
		var g RepoGrant
		var pid, teamID, orgID *uuid.UUID
		var kind, handle, display, email, org, team *string
		var parent *uuid.UUID
		var site *bool
		var created *time.Time
		if err := rows.Scan(&g.Role, &pid, &kind, &handle, &parent, &display, &email, &site, &created, &teamID, &orgID, &org, &team); err != nil {
			return nil, err
		}
		g.RepoID = repo
		if pid != nil {
			g.Principal = &Principal{ID: *pid, Kind: PrincipalKind(*kind), Handle: *handle, ParentID: parent, DisplayName: *display, Email: *email, SiteAdmin: *site, CreatedAt: *created}
		} else {
			g.Team = &Team{ID: *teamID, OrgID: *orgID, Org: *org, Name: *team}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RoleFacts are what a principal's role on one repository is computed
// from, read in one query.
type RoleFacts struct {
	SiteAdmin   bool   // the principal is a site admin
	Grant       string // highest direct or team grant, or ""
	OrgRole     string // role in the repo's organization, or ""
	DefaultRole string // the organization's default role for members
}

// roleRankSQL maps a role column to a number so max() picks the highest.
const roleRankSQL = `case %s when 'admin' then 3 when 'write' then 2 when 'read' then 1 else 0 end`

func rankSQL(col string) string { return fmt.Sprintf(roleRankSQL, col) }

const rankToRole = `(array['read', 'write', 'admin'])[nullif(%s, 0)]`

// roleFactsSQL computes RoleFacts for principal $P and repository r.
// directOnly ($D) leaves out team grants and organization roles.
func roleFactsSQL(p, d string) string {
	best := fmt.Sprintf(`(select max(`+rankSQL("g.role")+`) from repo_grants g where g.repo_id = r.id
		and (g.person_id = %[1]s or (not %[2]s and g.team_id in (select tm.team_id from team_members tm where tm.person_id = %[1]s))))`, p, d)
	return fmt.Sprintf(`coalesce((select site_admin and kind = 'human' from principals where id = %[1]s), false),
		coalesce(`+fmt.Sprintf(rankToRole, best)+`, ''),
		coalesce((select m.role from org_members m where not %[2]s and m.org_id = r.org_id and m.person_id = %[1]s), ''),
		o.default_repo_role`, p, d)
}

// RepoRoleFacts looks up the repository named name together with what
// principal's role on it is computed from, in one query.
func (q Q) RepoRoleFacts(ctx context.Context, name string, principal uuid.UUID, directOnly bool) (Repo, RoleFacts, error) {
	var f RoleFacts
	r, err := scanRepo(q.db.QueryRow(ctx, `select `+repoCols+`, `+roleFactsSQL("$2", "$3")+repoFrom+` where r.name = $1`,
		name, principal, directOnly), &f.SiteAdmin, &f.Grant, &f.OrgRole, &f.DefaultRole)
	return r, f, notFound(err, "repo "+name)
}

// RepoRoleFactsByID is RepoRoleFacts for a repository id.
func (q Q) RepoRoleFactsByID(ctx context.Context, id, principal uuid.UUID, directOnly bool) (Repo, RoleFacts, error) {
	var f RoleFacts
	r, err := scanRepo(q.db.QueryRow(ctx, `select `+repoCols+`, `+roleFactsSQL("$2", "$3")+repoFrom+` where r.id = $1`,
		id, principal, directOnly), &f.SiteAdmin, &f.Grant, &f.OrgRole, &f.DefaultRole)
	return r, f, notFound(err, "repo")
}

// ListRepoRoleFacts returns every repository with what principal's role on
// it is computed from, by name. Callers filter out what it cannot read.
func (q Q) ListRepoRoleFacts(ctx context.Context, principal uuid.UUID, directOnly bool, org *uuid.UUID) ([]Repo, []RoleFacts, error) {
	rows, err := q.db.Query(ctx, `select `+repoCols+`, `+roleFactsSQL("$1", "$2")+repoFrom+`
		where $3::uuid is null or r.org_id = $3 order by r.name`, principal, directOnly, org)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var repos []Repo
	var facts []RoleFacts
	for rows.Next() {
		var f RoleFacts
		r, err := scanRepo(rows, &f.SiteAdmin, &f.Grant, &f.OrgRole, &f.DefaultRole)
		if err != nil {
			return nil, nil, err
		}
		repos = append(repos, r)
		facts = append(facts, f)
	}
	return repos, facts, rows.Err()
}

// ReadableRepoIDs returns the ids of the repositories principal can read
// through grants, teams and organization membership (directOnly: through
// direct grants only). Site admins are not special here; callers check.
// GitHub-backed repositories are left out, except for directOnly
// (service principals keep their direct grants there): roles on them come
// from GitHub, and the service adds them.
func (q Q) ReadableRepoIDs(ctx context.Context, principal uuid.UUID, directOnly bool) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `select x.repo_id from (select repo_id from repo_grants where person_id = $1
		union select g.repo_id from repo_grants g join team_members tm on tm.team_id = g.team_id
			where not $2 and tm.person_id = $1
		union select r.id from repos r join org_members m on m.org_id = r.org_id join orgs o on o.id = r.org_id
			where not $2 and m.person_id = $1 and (m.role in ('owner', 'admin') or o.default_repo_role <> 'none')
		) x join repos r on r.id = x.repo_id
		where $2 or r.github_repo_id is null or r.github_installation_id is null`,
		principal, directOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AccessFact is one source of a principal's role on a repository, for
// listing who has access and why.
type AccessFact struct {
	Principal Principal
	Role      string // read, write or admin
	Via       string // "site admin", "org owner", "org admin", "org default", "user:alice", "team:acme/backend"
}

// RepoAccessFacts returns every source of access to repo: site admins,
// organization owners and admins, the default role for members, direct
// grants and team grants.
func (q Q) RepoAccessFacts(ctx context.Context, repo uuid.UUID) ([]AccessFact, error) {
	cols := prefixed("p", principalCols)
	rows, err := q.db.Query(ctx, `
		select `+cols+`, 'admin', 'site admin' from principals p where p.site_admin and p.kind = 'human'
		union all
		select `+cols+`, case when m.role in ('owner', 'admin') then 'admin' else o.default_repo_role end,
			case m.role when 'owner' then 'org owner' when 'admin' then 'org admin' else 'org default' end
		from repos r join orgs o on o.id = r.org_id join org_members m on m.org_id = o.id
			join principals p on p.id = m.person_id
		where r.id = $1 and (m.role in ('owner', 'admin') or o.default_repo_role <> 'none')
		union all
		select `+cols+`, g.role, case p.kind when 'service' then 'service:' else 'user:' end || p.handle
		from repo_grants g join principals p on p.id = g.person_id where g.repo_id = $1
		union all
		select `+cols+`, g.role, 'team:' || o.handle || '/' || t.name
		from repo_grants g join teams t on t.id = g.team_id join orgs o on o.id = t.org_id
			join team_members tm on tm.team_id = t.id join principals p on p.id = tm.person_id
		where g.repo_id = $1`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessFact
	for rows.Next() {
		var f AccessFact
		p := &f.Principal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Handle, &p.ParentID, &p.DisplayName, &p.Email, &p.SiteAdmin, &p.CreatedAt, &f.Role, &f.Via); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetSiteAdmin makes a person a site admin or not.
func (q Q) SetSiteAdmin(ctx context.Context, person uuid.UUID, admin bool) error {
	tag, err := q.db.Exec(ctx, `update principals set site_admin = $2 where id = $1 and kind = 'human'`, person, admin)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("person: %w", ErrNotFound)
	}
	return err
}

// ListPeople returns every person (KindHuman) by handle.
func (q Q) ListPeople(ctx context.Context) ([]Principal, error) {
	rows, err := q.db.Query(ctx, `select `+principalCols+` from principals where kind = 'human' order by handle`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// prefixed qualifies a comma-separated column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
