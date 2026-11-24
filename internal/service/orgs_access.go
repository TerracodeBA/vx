package service

// Who may do what in a repository. The rules:
//
//   - A person's role on a repository is the highest of: a direct grant to
//     them, a grant to any team they are in, the organization's default
//     repository role (for its members), and admin for the organization's
//     owners and admins.
//   - Superadmins (principals.site_admin) run the server and have no
//     implicit role on any repository: only what grants, teams and
//     organizations give them like anyone else, and support grants, a
//     time-limited role a repository's admins give them (support.go).
//   - Agents have exactly their person's role.
//   - Service principals have only their direct grants.
//   - On GitHub-backed repositories none of the above but support grants
//     counts: the role comes from GitHub (github_roles.go).
//
// A repository the caller cannot read is reported exactly like one that
// does not exist; one it can read but needs more on is a permission error
// that names the role needed and who can grant it.

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// roleSubject is whose role counts for actor: the person behind an agent,
// otherwise the actor itself. directOnly is set for service principals,
// who have their direct grants and nothing else.
func roleSubject(actor Actor) (id uuid.UUID, directOnly bool) {
	h := actor.Human()
	return h.ID, h.Kind == store.KindService
}

// effectiveRole computes a role from what the store read in one query.
func effectiveRole(f store.RoleFacts) RepoRole {
	best := higher(RepoRole(f.Grant), RepoRole(f.Support))
	switch OrgRole(f.OrgRole) {
	case OrgOwner, OrgAdmin:
		return RoleAdmin
	case OrgMember:
		if d := RepoRole(f.DefaultRole); f.DefaultRole != "none" && !best.Includes(d) {
			best = d
		}
	}
	return best
}

// higher is the higher of two roles.
func higher(a, b RepoRole) RepoRole {
	if a.Includes(b) {
		return a
	}
	return b
}

// repoNotFound is the error for a repository that does not exist or that
// the caller cannot read; both read exactly the same.
func repoNotFound(name string) error {
	return classify(fmt.Errorf("repo %s: %w", name, store.ErrNotFound))
}

// Authorize returns the repository named repo if actor has at least need on
// it. A repository the actor cannot read is reported as not found, so its
// existence does not leak; one it can read but needs more on is a
// permission error (the repository and the actor's role come back with it).
// Agents are checked as the person they act for.
//
// Every service function that takes a repository name calls this first.
// It runs one query, plus a cache lookup (and when that is stale, a call
// to GitHub) for GitHub-backed repositories.
func (s *Service) Authorize(ctx context.Context, actor Actor, repo string, need RepoRole) (store.Repo, RepoRole, error) {
	subject, direct := roleSubject(actor)
	r, facts, err := s.Store.RepoRoleFacts(ctx, repo, subject, direct, s.now())
	if err != nil {
		return store.Repo{}, RoleNone, classify(err)
	}
	role, err := s.roleFrom(ctx, actor, r, facts)
	if err != nil {
		return store.Repo{}, RoleNone, err
	}
	if role == RoleNone {
		return store.Repo{}, RoleNone, repoNotFound(repo)
	}
	if err := s.requireSSO(ctx, actor, r, facts); err != nil {
		return store.Repo{}, RoleNone, err
	}
	if !role.Includes(need) {
		return r, role, s.needRole(ctx, actor, r, role, need)
	}
	return r, role, nil
}

// AuthorizeID is Authorize for a repository id (for objects, workspaces and
// other records that carry a repo id).
func (s *Service) AuthorizeID(ctx context.Context, actor Actor, repoID uuid.UUID, need RepoRole) (RepoRole, error) {
	subject, direct := roleSubject(actor)
	r, facts, err := s.Store.RepoRoleFactsByID(ctx, repoID, subject, direct, s.now())
	if err != nil {
		return RoleNone, classify(err)
	}
	role, err := s.roleFrom(ctx, actor, r, facts)
	if err != nil {
		return RoleNone, err
	}
	if role == RoleNone {
		return RoleNone, classify(fmt.Errorf("repo: %w", store.ErrNotFound))
	}
	if err := s.requireSSO(ctx, actor, r, facts); err != nil {
		return RoleNone, err
	}
	if !role.Includes(need) {
		return role, s.needRole(ctx, actor, r, role, need)
	}
	return role, nil
}

