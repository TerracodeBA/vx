package service

// Organization, team, repository grant and people management.
//
//   - Only people manage organizations, teams and grants; agents are
//     refused, like token creation.
//   - Any person may create an organization and becomes its owner.
//   - Owners and admins change an organization, its members and its
//     teams (superadmins see every organization but change none they do
//     not own or administer: adding themselves would give them its
//     repositories); only owners make, change or remove owners;
//     the last owner cannot be removed or demoted. Anyone may leave an
//     organization, except its last owner.
//   - Personal organizations have one member, their person.
//   - Repository admins grant and revoke roles on the repository, and
//     support access for superadmins (support.go).
//   - Superadmins create people and see every person and organization.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// peopleOnly refuses agents and service principals for management
// actions.
func peopleOnly(actor Actor, what string) error {
	if actor.Kind == store.KindHuman {
		return nil
	}
	if actor.Parent != nil {
		return errf(CodePermission, "only people can %s; %s is an agent acting for %s. Ask %s to do it", what, actor.Ref(), actor.Parent.Ref(), actor.Parent.Ref())
	}
	return errf(CodePermission, "only people can %s; %s is a %s", what, actor.Ref(), actor.Kind)
}

func orgFromStore(o store.Org) Org {
	role := RepoRole(o.DefaultRepoRole)
	if o.DefaultRepoRole == "none" {
		role = RoleNone
	}
	return Org{ID: o.ID, Handle: o.Handle, DisplayName: o.DisplayName, DefaultRepoRole: role, Personal: o.Personal, CreatedAt: o.CreatedAt}
}

func orgNotFound(handle string) error {
	return classify(fmt.Errorf("organization %s: %w", handle, store.ErrNotFound))
}

// orgAccess is an organization as seen by the caller.
type orgAccess struct {
	org        store.Org
	role       OrgRole // the caller's role, "" when not a member
	superadmin bool    // sees the organization; changes nothing for it
}

func (a orgAccess) canManage() bool {
	return a.role == OrgOwner || a.role == OrgAdmin
}

func (a orgAccess) isOwner() bool { return a.role == OrgOwner }

// loadOrg reads an organization the caller can see (members and
// superadmins; anyone else gets not found), locking it inside transactions.
func (s *Service) loadOrg(ctx context.Context, q *store.Tx, actor Actor, handle string) (orgAccess, error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	var o store.Org
	var err error
	if q != nil {
		o, err = q.LockOrg(ctx, handle)
	} else {
		o, err = s.Store.OrgByHandle(ctx, handle)
	}
	if err != nil {
		return orgAccess{}, classify(err)
	}
	h := actor.Human()
	var role string
	if q != nil {
		role, err = q.OrgRole(ctx, o.ID, h.ID)
	} else {
		role, err = s.Store.OrgRole(ctx, o.ID, h.ID)
	}
	if err != nil {
		return orgAccess{}, err
	}
	a := orgAccess{org: o, role: OrgRole(role), superadmin: s.IsSuperadmin(ctx, actor)}
	if a.role == "" && !a.superadmin {
		return orgAccess{}, orgNotFound(handle)
	}
	return a, nil
}

// manageable is loadOrg for changes: owners and admins.
func (s *Service) manageable(ctx context.Context, q *store.Tx, actor Actor, handle, what string) (orgAccess, error) {
	a, err := s.loadOrg(ctx, q, actor, handle)
	if err != nil {
		return a, err
	}
	if !a.canManage() && a.role == "" {
		return a, errf(CodePermission, "%s needs owner or admin in organization %s; %s is a superadmin, which lets them see organizations but not change them. Ask an owner (%s)",
			what, a.org.Handle, actor.Human().Ref(), s.orgOwnerRefs(ctx, a.org.ID))
	}
	if !a.canManage() {
		return a, errf(CodePermission, "%s needs owner or admin in organization %s; %s is a %s. Ask an owner (%s)",
			what, a.org.Handle, actor.Human().Ref(), a.role, s.orgOwnerRefs(ctx, a.org.ID))
	}
	return a, nil
}

