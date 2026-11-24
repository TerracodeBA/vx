package service

// Organizations, teams and repository roles (v0.2). This file is the
// contract the org storage, enforcement, object access and admin pages build
// on. Authorize and its helpers allow everything until the org storage
// lands; the org management functions return "not implemented yet". The
// signatures are fixed.

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// RepoRole is what a principal may do in a repository. Roles are ordered:
// admin includes write, write includes read.
type RepoRole string

const (
	RoleNone  RepoRole = ""
	RoleRead  RepoRole = "read"
	RoleWrite RepoRole = "write"
	RoleAdmin RepoRole = "admin"
)

// Includes reports whether r is at least need.
func (r RepoRole) Includes(need RepoRole) bool { return roleRank[r] >= roleRank[need] }

var roleRank = map[RepoRole]int{RoleNone: 0, RoleRead: 1, RoleWrite: 2, RoleAdmin: 3}

// OrgRole is a person's role in an organization.
type OrgRole string

const (
	OrgOwner  OrgRole = "owner"
	OrgAdmin  OrgRole = "admin"
	OrgMember OrgRole = "member"
)

// Org is an organization.
type Org struct {
	ID              uuid.UUID
	Handle          string
	DisplayName     string
	DefaultRepoRole RepoRole // RoleNone, RoleRead or RoleWrite
	Personal        bool
	CreatedAt       time.Time
}

// Membership is a person in an organization.
type Membership struct {
	Person store.Principal
	Role   OrgRole
	Teams  []string
}

// Team is a named group of an organization's members.
type Team struct {
	ID      uuid.UUID
	OrgID   uuid.UUID
	Org     string
	Name    string
	Members []store.Principal
}

// RepoGrant is a direct grant of a role on a repository.
type RepoGrant struct {
	Repo    string
	Subject string // "user:alice" or "team:acme/backend"
	Role    RepoRole
}

// AccessEntry is one person's effective role on a repository and why.
type AccessEntry struct {
	Person store.Principal
	Role   RepoRole
	Via    []string
}

// OrgDetail is an organization with its members, teams and the
// repositories the caller can read.
type OrgDetail struct {
	Org     Org
	MyRole  OrgRole // empty when the caller is not a member
	Members []Membership
	Teams   []Team
	Repos   []store.Repo
}

// Authorize returns the repository named repo if actor has at least need on
// it. A repository the actor cannot read is reported as not found, so its
// existence does not leak; one it can read but needs more on is a
// permission error. Agents are checked as the person they act for.
//
// Every service function that takes a repository name calls this first.
func (s *Service) Authorize(ctx context.Context, actor Actor, repo string, need RepoRole) (store.Repo, RepoRole, error) {
	r, err := s.Store.RepoByName(ctx, repo)
	if err != nil {
		return store.Repo{}, RoleNone, classify(err)
	}
	return r, RoleAdmin, nil // until org storage lands: everyone is admin everywhere
}

// AuthorizeID is Authorize for a repository id (for objects, workspaces and
// other records that carry a repo id).
func (s *Service) AuthorizeID(ctx context.Context, actor Actor, repoID uuid.UUID, need RepoRole) (RepoRole, error) {
	return RoleAdmin, nil
}

// ReadableRepoIDs returns the ids of every repository actor can read, for
// filtering lists. all is true when the actor can read every repository (a
// site admin), in which case ids is nil.
func (s *Service) ReadableRepoIDs(ctx context.Context, actor Actor) (ids []uuid.UUID, all bool, err error) {
	return nil, true, nil
}

// IsSiteAdmin reports whether actor (or, for agents, its person) is a site
// admin.
func (s *Service) IsSiteAdmin(ctx context.Context, actor Actor) bool {
	return true
}

func orgsNotImplemented() error {
	return errf(CodeUnavailable, "organizations are not implemented yet")
}

// CreateOrg creates an organization owned by the calling person.
func (s *Service) CreateOrg(ctx context.Context, actor Actor, handle, displayName string) (Org, error) {
	return Org{}, orgsNotImplemented()
}

// ListOrgs lists the caller's organizations, or all of them for site admins
// with all set.
func (s *Service) ListOrgs(ctx context.Context, actor Actor, all bool) ([]Org, []OrgRole, error) {
	return nil, nil, orgsNotImplemented()
}

// GetOrg returns an organization the caller is a member of (or any, for
// site admins).
func (s *Service) GetOrg(ctx context.Context, actor Actor, handle string) (OrgDetail, error) {
	return OrgDetail{}, orgsNotImplemented()
}

// UpdateOrg changes an organization's display name or default repository
// role (owners and admins). Empty values are unchanged.
func (s *Service) UpdateOrg(ctx context.Context, actor Actor, handle, displayName string, defaultRole *RepoRole) (Org, error) {
	return Org{}, orgsNotImplemented()
}

// SetMember adds a person to an organization or changes their role.
// Owners and admins may; only owners make owners; the last owner stays.
func (s *Service) SetMember(ctx context.Context, actor Actor, org, person string, role OrgRole) (Membership, error) {
	return Membership{}, orgsNotImplemented()
}

// RemoveMember removes a person from an organization and its teams.
func (s *Service) RemoveMember(ctx context.Context, actor Actor, org, person string) error {
	return orgsNotImplemented()
}

// CreateTeam, DeleteTeam, SetTeamMember and RemoveTeamMember manage teams
// (org owners and admins).
func (s *Service) CreateTeam(ctx context.Context, actor Actor, org, name string) (Team, error) {
	return Team{}, orgsNotImplemented()
}

func (s *Service) DeleteTeam(ctx context.Context, actor Actor, org, name string) error {
	return orgsNotImplemented()
}

func (s *Service) SetTeamMember(ctx context.Context, actor Actor, org, team, person string) (Team, error) {
	return Team{}, orgsNotImplemented()
}

func (s *Service) RemoveTeamMember(ctx context.Context, actor Actor, org, team, person string) (Team, error) {
	return Team{}, orgsNotImplemented()
}

// GrantRepo gives a person or team a role on a repository (repo admins).
func (s *Service) GrantRepo(ctx context.Context, actor Actor, repo, subject string, role RepoRole) (RepoGrant, error) {
	return RepoGrant{}, orgsNotImplemented()
}

// RevokeRepo removes a direct grant (repo admins).
func (s *Service) RevokeRepo(ctx context.Context, actor Actor, repo, subject string) error {
	return orgsNotImplemented()
}

// RepoAccess lists a repository's direct grants and everyone's effective
// role (repo admins).
func (s *Service) RepoAccess(ctx context.Context, actor Actor, repo string) ([]RepoGrant, []AccessEntry, error) {
	return nil, nil, orgsNotImplemented()
}

// CreatePerson creates a person with a first token (site admins), optionally
// as a member of an organization.
func (s *Service) CreatePerson(ctx context.Context, actor Actor, handle, displayName, email string, siteAdmin bool, org string) (store.Principal, string, error) {
	return store.Principal{}, "", orgsNotImplemented()
}

// ListPeople lists every person on the server (site admins).
func (s *Service) ListPeople(ctx context.Context, actor Actor) ([]store.Principal, error) {
	return nil, orgsNotImplemented()
}
