package service

// Roles on GitHub-backed repositories come from GitHub. The rules:
//
//   - A repository linked to a GitHub repository through an installation
//     of the vx GitHub App (repos.github_repo_id and
//     github_installation_id set) is GitHub-backed. A person's role on it
//     is their GitHub permission (admin, maintain: admin; write: write;
//     triage, read: read; none: no access). vx grants, teams, organization
//     defaults and organization roles count for nothing there.
//   - Only people linked to GitHub (principals.github_user_id) have a role.
//     Site admins keep admin everywhere. Agents have their person's role.
//     Service principals have only their direct vx grants (GitHub does
//     not know them; repository admins may grant them, nobody else).
//   - A person who does not belong to the installation's account (it is
//     not their own GitHub account and GitHub does not say they are a
//     member of the organization) only gets roles GitHub granted them
//     explicitly: GitHub's "read" is ignored for them, because public
//     repositories give read to every GitHub user. Outside collaborators
//     with write, maintain or admin keep it; those with only read do not.
//   - A suspended installation, or one vx has no record of (removed),
//     gives nobody a role (site admins aside).
//   - Answers are cached in github_role_cache and
//     github_org_membership_cache for GitHubRoleTTL; webhooks drop them
//     sooner (DropGitHubRoles). When GitHub cannot be reached, an answer
//     up to githubStaleOK old is used; otherwise access fails closed with
//     a retryable CodeUnavailable error. vx never falls back to vx grants.
//   - vx organizations linked to a GitHub organization follow it: GitHub
//     admins are vx admins (owners stay owners), members are members, and
//     people GitHub says are not members leave the vx organization. This is
//     applied on sign-in (SyncGitHubOrgs), on organization webhooks
//     (ApplyGitHubOrgMembership) and whenever a membership is read again
//     from GitHub.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// githubStaleOK is how old a cached answer may be when GitHub cannot be
// reached. Past it, access fails closed.
const githubStaleOK = time.Hour

// githubParallel bounds concurrent GitHub calls for one listing.
const githubParallel = 8

// githubSubject is the person whose GitHub role counts for an actor.
type githubSubject struct {
	person    store.Principal
	ident     store.GitHubLink
	linked    bool
	siteAdmin bool
	service   bool // service principals: direct vx grants only
}

func (s *Service) githubSubjectOf(ctx context.Context, p store.Principal) (githubSubject, error) {
	if p.Kind == store.KindAgent {
		if p.ParentID == nil {
			return githubSubject{person: p}, nil
		}
		parent, err := s.Store.PrincipalByID(ctx, *p.ParentID)
		if err != nil {
			return githubSubject{}, err
		}
		p = parent
	}
	sub := githubSubject{person: p, service: p.Kind == store.KindService}
	if p.Kind != store.KindHuman {
		return sub, nil
	}
	fresh, err := s.Store.PrincipalByID(ctx, p.ID) // site admin changes apply at once
	if err != nil {
		return githubSubject{}, err
	}
	sub.siteAdmin = fresh.SiteAdmin
	sub.ident, sub.linked, err = s.Store.GitHubLinkOf(ctx, p.ID)
	sub.linked = sub.linked && sub.ident.Login != ""
	return sub, err
}

// GitHubRole returns a person's role on a GitHub-backed repository, from
// the cache or GitHub. ok is false when the repository is not GitHub-backed
// (then vx's own roles apply). Authorize calls it. An agent is checked as
// its person; a service principal has no GitHub role. When GitHub cannot be
// reached and no answer younger than an hour is cached, err is a retryable
// CodeUnavailable error and role is RoleNone.
func (s *Service) GitHubRole(ctx context.Context, repo store.Repo, person store.Principal) (role RepoRole, ok bool, err error) {
	if !repo.GitHubBacked() {
		return RoleNone, false, nil
	}
	sub, err := s.githubSubjectOf(ctx, person)
	if err != nil {
		return RoleNone, true, err
	}
	roles, errs := s.githubRoles(ctx, sub, []store.Repo{repo}, false)
	if err := errs[repo.ID]; err != nil {
		return RoleNone, true, err
	}
	return roles[repo.ID], true, nil
}