// resolvePerson finds a person by handle ("alice") or ref ("user:alice").
func (s *Service) resolvePerson(ctx context.Context, q *store.Tx, ref string) (store.Principal, error) {
	ref = strings.TrimSpace(ref)
	handle := strings.TrimPrefix(ref, "user:")
	if strings.Contains(handle, ":") || strings.Contains(handle, "/") {
		return store.Principal{}, errf(CodeInvalid, "%q is not a person: give a handle like alice or user:alice", ref)
	}
	p, err := q.PrincipalByHandle(ctx, handle)
	if err != nil {
		return store.Principal{}, classify(err)
	}
	if p.Kind != store.KindHuman {
		return store.Principal{}, errf(CodeInvalid, "%s is a %s, not a person", p.Ref(), p.Kind)
	}
	return p, nil
}

func validOrgRole(r OrgRole) error {
	switch r {
	case OrgOwner, OrgAdmin, OrgMember:
		return nil
	}
	return errf(CodeInvalid, "organization role %q: use owner, admin or member", r)
}

// CreateOrg creates an organization owned by the calling person.
func (s *Service) CreateOrg(ctx context.Context, actor Actor, handle, displayName string) (Org, error) {
	if err := peopleOnly(actor, "create organizations"); err != nil {
		return Org{}, err
	}
	handle = strings.TrimSpace(handle)
	if err := store.ValidateHandle(store.KindHuman, handle); err != nil {
		return Org{}, errf(CodeInvalid, "organization %v", err)
	}
	var o store.Org
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		if _, err := tx.PrincipalByHandle(ctx, handle); err == nil {
			return errf(CodeExists, "%s is a person's handle; pick another organization handle", handle)
		}
		var err error
		if o, err = tx.CreateOrg(ctx, handle, strings.TrimSpace(displayName), actor.ID); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "org.create", o.Handle, nil, o)
	})
	return orgFromStore(o), classify(err)
}

// ListOrgs lists the caller's organizations, or all of them for
// superadmins with all set.
func (s *Service) ListOrgs(ctx context.Context, actor Actor, all bool) ([]Org, []OrgRole, error) {
	if all && !s.IsSuperadmin(ctx, actor) {
		return nil, nil, errf(CodePermission, "only superadmins can list every organization; leave out --all to list yours")
	}
	rows, err := s.Store.ListOrgs(ctx, actor.Human().ID, all)
	if err != nil {
		return nil, nil, classify(err)
	}
	orgs := make([]Org, len(rows))
	roles := make([]OrgRole, len(rows))
	for i, r := range rows {
		orgs[i], roles[i] = orgFromStore(r.Org), OrgRole(r.Role)
	}
	return orgs, roles, nil
}

// GetOrg returns an organization the caller is a member of (or any, for
// superadmins, with only the repositories they can read).
func (s *Service) GetOrg(ctx context.Context, actor Actor, handle string) (OrgDetail, error) {
	a, err := s.loadOrg(ctx, nil, actor, handle)
	if err != nil {
		return OrgDetail{}, err
	}
	d := OrgDetail{Org: orgFromStore(a.org), MyRole: a.role}
	members, err := s.Store.ListOrgMembers(ctx, a.org.ID)
	if err != nil {
		return OrgDetail{}, err
	}
	for _, m := range members {
		d.Members = append(d.Members, Membership{Person: m.Person, Role: OrgRole(m.Role), Teams: m.Teams})
	}
	teams, err := s.Store.ListTeams(ctx, a.org)
	if err != nil {
		return OrgDetail{}, err
	}
	for _, t := range teams {
		d.Teams = append(d.Teams, teamFromStore(t))
	}
	d.Repos, _, err = s.ReadableRepos(ctx, actor, &a.org.ID)
	return d, err
}

func teamFromStore(t store.Team) Team {
	return Team{ID: t.ID, OrgID: t.OrgID, Org: t.Org, Name: t.Name, Members: t.Members}
}

