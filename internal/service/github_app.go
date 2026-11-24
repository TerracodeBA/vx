package service

// The vx GitHub App of this server: creating it through the manifest flow,
// storing its credentials sealed with the server key, and loading it. Use
// s.GitHubClient(ctx) to reach GitHub, not s.GitHub.API: it loads the App
// from the database on first use, so every server instance picks up an App
// set up on another one, and swapping it in needs no restart.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/secret"
	"github.com/TerracodeBA/vx/internal/store"
)

// Paths on this server the App's manifest points GitHub at.
const (
	GitHubCallbackPath      = "/auth/github/callback"     // OAuth sign-in (vx-signin)
	GitHubSetupURLPath      = "/ui/github/installed"      // after an installation
	GitHubWebhookPath       = "/hooks/github"             // webhook deliveries
	GitHubManifestReturnURL = "/ui/admin/github/created"  // the manifest flow returns here
	githubDefaultWebURL     = "https://github.com"        // github.com browser base
	githubManifestStateTTL  = time.Hour                   // GitHub's manifest codes live an hour too
	githubNotConfiguredTTL  = 10 * time.Second            // how long "no App" is trusted before asking the database again
	githubAppNameMax        = 34                          // GitHub's limit on App names
	githubPurposeClient     = "github_app.client_secret"  // associated data of sealed secrets
	githubPurposeKey        = "github_app.private_key"    //
	githubPurposeWebhook    = "github_app.webhook_secret" //
)

// GitHubAppPermissions and GitHubAppEvents are what the App asks for.
// installation and installation_repositories are always delivered to an
// App, so they are not listed (GitHub refuses them in a manifest).
var (
	GitHubAppPermissions = map[string]string{
		"metadata":      "read",
		"contents":      "write",
		"pull_requests": "write",
		"checks":        "read",
		"members":       "read", // organization members, for GitHub-backed roles
	}
	GitHubAppEvents = []string{"repository", "member", "membership", "organization", "team", "team_add", "push", "pull_request"}
)

// githubState is the App loaded from the database.
type githubState struct {
	mu        sync.Mutex
	api       GitHubAPI
	app       *GitHubAppInfo
	webhook   string    // webhook secret
	missingAt time.Time // when the database last had no App
}

// GitHubAppInfo is what may be shown about the App (no secrets).
type GitHubAppInfo struct {
	AppID        int64
	Slug         string
	HTMLURL      string // https://github.com/apps/<slug>
	ClientID     string
	InstallURL   string // where an organization installs the App
	AuthorizeURL string // OAuth authorize endpoint for web sign-in
	CreatedBy    *uuid.UUID
	CreatedAt    time.Time
}

// GitHubWebURL is GitHub's browser base URL (github.com or GHE).
func (s *Service) GitHubWebURL() string {
	if s.GitHub.WebURL != "" {
		return strings.TrimRight(s.GitHub.WebURL, "/")
	}
	return githubDefaultWebURL
}

func (s *Service) publicURL() string { return strings.TrimRight(s.GitHub.PublicURL, "/") }

// GitHubClient returns the API client for the server's App:
// ErrGitHubNotConfigured when there is none. s.GitHub.API, when set,
// wins (tests).
func (s *Service) GitHubClient(ctx context.Context) (GitHubAPI, error) {
	if s.GitHub.API != nil {
		return s.GitHub.API, nil
	}
	if err := s.loadGitHubApp(ctx); err != nil {
		return nil, err
	}
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	return s.gh.api, nil
}

// GitHubApp returns the server's App: ErrGitHubNotConfigured when there
// is none.
func (s *Service) GitHubApp(ctx context.Context) (GitHubAppInfo, error) {
	if err := s.loadGitHubApp(ctx); err != nil {
		return GitHubAppInfo{}, err
	}
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	return *s.gh.app, nil
}