// RepoRoleOf returns actor's role on r without checking anything, for
// filling my_role in responses. It is RoleNone when the actor cannot read r.
func (s *Service) RepoRoleOf(ctx context.Context, actor Actor, r store.Repo) RepoRole {
	subject, direct := roleSubject(actor)
	repo, facts, err := s.Store.RepoRoleFactsByID(ctx, r.ID, subject, direct, s.now())
	if err != nil {
		return RoleNone
	}
	role, err := s.roleFrom(ctx, actor, repo, facts)
	if err != nil {
		return RoleNone
	}
	return role
}

// roleFrom is actor's role on r: from vx's own facts, or for a
// GitHub-backed repository from GitHub (service principals keep their
// direct grants, support grants count, vx grants count for nothing else).
func (s *Service) roleFrom(ctx context.Context, actor Actor, r store.Repo, facts store.RoleFacts) (RepoRole, error) {
	if !r.GitHubBacked() {
		return effectiveRole(facts), nil
	}
	if _, direct := roleSubject(actor); direct {
		return RepoRole(facts.Grant), nil
	}
	role, _, err := s.GitHubRole(ctx, r, actor.Human())
	if support := RepoRole(facts.Support); support != RoleNone {
		// A support grant does not depend on GitHub answering.
		return higher(role, support), nil
	}
	return role, err
}

// ReadableRepos lists the repositories actor can read, by name, with its
// role on each, in one query. org limits it to one organization's
// repositories; nil lists them all.
func (s *Service) ReadableRepos(ctx context.Context, actor Actor, org *uuid.UUID) ([]store.Repo, []RepoRole, error) {
	subject, direct := roleSubject(actor)
	repos, facts, err := s.Store.ListRepoRoleFacts(ctx, subject, direct, org, s.now())
	if err != nil {
		return nil, nil, classify(err)
	}
	var gh []store.Repo
	for _, r := range repos {
		if r.GitHubBacked() && !direct {
			gh = append(gh, r)
		}
	}
	var ghRoles map[uuid.UUID]RepoRole
	if len(gh) > 0 {
		sub, err := s.githubSubjectOf(ctx, actor.Human())
		if err != nil {
			return nil, nil, err
		}
		ghRoles, _ = s.githubRoles(ctx, sub, gh, true) // unanswered ones are left out
	}
	var outR []store.Repo
	var outRoles []RepoRole
	for i, r := range repos {
		role := effectiveRole(facts[i])
		if r.GitHubBacked() {
			if direct {
				role = RepoRole(facts[i].Grant)
			} else {
				role = higher(ghRoles[r.ID], RepoRole(facts[i].Support))
			}
		}
		if role != RoleNone {
			outR = append(outR, r)
			outRoles = append(outRoles, role)
		}
	}
	return outR, outRoles, nil
}

// ReadableRepoIDs returns the ids of every repository actor can read, for
// filtering lists. all would mean every repository; nobody has that any
// more (superadmins have no implicit role), so it is always false, and
// callers keep handling it.
func (s *Service) ReadableRepoIDs(ctx context.Context, actor Actor) (ids []uuid.UUID, all bool, err error) {
	subject, direct := roleSubject(actor)
	ids, err = s.Store.ReadableRepoIDs(ctx, subject, direct, s.now())
	if err != nil || direct {
		return ids, false, classify(err)
	}
	sub, err := s.githubSubjectOf(ctx, actor.Human())
	if err != nil {
		return nil, false, err
	}
	gh, _, err := s.githubReadable(ctx, sub, nil)
	if err != nil {
		return nil, false, err
	}
	for _, r := range gh {
		ids = append(ids, r.ID)
	}
	return ids, false, nil
}