// UpdateOrg changes an organization's display name or default repository
// role (owners and admins). Empty values are unchanged.
func (s *Service) UpdateOrg(ctx context.Context, actor Actor, handle, displayName string, defaultRole *RepoRole) (Org, error) {
	if err := peopleOnly(actor, "change organizations"); err != nil {
		return Org{}, err
	}
	var name, role *string
	if d := strings.TrimSpace(displayName); d != "" {
		name = &d
	}
	if defaultRole != nil {
		var r string
		switch *defaultRole {
		case RoleNone, "none":
			r = "none"
		case RoleRead, RoleWrite:
			r = string(*defaultRole)
		default:
			return Org{}, errf(CodeInvalid, "default repository role %q: use none, read or write (owners and admins are always admin)", *defaultRole)
		}
		role = &r
	}
	var out store.Org
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.manageable(ctx, tx, actor, handle, "changing the organization")
		if err != nil {
			return err
		}
		if out, err = tx.UpdateOrg(ctx, a.org.ID, name, role); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "org.update", out.Handle, a.org, out)
	})
	return orgFromStore(out), classify(err)
}

type memberChange struct {
	Person string `json:"person"`
	Role   string `json:"role,omitempty"`
}

// SetMember adds a person to an organization or changes their role.
// Owners and admins may; only owners make owners; the last owner stays.
func (s *Service) SetMember(ctx context.Context, actor Actor, org, person string, role OrgRole) (Membership, error) {
	if err := peopleOnly(actor, "manage organization members"); err != nil {
		return Membership{}, err
	}
	if err := validOrgRole(role); err != nil {
		return Membership{}, err
	}
	var out Membership
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.manageable(ctx, tx, actor, org, "adding or changing members")
		if err != nil {
			return err
		}
		if a.org.GitHubLinked() {
			return s.refuseOnGitHubOrg(a.org, "add members or change their roles")
		}
		if a.org.Personal {
			return errf(CodeFailedPrecondition, "%s is a personal organization and has only its person; create a shared one with `vx org create HANDLE`, or grant access to single repositories with `vx repo grant`", a.org.Handle)
		}
		p, err := s.resolvePerson(ctx, tx, person)
		if err != nil {
			return err
		}
		prev, err := tx.OrgRole(ctx, a.org.ID, p.ID)
		if err != nil {
			return err
		}
		if (role == OrgOwner || OrgRole(prev) == OrgOwner) && OrgRole(prev) != role && !a.isOwner() {
			return errf(CodePermission, "only owners of %s can make or change owners; ask an owner (%s)", a.org.Handle, s.orgOwnerRefs(ctx, a.org.ID))
		}
		if OrgRole(prev) == OrgOwner && role != OrgOwner {
			if err := lastOwnerCheck(ctx, tx, a.org, p, "demoted"); err != nil {
				return err
			}
		}
		if err := tx.SetOrgMember(ctx, a.org.ID, p.ID, string(role)); err != nil {
			return err
		}
		var before any
		if prev != "" {
			before = memberChange{Person: p.Ref(), Role: prev}
		}
		if err := tx.LogOp(ctx, nil, &actor.ID, "org.member", a.org.Handle, before, memberChange{Person: p.Ref(), Role: string(role)}); err != nil {
			return err
		}
		out = Membership{Person: p, Role: role}
		members, err := tx.ListOrgMembers(ctx, a.org.ID)
		for _, m := range members {
			if m.Person.ID == p.ID {
				out.Teams = m.Teams
			}
		}
		return err
	})
	return out, classify(err)
}

func lastOwnerCheck(ctx context.Context, tx *store.Tx, org store.Org, p store.Principal, what string) error {
	n, err := tx.CountOwners(ctx, org.ID)
	if err != nil {
		return err
	}
	if n <= 1 {
		return errf(CodeFailedPrecondition, "%s is the last owner of %s and cannot be %s; make someone else an owner first: vx org member %s PERSON --role owner", p.Ref(), org.Handle, what, org.Handle)
	}
	return nil
}