// GitHubAppInstallURL is where an organization installs the App; empty
// without an App.
func (s *Service) GitHubAppInstallURL(ctx context.Context) (string, error) {
	app, err := s.GitHubApp(ctx)
	if errors.Is(err, ErrGitHubNotConfigured) {
		return "", nil
	}
	return app.InstallURL, err
}

// LoadGitHubApp loads the App at server start. No App is not an error;
// an App the server cannot open (no or another VX_SECRET_KEY) is.
func (s *Service) LoadGitHubApp(ctx context.Context) error {
	err := s.loadGitHubApp(ctx)
	if errors.Is(err, ErrGitHubNotConfigured) {
		return nil
	}
	return err
}

// githubWebhookSecret returns the App's webhook secret.
func (s *Service) githubWebhookSecret(ctx context.Context) (string, error) {
	if err := s.loadGitHubApp(ctx); err != nil {
		return "", err
	}
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	return s.gh.webhook, nil
}

func (s *Service) loadGitHubApp(ctx context.Context) error {
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	if s.gh.app != nil {
		return nil
	}
	if !s.gh.missingAt.IsZero() && time.Since(s.gh.missingAt) < githubNotConfiguredTTL {
		return ErrGitHubNotConfigured
	}
	row, err := s.Store.GitHubApp(ctx)
	if errors.Is(err, store.ErrNotFound) {
		s.gh.missingAt = time.Now()
		return ErrGitHubNotConfigured
	}
	if err != nil {
		return err
	}
	creds, err := s.openGitHubApp(row)
	if err != nil {
		return err
	}
	return s.useGitHubApp(row, creds)
}

// useGitHubApp makes the client for creds; the caller holds s.gh.mu.
func (s *Service) useGitHubApp(row store.GitHubApp, creds GitHubAppCredentials) error {
	api := s.GitHub.API
	if api == nil {
		if s.GitHub.Connect == nil {
			return errf(CodeUnavailable, "this server cannot talk to GitHub (no client configured)")
		}
		var err error
		api, err = s.GitHub.Connect(creds)
		if err != nil {
			return fmt.Errorf("GitHub App %d: %w", row.AppID, err)
		}
	}
	info := GitHubAppInfo{
		AppID: row.AppID, Slug: row.Slug, HTMLURL: row.HTMLURL, ClientID: row.ClientID,
		InstallURL:   strings.TrimRight(row.HTMLURL, "/") + "/installations/new",
		AuthorizeURL: s.GitHubWebURL() + "/login/oauth/authorize",
		CreatedBy:    row.CreatedBy, CreatedAt: row.CreatedAt,
	}
	s.gh.api, s.gh.app, s.gh.webhook, s.gh.missingAt = api, &info, creds.WebhookSecret, time.Time{}
	return nil
}

func (s *Service) secretKey() ([]byte, error) {
	if len(s.GitHub.SecretKey) == 0 {
		return nil, errf(CodeFailedPrecondition, "%s", secret.ErrNoKey.Error())
	}
	if len(s.GitHub.SecretKey) != secret.KeySize {
		return nil, errf(CodeFailedPrecondition, "VX_SECRET_KEY must be %d bytes", secret.KeySize)
	}
	return s.GitHub.SecretKey, nil
}

func (s *Service) openGitHubApp(row store.GitHubApp) (GitHubAppCredentials, error) {
	key, err := s.secretKey()
	if err != nil {
		return GitHubAppCredentials{}, errf(CodeFailedPrecondition, "this server has a GitHub App, so it needs VX_SECRET_KEY, the key the App was stored with (%v)", err)
	}
	creds := GitHubAppCredentials{AppID: row.AppID, Slug: row.Slug, HTMLURL: row.HTMLURL, ClientID: row.ClientID}
	for _, f := range []struct {
		purpose string
		sealed  []byte
		into    *string
	}{
		{githubPurposeClient, row.ClientSecretEnc, &creds.ClientSecret},
		{githubPurposeKey, row.PrivateKeyEnc, &creds.PrivateKeyPEM},
		{githubPurposeWebhook, row.WebhookSecretEnc, &creds.WebhookSecret},
	} {
		v, err := secret.OpenString(key, f.purpose, f.sealed)
		if err != nil {
			return GitHubAppCredentials{}, errf(CodeFailedPrecondition, "the GitHub App's credentials do not open with VX_SECRET_KEY: set the key the App was created with, or create the App again on Admin > GitHub")
		}
		*f.into = v
	}
	return creds, nil
}

