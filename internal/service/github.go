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
}

// ErrGitHubNotConfigured is returned when the server has no GitHub App yet.
var ErrGitHubNotConfigured = errors.New("GitHub is not configured on this server: a site admin sets it up on the Admin page")

func githubNotImplemented() error {
	return errf(CodeUnavailable, "this part of GitHub sign-in is not implemented yet")
}

// GitHubSetupManifest returns the App manifest (JSON) and a state value for
// the manifest flow (site admins). The web page posts it to GitHub.
func (s *Service) GitHubSetupManifest(ctx context.Context, actor Actor) (manifest []byte, state string, err error) {
	return nil, "", githubNotImplemented()
}

// CompleteGitHubSetup exchanges the manifest code for the App's
// credentials and stores them encrypted (site admins).
func (s *Service) CompleteGitHubSetup(ctx context.Context, actor Actor, code, state string) error {
	return githubNotImplemented()
}

// GitHubSignInStart returns the GitHub authorize URL for a web sign-in and
// the state and PKCE verifier the web layer keeps in a short-lived cookie.
func (s *Service) GitHubSignInStart(ctx context.Context, next string) (authorizeURL, state, verifier string, err error) {
	return "", "", "", githubNotImplemented()
}

// GitHubSignInFinish completes a web sign-in: it links or creates the
// person (per the signup mode), syncs their organizations, and returns the
// person and a new web-session token. isNew is true on first sign-in.
func (s *Service) GitHubSignInFinish(ctx context.Context, code, verifier string) (p store.Principal, token string, isNew bool, err error) {
	return store.Principal{}, "", false, githubNotImplemented()
}

// InviteGitHubUser allows a GitHub login to sign up while sign-up is
// invite-only (site admins).
func (s *Service) InviteGitHubUser(ctx context.Context, actor Actor, login, note string) error {
	return githubNotImplemented()
}

// HandleGitHubWebhook verifies and applies a webhook delivery:
// installations, repositories, membership and permission changes.
func (s *Service) HandleGitHubWebhook(ctx context.Context, event, deliveryID, signature string, body []byte) error {
	return githubNotImplemented()
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

// CLILogin is a started browser login.
type CLILogin struct {
	DeviceCode      string
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time
	Interval        time.Duration
}

// CLILogin states.
const (
	CLILoginPending  = "pending"
	CLILoginApproved = "approved"
	CLILoginDenied   = "denied"
	CLILoginExpired  = "expired"
)

// StartCLILogin begins a browser login for a CLI (no token needed).
func (s *Service) StartCLILogin(ctx context.Context, clientName string) (CLILogin, error) {
	return CLILogin{}, githubNotImplemented()
}

// PollCLILogin returns the state of a login; on approval it returns the new
// token exactly once.
func (s *Service) PollCLILogin(ctx context.Context, deviceCode string) (status, token string, p store.Principal, err error) {
	return "", "", store.Principal{}, githubNotImplemented()
}

// PendingCLILogin returns the client name of a pending login by user code,
// for the approval page (signed-in people only).
func (s *Service) PendingCLILogin(ctx context.Context, actor Actor, userCode string) (clientName string, expires time.Time, err error) {
	return "", time.Time{}, githubNotImplemented()
}

// ApproveCLILogin approves or denies a pending login by user code; approval
// creates a token for the person named after the client.
func (s *Service) ApproveCLILogin(ctx context.Context, actor Actor, userCode string, approve bool) error {
	return githubNotImplemented()
}

// ServerInfo is the server's version and the oldest CLI it accepts.
type ServerInfo struct {
	Version       string
	MinCLIVersion string
	Features      []string
	GitHubSignIn  bool
}