// RemoveMember removes a person from an organization and its teams.
func (s *Service) RemoveMember(ctx context.Context, actor Actor, org, person string) error {
	if err := peopleOnly(actor, "manage organization members"); err != nil {
		return err
	}
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.loadOrg(ctx, tx, actor, org)
		if err != nil {
			return err
		}
		if a.org.GitHubLinked() {
			return s.refuseOnGitHubOrg(a.org, "remove members")
		}
		p, err := s.resolvePerson(ctx, tx, person)
		if err != nil {
			return err
		}
		self := p.ID == actor.ID
		if !self && !a.canManage() {
			return errf(CodePermission, "removing members needs owner or admin in organization %s; %s is a %s", a.org.Handle, actor.Ref(), a.role)
		}
		prev, err := tx.OrgRole(ctx, a.org.ID, p.ID)
		if err != nil {
			return err
		}
		if prev == "" {
			return errf(CodeNotFound, "%s is not a member of %s", p.Ref(), a.org.Handle)
		}
		if OrgRole(prev) == OrgOwner {
			if !self && !a.isOwner() {
				return errf(CodePermission, "only owners of %s can remove an owner", a.org.Handle)
			}
			if err := lastOwnerCheck(ctx, tx, a.org, p, "removed"); err != nil {
				return err
			}
		}
		if _, err := tx.RemoveOrgMember(ctx, a.org.ID, p.ID); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "org.member", a.org.Handle, memberChange{Person: p.Ref(), Role: prev}, nil)
	})
	return classify(err)
}

// CreateTeam, DeleteTeam, SetTeamMember and RemoveTeamMember manage teams
// (org owners and admins).
func (s *Service) CreateTeam(ctx context.Context, actor Actor, org, name string) (Team, error) {
	if err := peopleOnly(actor, "manage teams"); err != nil {
		return Team{}, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	var t store.Team
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.manageable(ctx, tx, actor, org, "creating teams")
		if err != nil {
			return err
		}
		if a.org.GitHubLinked() {
			return s.refuseOnGitHubOrg(a.org, "manage teams")
		}
		if a.org.Personal {
			return errf(CodeFailedPrecondition, "%s is a personal organization and has no teams; create a shared one with `vx org create HANDLE`", a.org.Handle)
		}
		if t, err = tx.CreateTeam(ctx, a.org, name); err != nil {
			if !errors.Is(err, store.ErrExists) {
				return errf(CodeInvalid, "%v", err)
			}
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "team.create", t.Org+"/"+t.Name, nil, nil)
	})
	return teamFromStore(t), classify(err)
}

func (s *Service) DeleteTeam(ctx context.Context, actor Actor, org, name string) error {
	if err := peopleOnly(actor, "manage teams"); err != nil {
		return err
	}
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.manageable(ctx, tx, actor, org, "deleting teams")
		if err != nil {
			return err
		}
		if a.org.GitHubLinked() {
			return s.refuseOnGitHubOrg(a.org, "manage teams")
		}
		t, err := tx.TeamByName(ctx, a.org, strings.ToLower(strings.TrimSpace(name)))
		if err != nil {
			return err
		}
		if err := tx.DeleteTeam(ctx, t.ID); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "team.delete", t.Org+"/"+t.Name, teamFromStore(t), nil)
	})
	return classify(err)
}

func (s *Service) SetTeamMember(ctx context.Context, actor Actor, org, team, person string) (Team, error) {
	return s.changeTeam(ctx, actor, org, team, person, true)
}

func (s *Service) RemoveTeamMember(ctx context.Context, actor Actor, org, team, person string) (Team, error) {
	return s.changeTeam(ctx, actor, org, team, person, false)
}

