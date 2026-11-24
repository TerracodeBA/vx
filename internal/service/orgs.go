package service

// Organizations, teams and repository roles (v0.2). This file holds the
// types; orgs_access.go has Authorize and its helpers, orgs_manage.go the
// organization, team, grant and people functions. The signatures are the
// contract the enforcement, object access and admin pages build on.

import (
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