// githubRoles returns sub's roles on GitHub-backed repos, with an error per
// repository GitHub could not answer for (those have RoleNone). With
// listing set, repositories of accounts sub does not belong to are only
// checked when a cached answer exists, so a listing asks GitHub about the
// repositories of the person's own account and organizations only.
func (s *Service) githubRoles(ctx context.Context, sub githubSubject, repos []store.Repo, listing bool) (map[uuid.UUID]RepoRole, map[uuid.UUID]error) {
	roles := make(map[uuid.UUID]RepoRole, len(repos))
	errs := map[uuid.UUID]error{}
	if len(repos) == 0 {
		return roles, errs
	}
	if sub.siteAdmin {
		for _, r := range repos {
			roles[r.ID] = RoleAdmin
		}
		return roles, errs
	}
	if sub.service || !sub.linked {
		return roles, errs
	}
	ids := make([]uuid.UUID, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	cached, err := s.Store.GitHubRoleCache(ctx, sub.person.ID, ids)
	if err != nil {
		for _, r := range repos {
			errs[r.ID] = err
		}
		return roles, errs
	}
	insts, err := s.githubInstallations(ctx)
	if err != nil {
		for _, r := range repos {
			errs[r.ID] = err
		}
		return roles, errs
	}
	now := s.now()
	var ask []store.Repo
	for _, r := range repos {
		if inst, found := insts[*r.GitHubInstallationID]; !found || inst.SuspendedAt != nil {
			continue // a removed or suspended installation gives nobody a role
		}
		if c, hit := cached[r.ID]; hit && now.Sub(c.CheckedAt) < GitHubRoleTTL {
			roles[r.ID] = RepoRole(c.Role)
			continue
		}
		ask = append(ask, r)
	}
	if len(ask) == 0 {
		return roles, errs
	}

	// Membership in the accounts of the installations involved, once each.
	members := s.githubMemberships(ctx, sub, ask, insts)
	var mu sync.Mutex
	sem := make(chan struct{}, githubParallel)
	var wg sync.WaitGroup
	for _, r := range ask {
		m := members[*r.GitHubInstallationID]
		c, hit := cached[r.ID]
		if listing && !hit && m.err == nil && !m.member {
			continue // not their account or organization: not listed
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(r store.Repo, m membership, c store.CachedRole, hit bool) {
			defer wg.Done()
			defer func() { <-sem }()
			role, err := s.askGitHubRole(ctx, sub, r, m)
			if err != nil && hit && now.Sub(c.CheckedAt) < githubStaleOK {
				role, err = RepoRole(c.Role), nil
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[r.ID] = err
				return
			}
			roles[r.ID] = role
		}(r, m, c, hit)
	}
	wg.Wait()
	return roles, errs
}

// askGitHubRole asks GitHub for sub's permission on r and caches the
// resulting role.
func (s *Service) askGitHubRole(ctx context.Context, sub githubSubject, r store.Repo, m membership) (RepoRole, error) {
	if m.err != nil {
		return RoleNone, m.err
	}
	api, err := s.GitHubClient(ctx)
	if err != nil {
		return RoleNone, githubUnreachable(r.Name, err)
	}
	owner, name := githubRepoName(r)
	perm, err := api.RepoPermission(ctx, *r.GitHubInstallationID, owner, name, sub.ident.Login)
	if err != nil {
		return RoleNone, githubUnreachable(r.Name, err)
	}
	if !m.member && perm == "read" {
		perm = "none" // public repositories give read to everyone; only explicit grants count
	}
	role := RoleFromGitHub(perm)
	if err := s.Store.PutGitHubRole(ctx, r.ID, sub.person.ID, string(role), s.now()); err != nil {
		log.Printf("github roles: caching %s on %s: %v", sub.person.Ref(), r.Name, err)
	}
	return role, nil
}

func githubUnreachable(repo string, err error) error {
	return &Error{Code: CodeUnavailable, Err: err, Msg: fmt.Sprintf(
		"could not check your role on %s with GitHub (%v), and vx does not allow access without it; try again in a minute", repo, err)}
}

// githubRepoName is owner and name of r on GitHub: its remote for GitHub
// repositories (kept up to date on renames and transfers), else its name.
func githubRepoName(r store.Repo) (owner, name string) {
	full := r.Name
	if r.Provider == store.ProviderGitHub && strings.Count(r.Remote, "/") == 1 {
		full = r.Remote
	}
	owner, name, _ = strings.Cut(full, "/")
	return owner, name
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// githubInstallations reads every installation by id. There are few.
func (s *Service) githubInstallations(ctx context.Context) (map[int64]store.GitHubInstallationRow, error) {
	rows, err := s.Store.ListGitHubInstallations(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]store.GitHubInstallationRow, len(rows))
	for _, i := range rows {
		out[i.ID] = i
	}
	return out, nil
}

// membership is whether a person belongs to an installation's account.
type membership struct {
	member bool
	role   string // admin, member or "" for organizations
	err    error  // GitHub could not be asked and nothing recent is cached
}

// githubMemberships works out, per installation of repos, whether sub
// belongs to its account: their own user account, or an organization
// GitHub says they are a member of (cached like roles). An installation vx
// has no record of counts as an account they do not belong to.
func (s *Service) githubMemberships(ctx context.Context, sub githubSubject, repos []store.Repo, insts map[int64]store.GitHubInstallationRow) map[int64]membership {
	out := map[int64]membership{}
	want := map[int64]store.GitHubInstallationRow{}
	for _, r := range repos {
		id := *r.GitHubInstallationID
		if _, done := out[id]; done {
			continue
		}
		inst, ok := insts[id]
		switch {
		case !ok:
			out[id] = membership{}
		case inst.AccountType != "Organization":
			out[id] = membership{member: inst.AccountID == sub.ident.UserID}
		default:
			out[id] = membership{}
			want[id] = inst
		}
	}
	if len(want) == 0 {
		return out
	}
	cached, err := s.Store.GitHubMembershipCache(ctx, sub.person.ID)
	if err != nil {
		for id := range want {
			out[id] = membership{err: err}
		}
		return out
	}
	for id, inst := range want {
		out[id] = s.orgMembership(ctx, sub, inst, cached)
	}
	return out
}

// orgMembership is sub's membership in the organization of inst, from the
// cache or GitHub; a fresh answer also updates the linked vx organization.
func (s *Service) orgMembership(ctx context.Context, sub githubSubject, inst store.GitHubInstallationRow, cached map[int64]store.CachedRole) membership {
	now := s.now()
	c, hit := cached[inst.AccountID]
	if hit && now.Sub(c.CheckedAt) < GitHubRoleTTL {
		return membership{member: c.Role != "", role: c.Role}
	}
	var role string
	api, err := s.GitHubClient(ctx)
	if err == nil {
		role, err = api.OrgMembership(ctx, inst.ID, inst.AccountLogin, sub.ident.Login)
	}
	if err != nil {
		if hit && now.Sub(c.CheckedAt) < githubStaleOK {
			return membership{member: c.Role != "", role: c.Role}
		}
		return membership{err: &Error{Code: CodeUnavailable, Err: err, Msg: fmt.Sprintf(
			"could not check your membership in GitHub organization %s (%v), and vx does not allow access without it; try again in a minute", inst.AccountLogin, err)}}
	}
	if role != "admin" && role != "member" {
		role = ""
	}
	if err := s.Store.PutGitHubMembership(ctx, inst.AccountID, sub.person.ID, role, now); err != nil {
		log.Printf("github roles: caching membership of %s in %s: %v", sub.person.Ref(), inst.AccountLogin, err)
	}
	if !hit || c.Role != role {
		if err := s.applyOrgMembership(ctx, inst.AccountID, sub.person, role); err != nil {
			log.Printf("github roles: membership of %s in %s: %v", sub.person.Ref(), inst.AccountLogin, err)
		}
	}
	return membership{member: role != "", role: role}
}

// githubReadable returns the GitHub-backed repositories sub can read, with
// sub's role on each; org limits them to one vx organization. Repositories
// GitHub could not answer for are left out (fail closed).
func (s *Service) githubReadable(ctx context.Context, sub githubSubject, org *uuid.UUID) ([]store.Repo, map[uuid.UUID]RepoRole, error) {
	if sub.service || (!sub.linked && !sub.siteAdmin) {
		return nil, nil, nil
	}
	insts, err := s.githubInstallations(ctx)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(insts))
	for id := range insts {
		ids = append(ids, id)
	}
	repos, err := s.Store.GitHubBackedRepos(ctx, ids, org)
	if err != nil {
		return nil, nil, err
	}
	roles, errs := s.githubRoles(ctx, sub, repos, true)
	var out []store.Repo
	for _, r := range repos {
		if err := errs[r.ID]; err != nil {
			log.Printf("github roles: leaving %s out of %s's list: %v", r.Name, sub.person.Ref(), err)
			continue
		}
		if roles[r.ID] != RoleNone {
			out = append(out, r)
		}
	}
	return out, roles, nil
}

// GitHubRoleScope selects what DropGitHubRoles forgets. Every set field
// adds its own part (the fields are OR-ed, never AND-ed):
//
//   - RepoID or GitHubRepoID: everyone's role on that repository (member
//     added, removed or edited; repository visibility, rename, transfer).
//   - GitHubOrgID: roles on every repository of installations on that
//     account and memberships in it (organization membership and team
//     changes).
//   - GitHubUserID: everything cached for that person (person removed,
//     or their access changed in a way that names only them).
//   - InstallationID: roles on every repository of the installation and
//     memberships in its account (installation removed, suspended, or its
//     repositories changed).
//
// Reason is logged.
type GitHubRoleScope struct {
	RepoID         *uuid.UUID
	GitHubRepoID   int64
	GitHubOrgID    int64
	GitHubUserID   int64
	InstallationID int64
	Reason         string
}

// DropGitHubRoles forgets cached GitHub answers in scope, so the next
// check asks GitHub. Webhooks call it; it never calls GitHub. An empty
// scope is an error, so a caller bug cannot silently keep stale roles.
func (s *Service) DropGitHubRoles(ctx context.Context, scope GitHubRoleScope) error {
	if scope.RepoID == nil && scope.GitHubRepoID == 0 && scope.GitHubOrgID == 0 && scope.GitHubUserID == 0 && scope.InstallationID == 0 {
		return errf(CodeInvalid, "DropGitHubRoles needs a repository, organization, user or installation")
	}
	n, err := s.Store.DropGitHubCache(ctx, store.GitHubCacheScope{
		RepoID: scope.RepoID, GitHubRepoID: scope.GitHubRepoID, GitHubOrgID: scope.GitHubOrgID,
		GitHubUserID: scope.GitHubUserID, InstallationID: scope.InstallationID,
	})
	if err != nil {
		return err
	}
	if scope.Reason != "" {
		log.Printf("github roles: dropped %d cached answers: %s", n, scope.Reason)
	}
	return nil
}

// ApplyGitHubOrgMembership records a membership change GitHub reported in
// a webhook (organization member_added, member_removed, member_invited
// does not count): role is admin, member or "" (not a member). It drops the
// person's cached answers for that organization and updates the linked vx
// organization. People vx does not know are ignored.
func (s *Service) ApplyGitHubOrgMembership(ctx context.Context, githubOrgID, githubUserID int64, role string) error {
	if err := s.DropGitHubRoles(ctx, GitHubRoleScope{GitHubOrgID: githubOrgID, Reason: "organization membership changed"}); err != nil {
		return err
	}
	p, err := s.Store.PrincipalByGitHubID(ctx, githubUserID)
	if err != nil {
		if isHidden(classify(err)) {
			return nil
		}
		return err
	}
	if role != "admin" && role != "member" {
		role = ""
	}
	if err := s.Store.PutGitHubMembership(ctx, githubOrgID, p.ID, role, s.now()); err != nil {
		return err
	}
	return s.applyOrgMembership(ctx, githubOrgID, p, role)
}

// applyOrgMembership makes person's membership in the vx organization
// linked to a GitHub organization follow GitHub: admin is admin (an owner
// stays owner), member is member, "" removes them (and from its teams).
// Without a linked vx organization it does nothing.
func (s *Service) applyOrgMembership(ctx context.Context, githubOrgID int64, person store.Principal, role string) error {
	return s.Store.InTx(ctx, func(tx *store.Tx) error {
		o, err := tx.OrgByGitHubID(ctx, githubOrgID)
		if isHidden(classify(err)) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.LockOrg(ctx, o.Handle); err != nil {
			return err
		}
		prev, err := tx.OrgRole(ctx, o.ID, person.ID)
		if err != nil {
			return err
		}
		want := role
		if role == "admin" && OrgRole(prev) == OrgOwner {
			want = string(OrgOwner)
		}
		if want == prev {
			return nil
		}
		var before, after any
		if prev != "" {
			before = memberChange{Person: person.Ref(), Role: prev}
		}
		if want == "" {
			if _, err := tx.RemoveOrgMember(ctx, o.ID, person.ID); err != nil {
				return err
			}
		} else {
			if err := tx.SetOrgMember(ctx, o.ID, person.ID, want); err != nil {
				return err
			}
			after = memberChange{Person: person.Ref(), Role: want}
		}
		return tx.LogOp(ctx, nil, nil, "org.member", o.Handle, before, after)
	})
}

// SyncGitHubOrgs refreshes a person's memberships in every GitHub
// organization with an installation of the App, and so in the linked vx
// organizations, and drops their cached roles. Web sign-in calls it. It
// asks GitHub once per organization installation.
func (s *Service) SyncGitHubOrgs(ctx context.Context, person store.Principal) error {
	sub, err := s.githubSubjectOf(ctx, person)
	if err != nil || !sub.linked {
		return err
	}
	if err := s.DropGitHubRoles(ctx, GitHubRoleScope{GitHubUserID: sub.ident.UserID}); err != nil {
		return err
	}
	insts, err := s.githubInstallations(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, inst := range insts {
		if inst.AccountType != "Organization" || inst.SuspendedAt != nil {
			continue
		}
		if m := s.orgMembership(ctx, sub, inst, nil); m.err != nil && firstErr == nil {
			firstErr = m.err
		}
	}
	return firstErr
}

// MyGitHubInstallations lists the installations of the App on the actor's
// own GitHub account and on GitHub organizations GitHub says they are a
// member of. It is empty for people without a GitHub link.
func (s *Service) MyGitHubInstallations(ctx context.Context, actor Actor) ([]GitHubInstallation, error) {
	sub, err := s.githubSubjectOf(ctx, actor.Principal)
	if err != nil || !sub.linked {
		return nil, err
	}
	rows, err := s.Store.ListGitHubInstallations(ctx)
	if err != nil {
		return nil, err
	}
	cached, err := s.Store.GitHubMembershipCache(ctx, sub.person.ID)
	if err != nil {
		return nil, err
	}
	var out []GitHubInstallation
	for _, i := range rows {
		mine := i.AccountType != "Organization" && i.AccountID == sub.ident.UserID
		if i.AccountType == "Organization" {
			mine = s.orgMembership(ctx, sub, i, cached).member
		}
		if mine {
			out = append(out, GitHubInstallation{ID: i.ID, AccountID: i.AccountID, AccountLogin: i.AccountLogin,
				AccountType: i.AccountType, RepositorySelection: i.RepositorySelection, SuspendedAt: i.SuspendedAt})
		}
	}
	return out, nil
}

// GitHubSettingsURL is where access to a GitHub-backed repository is
// managed on GitHub.
func (s *Service) GitHubSettingsURL(r store.Repo) string {
	owner, name := githubRepoName(r)
	return s.GitHubWebURL() + "/" + owner + "/" + name + "/settings/access"
}

// refuseOnGitHubRepo is the error for managing vx roles on a GitHub-backed
// repository.
func (s *Service) refuseOnGitHubRepo(r store.Repo, what string) error {
	return errf(CodeFailedPrecondition, "roles on %s come from GitHub, so vx does not %s there; change who has access in GitHub: %s",
		r.Name, what, s.GitHubSettingsURL(r))
}

// refuseOnGitHubOrg is the error for managing members or teams of a vx
// organization that follows a GitHub organization.
func (s *Service) refuseOnGitHubOrg(o store.Org, what string) error {
	return errf(CodeFailedPrecondition, "%s follows GitHub organization %s, so vx does not %s there; change it in GitHub: %s/orgs/%s/people",
		o.Handle, o.GitHubLogin, what, s.GitHubWebURL(), o.GitHubLogin)
}

// githubRepoAccess is RepoAccess for a GitHub-backed repository: the
// direct grants (only those of service principals count), and the roles
// GitHub reported for people, as cached, with site admins.
func (s *Service) githubRepoAccess(ctx context.Context, r store.Repo, rows []store.RepoGrant) ([]RepoGrant, []AccessEntry, error) {
	var grants []RepoGrant
	for _, g := range rows {
		sub := grantSubject{principal: g.Principal, team: g.Team}
		grants = append(grants, RepoGrant{Repo: r.Name, Subject: sub.ref(), Role: RepoRole(g.Role)})
	}
	facts, err := s.Store.GitHubAccessFacts(ctx, r.ID, s.now().Add(-GitHubRoleTTL))
	if err != nil {
		return nil, nil, err
	}
	var entries []AccessEntry
	byID := map[uuid.UUID]int{}
	for _, f := range facts {
		i, seen := byID[f.Principal.ID]
		if !seen {
			byID[f.Principal.ID] = len(entries)
			entries = append(entries, AccessEntry{Person: f.Principal})
			i = len(entries) - 1
		}
		if role := RepoRole(f.Role); !entries[i].Role.Includes(role) {
			entries[i].Role = role
		}
		entries[i].Via = append(entries[i].Via, f.Via)
	}
	return grants, entries, nil
}