func (s *Service) changeTeam(ctx context.Context, actor Actor, org, team, person string, add bool) (Team, error) {
	if err := peopleOnly(actor, "manage teams"); err != nil {
		return Team{}, err
	}
	var out store.Team
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.manageable(ctx, tx, actor, org, "changing teams")
		if err != nil {
			return err
		}
		if a.org.GitHubLinked() {
			return s.refuseOnGitHubOrg(a.org, "manage teams")
		}
		t, err := tx.TeamByName(ctx, a.org, strings.ToLower(strings.TrimSpace(team)))
		if err != nil {
			return err
		}
		p, err := s.resolvePerson(ctx, tx, person)
		if err != nil {
			return err
		}
		op := "team.add"
		if add {
			if err := tx.AddTeamMember(ctx, t, p.ID); errors.Is(err, store.ErrNotFound) {
				return errf(CodeFailedPrecondition, "%s is not a member of %s; add them first: vx org member %s %s --role member", p.Ref(), a.org.Handle, a.org.Handle, p.Handle)
			} else if err != nil {
				return err
			}
		} else {
			op = "team.remove"
			removed, err := tx.RemoveTeamMember(ctx, t.ID, p.ID)
			if err != nil {
				return err
			}
			if !removed {
				return errf(CodeNotFound, "%s is not in team %s/%s", p.Ref(), t.Org, t.Name)
			}
		}
		if err := tx.LogOp(ctx, nil, &actor.ID, op, t.Org+"/"+t.Name, nil, memberChange{Person: p.Ref()}); err != nil {
			return err
		}
		out, err = tx.TeamByName(ctx, a.org, t.Name)
		return err
	})
	return teamFromStore(out), classify(err)
}

// grantSubject is who a grant is for: a person or service principal, or a
// team of the repository's organization.
type grantSubject struct {
	principal *store.Principal
	team      *store.Team
}

func (g grantSubject) ref() string {
	if g.team != nil {
		return "team:" + g.team.Org + "/" + g.team.Name
	}
	return g.principal.Ref()
}

func (g grantSubject) ids() (*uuid.UUID, *uuid.UUID) {
	if g.team != nil {
		return nil, &g.team.ID
	}
	return &g.principal.ID, nil
}

// parseSubject resolves "user:alice", "alice", "service:ci" or
// "team:acme/backend" for a grant on repo.
func (s *Service) parseSubject(ctx context.Context, tx *store.Tx, repo store.Repo, subject string) (grantSubject, error) {
	subject = strings.TrimSpace(subject)
	kind, rest, ok := strings.Cut(subject, ":")
	if !ok {
		kind, rest = "user", subject
	}
	switch kind {
	case "team":
		orgHandle, name, ok := strings.Cut(rest, "/")
		if !ok {
			return grantSubject{}, errf(CodeInvalid, "team %q: write it as team:ORG/NAME", subject)
		}
		if orgHandle != repo.Org {
			return grantSubject{}, errf(CodeFailedPrecondition, "team %s belongs to %s, but %s belongs to %s; use a team of %s or grant people directly", rest, orgHandle, repo.Name, repo.Org, repo.Org)
		}
		o, err := tx.OrgByHandle(ctx, orgHandle)
		if err != nil {
			return grantSubject{}, err
		}
		t, err := tx.TeamByName(ctx, o, name)
		if err != nil {
			return grantSubject{}, err
		}
		return grantSubject{team: &t}, nil
	case "user", "service":
		p, err := tx.PrincipalByHandle(ctx, rest)
		if err != nil {
			return grantSubject{}, err
		}
		want := store.KindHuman
		if kind == "service" {
			want = store.KindService
		}
		if p.Kind == store.KindAgent {
			return grantSubject{}, errf(CodeInvalid, "agents act with their person's role; grant %s instead", "user:"+strings.SplitN(p.Handle, "/", 2)[0])
		}
		if p.Kind != want && strings.Contains(subject, ":") {
			return grantSubject{}, errf(CodeInvalid, "%s is a %s; write it as %s", subject, p.Kind, p.Ref())
		}
		return grantSubject{principal: &p}, nil
	case "agent":
		owner, _, _ := strings.Cut(rest, "/")
		return grantSubject{}, errf(CodeInvalid, "agents act with their person's role; grant user:%s instead", owner)
	}
	return grantSubject{}, errf(CodeInvalid, "grant subject %q: use user:HANDLE, service:HANDLE or team:ORG/NAME", subject)
}

type grantChange struct {
	Subject string `json:"subject"`
	Role    string `json:"role"`
}