// SaveGitHubApp stores the App's credentials sealed with the server key
// and starts using them. CompleteGitHubSetup calls it; tests too.
func (s *Service) SaveGitHubApp(ctx context.Context, creds GitHubAppCredentials, createdBy *uuid.UUID) error {
	key, err := s.secretKey()
	if err != nil {
		return err
	}
	row := store.GitHubApp{AppID: creds.AppID, Slug: creds.Slug, HTMLURL: creds.HTMLURL, ClientID: creds.ClientID, CreatedBy: createdBy}
	for _, f := range []struct {
		purpose, value string
		into           *[]byte
	}{
		{githubPurposeClient, creds.ClientSecret, &row.ClientSecretEnc},
		{githubPurposeKey, creds.PrivateKeyPEM, &row.PrivateKeyEnc},
		{githubPurposeWebhook, creds.WebhookSecret, &row.WebhookSecretEnc},
	} {
		if *f.into, err = secret.SealString(key, f.purpose, f.value); err != nil {
			return err
		}
	}
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	if err := s.Store.PutGitHubApp(ctx, row); err != nil {
		return err
	}
	row, err = s.Store.GitHubApp(ctx)
	if err != nil {
		return err
	}
	s.gh.app = nil
	return s.useGitHubApp(row, creds)
}

// GitHubManifestFormURL is where the manifest form posts: the person's
// account, or org when set. state goes along and comes back.
func (s *Service) GitHubManifestFormURL(org, state string) string {
	u := s.GitHubWebURL() + "/settings/apps/new"
	if org = strings.TrimSpace(org); org != "" {
		u = s.GitHubWebURL() + "/organizations/" + url.PathEscape(org) + "/settings/apps/new"
	}
	return u + "?" + url.Values{"state": {state}}.Encode()
}

func (s *Service) needSiteAdmin(ctx context.Context, actor Actor, what string) error {
	if actor.Kind != store.KindHuman || !s.IsSiteAdmin(ctx, actor) {
		return errf(CodePermission, "only site admins can %s; ask one of them", what)
	}
	return nil
}

// githubSetupReady checks what the manifest flow needs.
func (s *Service) githubSetupReady() error {
	pub := s.publicURL()
	if pub == "" {
		return errf(CodeFailedPrecondition, "VX_PUBLIC_URL is not set: GitHub needs this server's public address (for example https://vx.example.com) for its callbacks and webhooks; set it and restart vx-server")
	}
	if u, err := url.Parse(pub); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errf(CodeFailedPrecondition, "VX_PUBLIC_URL %q is not an http(s) URL", pub)
	}
	if _, err := s.secretKey(); err != nil {
		return err
	}
	if s.GitHub.Connect == nil && s.GitHub.API == nil {
		return errf(CodeUnavailable, "this server cannot talk to GitHub (no client configured)")
	}
	return nil
}

// githubAppName is "vx (HOST)", cut to GitHub's limit.
func githubAppName(publicURL string) string {
	host := publicURL
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" {
		host = u.Host
	}
	name := "vx (" + host + ")"
	if len(name) > githubAppNameMax {
		name = "vx (" + host[:githubAppNameMax-6] + ")"
	}
	return name
}

