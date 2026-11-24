package service

// GitHub as identity and permission source. This file is the contract the
// GitHub App, permissions, web sign-in and CLI login build on; the functions
// return errors until their parts land. Signatures are fixed.
//
// Rules:
//   - A repository linked to a GitHub App installation (repos.github_repo_id
//     set) is GitHub-backed: a person's role on it is their GitHub
//     permission (admin, maintain: admin; write: write; triage, read: read;
//     none: no access), never vx grants. Site admins keep admin. Agents have
//     their person's role. Organization owners get no extra rights on
//     GitHub-backed repositories: GitHub decides.
//   - People are linked to GitHub by numeric user id (logins can change).
//   - Secrets (App private key, client secret, webhook secret, CLI tokens in
//     flight) are stored encrypted with the server key.

import (
	"context"
	"errors"
	"time"

	"github.com/TerracodeBA/vx/internal/store"
)

// GitHubAPI is what vx needs from GitHub. internal/github implements it over
// HTTP; tests use the fake in internal/github/githubtest.
type GitHubAPI interface {
	// CreateAppFromManifest completes the manifest flow and returns the new
	// App's credentials.
	CreateAppFromManifest(ctx context.Context, code string) (GitHubAppCredentials, error)
	// Installation and InstallationRepos read an installation of the App.
	Installation(ctx context.Context, id int64) (GitHubInstallation, error)
	InstallationRepos(ctx context.Context, id int64) ([]GitHubRepo, error)
	// InstallationToken returns a short-lived token for an installation, for
	// git pushes and API calls on its repositories.
	InstallationToken(ctx context.Context, id int64) (token string, expires time.Time, err error)
	// ExchangeOAuthCode turns a web sign-in code into a user access token.
	ExchangeOAuthCode(ctx context.Context, code, redirectURI, codeVerifier string) (userToken string, err error)
	// User returns the signed-in GitHub user.
	User(ctx context.Context, userToken string) (GitHubUser, error)
	// RepoPermission returns login's permission on owner/repo: admin,
	// maintain, write, triage, read or none.
	RepoPermission(ctx context.Context, installationID int64, owner, repo, login string) (string, error)
	// OrgMembership returns login's role in org: admin, member or none.
	OrgMembership(ctx context.Context, installationID int64, org, login string) (string, error)
}

// GitHubAppCredentials are what the manifest flow returns.
type GitHubAppCredentials struct {
	AppID         int64
	Slug          string
	HTMLURL       string
	ClientID      string
	ClientSecret  string
	PrivateKeyPEM string
	WebhookSecret string
}

// GitHubInstallation is an installation of the App.
type GitHubInstallation struct {
	ID                  int64
	AccountID           int64
	AccountLogin        string
	AccountType         string // Organization or User
	RepositorySelection string // all or selected
	SuspendedAt         *time.Time
}

// GitHubRepo is a repository an installation can reach.
type GitHubRepo struct {
	ID            int64
	FullName      string // owner/name
	DefaultBranch string
	Private       bool
}

// GitHubUser is a signed-in GitHub user.
type GitHubUser struct {
	ID    int64
	Login string
	Name  string
	Email string // primary verified email if GitHub shares it; may be empty
}

// RoleFromGitHub maps a GitHub repository permission to a vx role.
func RoleFromGitHub(perm string) RepoRole {
	switch perm {
	case "admin", "maintain":
		return RoleAdmin
	case "write":
		return RoleWrite
	case "triage", "read":
		return RoleRead
	}
	return RoleNone
}

// GitHubRoleTTL is how long a role read from GitHub is trusted without
// asking again; webhooks drop cached roles sooner when access changes.
const GitHubRoleTTL = 5 * time.Minute

// SignupMode says who may create an account by signing in with GitHub.
type SignupMode string

const (
	SignupInvite SignupMode = "invite" // only invited GitHub logins (the default)
	SignupOpen   SignupMode = "open"   // anyone with a GitHub account
	SignupClosed SignupMode = "closed" // nobody new; existing people still sign in
)

// GitHubSettings configure the GitHub integration of a Service.
type GitHubSettings struct {
	API       GitHubAPI  // nil until the App is configured
	PublicURL string     // e.g. https://app.vxbench.com, for redirect and webhook URLs
	Signup    SignupMode // empty: SignupInvite
	SecretKey []byte     // 32 bytes from VX_SECRET_KEY, for secrets at rest

	// WebURL and APIURL are GitHub's browser and REST API bases; empty:
	// github.com. GitHub Enterprise Server: https://HOST and
	// https://HOST/api/v3.
	WebURL, APIURL string
	// Connect makes the API client for the App once its credentials are
	// known (internal/github in the server, the fake in tests). With zero
	// credentials it returns a client that can only complete the manifest
	// flow. nil: the App cannot be set up or loaded.
	Connect func(creds GitHubAppCredentials) (GitHubAPI, error)
}

// ErrGitHubNotConfigured is returned when the server has no GitHub App yet.
var ErrGitHubNotConfigured = errors.New("GitHub is not configured on this server: a site admin sets it up on the Admin page")

func githubNotImplemented() error {
	return errf(CodeUnavailable, "this part of GitHub sign-in is not implemented yet")
}

// SyncInstallation reads an installation's account and repositories from
// GitHub and creates or updates the matching vx organization and repos.
func (s *Service) SyncInstallation(ctx context.Context, installationID int64) error {
	return githubNotImplemented()
}

// GitHubRole returns a person's role on a GitHub-backed repository, from
// the cache or GitHub. ok is false when the repository is not GitHub-backed
// (then vx's own roles apply). Authorize calls it.
func (s *Service) GitHubRole(ctx context.Context, repo store.Repo, person store.Principal) (role RepoRole, ok bool, err error) {
	return RoleNone, false, nil // until the permissions part lands: not GitHub-backed
}