// GrantRepo gives a person or team a role on a repository (repo admins).
// On a GitHub-backed repository only service principals can be granted:
// people's roles come from GitHub.
func (s *Service) GrantRepo(ctx context.Context, actor Actor, repo, subject string, role RepoRole) (RepoGrant, error) {
	if err := peopleOnly(actor, "grant repository roles"); err != nil {
		return RepoGrant{}, err
	}
	switch role {
	case RoleRead, RoleWrite, RoleAdmin:
	default:
		return RepoGrant{}, errf(CodeInvalid, "repository role %q: use read, write or admin", role)
	}
	r, _, err := s.Authorize(ctx, actor, repo, RoleAdmin)
	if err != nil {
		return RepoGrant{}, err
	}
	var out RepoGrant
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		sub, err := s.parseSubject(ctx, tx, r, subject)
		if err != nil {
			return err
		}
		if r.GitHubBacked() && (sub.team != nil || sub.principal.Kind != store.KindService) {
			return s.refuseOnGitHubRepo(r, "grant roles to people or teams")
		}
		pid, tid := sub.ids()
		prev, err := tx.SetRepoGrant(ctx, r.ID, pid, tid, string(role))
		if err != nil {
			return err
		}
		var before any
		if prev != "" {
			before = grantChange{Subject: sub.ref(), Role: prev}
		}
		out = RepoGrant{Repo: r.Name, Subject: sub.ref(), Role: role}
		return tx.LogOp(ctx, &r.ID, &actor.ID, "repo.grant", r.Name, before, grantChange{Subject: sub.ref(), Role: string(role)})
	})
	return out, classify(err)
}

// RevokeRepo removes a direct grant (repo admins). It also works on
// GitHub-backed repositories, to clean up grants made before the
// repository was linked (they have no effect there).
func (s *Service) RevokeRepo(ctx context.Context, actor Actor, repo, subject string) error {
	if err := peopleOnly(actor, "revoke repository roles"); err != nil {
		return err
	}
	r, _, err := s.Authorize(ctx, actor, repo, RoleAdmin)
	if err != nil {
		return err
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		sub, err := s.parseSubject(ctx, tx, r, subject)
		if err != nil {
			return err
		}
		pid, tid := sub.ids()
		prev, err := tx.DeleteRepoGrant(ctx, r.ID, pid, tid)
		if err != nil {
			return err
		}
		if prev == "" {
			return errf(CodeNotFound, "%s has no direct grant on %s; `vx repo access %s` lists them", sub.ref(), r.Name, r.Name)
		}
		return tx.LogOp(ctx, &r.ID, &actor.ID, "repo.revoke", r.Name, grantChange{Subject: sub.ref(), Role: prev}, nil)
	})
	return classify(err)
}

// RepoAccess lists a repository's direct grants and everyone's effective
// role (repo admins).
func (s *Service) RepoAccess(ctx context.Context, actor Actor, repo string) ([]RepoGrant, []AccessEntry, error) {
	r, _, err := s.Authorize(ctx, actor, repo, RoleAdmin)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.Store.ListRepoGrants(ctx, r.ID)
	if err != nil {
		return nil, nil, err
	}
	if r.GitHubBacked() {
		grants, entries, err := s.githubRepoAccess(ctx, r, rows)
		if err != nil {
			return nil, nil, err
		}
		entries, err = s.withSupportAccess(ctx, r, entries)
		return grants, entries, err
	}
	var grants []RepoGrant
	for _, g := range rows {
		sub := grantSubject{principal: g.Principal, team: g.Team}
		grants = append(grants, RepoGrant{Repo: r.Name, Subject: sub.ref(), Role: RepoRole(g.Role)})
	}
	facts, err := s.Store.RepoAccessFacts(ctx, r.ID)
	if err != nil {
		return nil, nil, err
	}
	byID := map[uuid.UUID]*AccessEntry{}
	var order []uuid.UUID
	for _, f := range facts {
		e := byID[f.Principal.ID]
		if e == nil {
			e = &AccessEntry{Person: f.Principal}
			byID[f.Principal.ID] = e
			order = append(order, f.Principal.ID)
		}
		if role := RepoRole(f.Role); !e.Role.Includes(role) {
			e.Role = role
		}
		e.Via = append(e.Via, f.Via)
	}
	entries := make([]AccessEntry, 0, len(order))
	for _, id := range order {
		e := byID[id]
		sort.Strings(e.Via)
		entries = append(entries, *e)
	}
	entries, err = s.withSupportAccess(ctx, r, entries)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Person.Handle < entries[j].Person.Handle })
	return grants, entries, nil
}