// GitHubAppManifest builds the manifest of this server's App.
func (s *Service) GitHubAppManifest() ([]byte, error) {
	pub := s.publicURL()
	m := map[string]any{
		"name":                     githubAppName(pub),
		"url":                      pub,
		"description":              "vx: a workbench where coding agents land work into shared efforts before it goes to git as pull requests.",
		"hook_attributes":          map[string]any{"url": pub + GitHubWebhookPath, "active": true},
		"redirect_url":             pub + GitHubManifestReturnURL,
		"callback_urls":            []string{pub + GitHubCallbackPath},
		"setup_url":                pub + GitHubSetupURLPath,
		"setup_on_update":          true,
		"public":                   true, // other organizations install it too
		"request_oauth_on_install": false,
		"default_permissions":      GitHubAppPermissions,
		"default_events":           GitHubAppEvents,
	}
	return json.Marshal(m)
}

// GitHubSetupManifest returns the App manifest (JSON) and a state value for
// the manifest flow (site admins). The web page posts it to GitHub.
func (s *Service) GitHubSetupManifest(ctx context.Context, actor Actor) (manifest []byte, state string, err error) {
	if err := s.needSiteAdmin(ctx, actor, "set up the GitHub App"); err != nil {
		return nil, "", err
	}
	if err := s.githubSetupReady(); err != nil {
		return nil, "", err
	}
	manifest, err = s.GitHubAppManifest()
	if err != nil {
		return nil, "", err
	}
	state, err = s.signManifestState(actor.Human().ID, s.Now().Add(githubManifestStateTTL))
	return manifest, state, err
}

// CompleteGitHubSetup exchanges the manifest code for the App's
// credentials and stores them encrypted (site admins).
func (s *Service) CompleteGitHubSetup(ctx context.Context, actor Actor, code, state string) error {
	if err := s.needSiteAdmin(ctx, actor, "set up the GitHub App"); err != nil {
		return err
	}
	if err := s.githubSetupReady(); err != nil {
		return err
	}
	if err := s.checkManifestState(actor.Human().ID, state); err != nil {
		return err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return errf(CodeInvalid, "GitHub sent no code back; create the App again")
	}
	setup := s.GitHub.API
	if setup == nil {
		var err error
		if setup, err = s.GitHub.Connect(GitHubAppCredentials{}); err != nil {
			return err
		}
	}
	creds, err := setup.CreateAppFromManifest(ctx, code)
	if err != nil {
		return errf(CodeUnavailable, "GitHub did not hand over the new App: %v", err)
	}
	h := actor.Human()
	if err := s.SaveGitHubApp(ctx, creds, &h.ID); err != nil {
		return err
	}
	return s.Store.LogOp(ctx, nil, &h.ID, "github.app.create", creds.Slug, nil, map[string]any{"app_id": creds.AppID, "slug": creds.Slug, "html_url": creds.HTMLURL})
}

// Manifest state: who started the flow and until when, HMAC-signed with a
// key derived from the server key, so it needs no table and cannot be
// forged or used by another person.
func (s *Service) stateMAC(payload []byte) ([]byte, error) {
	key, err := s.secretKey()
	if err != nil {
		return nil, err
	}
	k := hmac.New(sha256.New, key)
	k.Write([]byte("vx github manifest state"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write(payload)
	return m.Sum(nil), nil
}

func (s *Service) signManifestState(person uuid.UUID, expires time.Time) (string, error) {
	payload := make([]byte, 16+8+8)
	copy(payload, person[:])
	binary.BigEndian.PutUint64(payload[16:], uint64(expires.Unix()))
	if _, err := rand.Read(payload[24:]); err != nil {
		return "", err
	}
	mac, err := s.stateMAC(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

func (s *Service) checkManifestState(person uuid.UUID, state string) error {
	bad := errf(CodeInvalid, "this GitHub App setup was not started here by you, or it is older than an hour; start it again on Admin > GitHub")
	p, m, ok := strings.Cut(state, ".")
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(m)
	if !ok || err1 != nil || err2 != nil || len(payload) != 32 {
		return bad
	}
	want, err := s.stateMAC(payload)
	if err != nil {
		return err
	}
	if !hmac.Equal(mac, want) || !hmac.Equal(payload[:16], person[:]) {
		return bad
	}
	if s.Now().Unix() > int64(binary.BigEndian.Uint64(payload[16:24])) {
		return bad
	}
	return nil
}