// IsSuperadmin reports whether actor (or, for agents, its person) is a
// superadmin (principals.site_admin): someone who runs the server through
// the admin panel. It gives no role on any repository. It reads the
// database, so a change applies at once.
func (s *Service) IsSuperadmin(ctx context.Context, actor Actor) bool {
	h := actor.Human()
	if h.Kind != store.KindHuman {
		return false
	}
	p, err := s.Store.PrincipalByID(ctx, h.ID)
	return err == nil && p.SiteAdmin
}

// IsSiteAdmin is IsSuperadmin under its old name.
//
// Deprecated: use IsSuperadmin.
func (s *Service) IsSiteAdmin(ctx context.Context, actor Actor) bool {
	return s.IsSuperadmin(ctx, actor)
}

// needRole is the permission error for an actor that can read r but needs
// more: it names the role needed and who can grant it.
func (s *Service) needRole(ctx context.Context, actor Actor, r store.Repo, have, need RepoRole) error {
	who := actor.Ref()
	if actor.Parent != nil {
		who = fmt.Sprintf("%s acts for %s, who", actor.Ref(), actor.Parent.Ref())
	}
	if r.GitHubBacked() && actor.Human().Kind == store.KindHuman {
		return errf(CodePermission, "%s has %s on %s and this needs %s. Roles on %s come from GitHub: an admin of the repository changes them in GitHub (%s); vx sees the change within %s, at once with webhooks",
			who, have, r.Name, need, r.Name, s.GitHubSettingsURL(r), GitHubRoleTTL)
	}
	granters := "an owner or admin of organization " + r.Org
	if owners := s.orgOwnerRefs(ctx, r.OrgID); owners != "" {
		granters += " (" + owners + ")"
	}
	return errf(CodePermission, "%s has %s on %s and this needs %s. %s or an admin of %s can grant it: vx repo grant %s %s --role %s",
		who, have, r.Name, need, upperFirst(granters), r.Name, r.Name, actor.Human().Ref(), need)
}

// orgOwnerRefs names up to three owners of an organization, for messages.
func (s *Service) orgOwnerRefs(ctx context.Context, org uuid.UUID) string {
	members, err := s.Store.ListOrgMembers(ctx, org)
	if err != nil {
		return ""
	}
	var refs []string
	for _, m := range members {
		if OrgRole(m.Role) == OrgOwner {
			refs = append(refs, m.Person.Ref())
		}
	}
	if len(refs) > 3 {
		refs = append(refs[:3], "...")
	}
	return strings.Join(refs, ", ")
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// repoOrg picks the organization a new repository goes to and checks that
// actor may create repositories there: owners and admins of the
// organization (not superadmins as such). With org empty it is the organization
// named like the repository's owner part when the caller may create there,
// else the caller's personal organization.
func (s *Service) repoOrg(ctx context.Context, actor Actor, org, repoName string) (store.Org, error) {
	h := actor.Human()
	if h.Kind != store.KindHuman {
		return store.Org{}, errf(CodePermission, "only people and their agents can create repositories; %s is a %s", actor.Ref(), actor.Kind)
	}
	if org = strings.TrimSpace(org); org != "" {
		a, err := s.manageable(ctx, nil, actor, org, "creating repositories")
		return a.org, err
	}
	if owner, _, ok := strings.Cut(repoName, "/"); ok {
		if a, err := s.loadOrg(ctx, nil, actor, owner); err == nil && a.canManage() {
			return a.org, nil
		}
	}
	o, err := s.Store.OrgByHandle(ctx, h.Handle)
	if err != nil {
		return store.Org{}, errf(CodeFailedPrecondition, "%s has no personal organization; pass --org ORG", h.Ref())
	}
	return o, nil
}