// NewPerson describes a person for AddPerson.
type NewPerson struct {
	Handle      string
	DisplayName string
	Email       string
	SiteAdmin   bool
	// Org, when set, adds the person to this organization as a member.
	Org string
	// CreateOrg creates Org with the person as its owner when it does not
	// exist yet (vx-server user-add on a new server).
	CreateOrg bool
}

// AddPerson creates a person, their personal organization and a first
// token, without checking who asks: CreatePerson checks, and vx-server
// user-add runs it with by nil. It reports whether it created n.Org.
func (s *Service) AddPerson(ctx context.Context, by *store.Principal, n NewPerson) (store.Principal, string, bool, error) {
	n.Handle = strings.TrimSpace(n.Handle)
	if err := store.ValidateHandle(store.KindHuman, n.Handle); err != nil {
		return store.Principal{}, "", false, errf(CodeInvalid, "%v", err)
	}
	var byID *uuid.UUID
	tokenName := "created by vx-server user-add"
	if by != nil {
		byID = &by.ID
		tokenName = "created by " + by.Ref()
	}
	var p store.Principal
	var token string
	var created bool
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		p, err = tx.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: n.Handle, DisplayName: strings.TrimSpace(n.DisplayName), Email: strings.TrimSpace(n.Email), SiteAdmin: n.SiteAdmin})
		if err != nil {
			return err
		}
		if org := strings.ToLower(strings.TrimSpace(n.Org)); org != "" {
			o, err := tx.LockOrg(ctx, org)
			switch {
			case errors.Is(err, store.ErrNotFound) && n.CreateOrg:
				if o, err = tx.CreateOrg(ctx, org, "", p.ID); err != nil {
					return err
				}
				created = true
				if err := tx.LogOp(ctx, nil, &p.ID, "org.create", o.Handle, nil, o); err != nil {
					return err
				}
			case err != nil:
				return err
			case o.Personal:
				return errf(CodeFailedPrecondition, "%s is a personal organization and has only its person", o.Handle)
			default:
				if err := tx.SetOrgMember(ctx, o.ID, p.ID, string(OrgMember)); err != nil {
					return err
				}
				if err := tx.LogOp(ctx, nil, byID, "org.member", o.Handle, nil, memberChange{Person: p.Ref(), Role: string(OrgMember)}); err != nil {
					return err
				}
			}
		}
		if token, err = tx.CreateToken(ctx, p.ID, tokenName); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, byID, "person.create", p.Ref(), nil, map[string]any{"site_admin": p.SiteAdmin, "org": n.Org})
	})
	if err != nil {
		return store.Principal{}, "", false, classify(err)
	}
	return p, token, created, nil
}

// CreatePerson creates a person with a first token (superadmins),
// optionally as a member of an organization (a superadmin may add people
// to any organization: that gives the new person a role, never the
// superadmin).
func (s *Service) CreatePerson(ctx context.Context, actor Actor, handle, displayName, email string, siteAdmin bool, org string) (store.Principal, string, error) {
	if err := peopleOnly(actor, "create people"); err != nil {
		return store.Principal{}, "", err
	}
	if !s.IsSuperadmin(ctx, actor) {
		return store.Principal{}, "", errf(CodePermission, "only superadmins can create people; %s is not one. Ask a superadmin, or run `vx-server user-add` on the server", actor.Ref())
	}
	p, token, _, err := s.AddPerson(ctx, &actor.Principal, NewPerson{Handle: handle, DisplayName: displayName, Email: email, SiteAdmin: siteAdmin, Org: org})
	return p, token, err
}

// ListPeople lists every person on the server (superadmins).
func (s *Service) ListPeople(ctx context.Context, actor Actor) ([]store.Principal, error) {
	if !s.IsSuperadmin(ctx, actor) {
		return nil, errf(CodePermission, "only superadmins can list everyone on the server; %s is not one", actor.Human().Ref())
	}
	people, err := s.Store.ListPeople(ctx)
	return people, classify(err)
}
