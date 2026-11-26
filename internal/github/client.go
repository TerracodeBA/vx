// Package github is vx's client for the GitHub REST API as a GitHub App:
// the manifest flow, installations, installation tokens, OAuth sign-in,
// repository permissions and organization membership. It implements
// service.GitHubAPI. Base URLs are configurable for GitHub Enterprise and
// for tests, which run it against the fake in internal/github/githubtest.
//
// Errors never contain tokens, keys, client secrets or OAuth codes.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
)

// Default base URLs of github.com.
const (
	DefaultAPIURL = "https://api.github.com"
	DefaultWebURL = "https://github.com"
)

// Config configures a Client. Only the base URLs are needed to complete the
// manifest flow; everything else needs the App's credentials.
type Config struct {
	// APIURL is the REST API base: https://api.github.com, or
	// https://HOST/api/v3 for GitHub Enterprise Server. Empty: github.com.
	APIURL string
	// WebURL is the browser base: https://github.com or https://HOST.
	// OAuth code exchange goes here. Empty: github.com.
	WebURL string

	AppID         int64
	PrivateKeyPEM string
	ClientID      string
	ClientSecret  string

	// HTTPClient defaults to a client with a 30 second timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now.
	Now func() time.Time
}

// Client talks to GitHub as the App. It is safe for concurrent use.
type Client struct {
	apiURL, webURL string
	appID          int64
	key            *rsa.PrivateKey
	clientID       string
	clientSecret   string
	http           *http.Client
	now            func() time.Time

	mu     sync.Mutex
	tokens map[int64]installationToken
}

type installationToken struct {
	token   string
	expires time.Time
}

// tokenSlack is how long before its expiry a cached installation token is
// replaced, so a token handed out still works for a git push.
const tokenSlack = 5 * time.Minute

var _ service.GitHubAPI = (*Client)(nil)

// New returns a client. A private key, when given, must be an RSA key in
// PEM (PKCS#1 or PKCS#8), as GitHub hands it out.
func New(cfg Config) (*Client, error) {
	c := &Client{
		apiURL:       strings.TrimRight(cfg.APIURL, "/"),
		webURL:       strings.TrimRight(cfg.WebURL, "/"),
		appID:        cfg.AppID,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		http:         cfg.HTTPClient,
		now:          cfg.Now,
		tokens:       map[int64]installationToken{},
	}
	if c.apiURL == "" {
		c.apiURL = DefaultAPIURL
	}
	if c.webURL == "" {
		c.webURL = DefaultWebURL
	}
	for _, u := range []string{c.apiURL, c.webURL} {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			return nil, fmt.Errorf("github: base URL %q is not an http(s) URL", u)
		}
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if cfg.PrivateKeyPEM != "" {
		key, err := ParsePrivateKey(cfg.PrivateKeyPEM)
		if err != nil {
			return nil, err
		}
		c.key = key
	}
	return c, nil
}

// FromCredentials returns a client for an App created by the manifest flow.
func FromCredentials(creds service.GitHubAppCredentials, apiURL, webURL string) (*Client, error) {
	return New(Config{
		APIURL: apiURL, WebURL: webURL,
		AppID: creds.AppID, PrivateKeyPEM: creds.PrivateKeyPEM,
		ClientID: creds.ClientID, ClientSecret: creds.ClientSecret,
	})
}

// APIURL and WebURL return the configured base URLs.
func (c *Client) APIURL() string { return c.apiURL }
func (c *Client) WebURL() string { return c.webURL }

// ParsePrivateKey parses an App private key in PEM.
func ParsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("github: the App private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github: the App private key is not an RSA key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: the App private key is not an RSA key")
	}
	return rk, nil
}

// AppJWT returns a JSON Web Token that authenticates as the App, valid for
// at most ten minutes (RS256, issued a minute in the past for clock skew).
func (c *Client) AppJWT() (string, error) {
	if c.key == nil || c.appID == 0 {
		return "", errors.New("github: the App's id and private key are not configured")
	}
	now := c.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(c.appID, 10),
	})
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github: sign App token: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Error is a failed GitHub request. Path never contains secrets.
type Error struct {
	Method, Path string
	Status       int
	Message      string
	// RateLimited is set when GitHub refused for a rate limit; Reset is
	// when the limit resets, if GitHub said.
	RateLimited bool
	Reset       time.Time
}

func (e *Error) Error() string {
	switch {
	case e.RateLimited && !e.Reset.IsZero():
		return fmt.Sprintf("GitHub rate limit reached (%s %s); it resets at %s, try again then", e.Method, e.Path, e.Reset.UTC().Format(time.RFC3339))
	case e.RateLimited:
		return fmt.Sprintf("GitHub rate limit reached (%s %s); try again in a few minutes", e.Method, e.Path)
	case e.Status == http.StatusUnauthorized:
		return fmt.Sprintf("GitHub refused the credentials (%s %s: 401 %s); check the App's credentials or sign in again", e.Method, e.Path, e.Message)
	case e.Status == http.StatusNotFound:
		return fmt.Sprintf("GitHub: %s %s: not found, or the App cannot see it", e.Method, e.Path)
	}
	if e.Message != "" {
		return fmt.Sprintf("GitHub: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
	}
	return fmt.Sprintf("GitHub: %s %s: %d", e.Method, e.Path, e.Status)
}

// ErrNotFound matches (errors.Is) GitHub 404 responses.
var ErrNotFound = errors.New("github: not found")

// ErrUnauthorized matches (errors.Is) GitHub 401 responses.
var ErrUnauthorized = errors.New("github: unauthorized")

// ErrRateLimited matches (errors.Is) rate-limited responses.
var ErrRateLimited = errors.New("github: rate limited")

func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrRateLimited:
		return e.RateLimited
	case service.ErrGitHubForbidden:
		return e.Status == http.StatusForbidden && !e.RateLimited
	}
	return false
}

// auth is how a request authenticates.
type auth struct {
	scheme, value string // e.g. "Bearer", jwt
}

// do sends a request and decodes a JSON response into out. logPath is the
// path shown in errors (with any secret part replaced).
func (c *Client) do(ctx context.Context, method, rawURL, logPath string, a *auth, body io.Reader, contentType string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, logPath, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "vx")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if a != nil {
		req.Header.Set("Authorization", a.scheme+" "+a.value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error repeats the URL, which may hold a secret path part.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("github: %s %s: %w", method, logPath, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: read response: %w", method, logPath, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, apiError(method, logPath, resp, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, fmt.Errorf("github: %s %s: unexpected response: %w", method, logPath, err)
		}
	}
	return resp.Header, nil
}

func apiError(method, path string, resp *http.Response, data []byte) error {
	e := &Error{Method: method, Path: path, Status: resp.StatusCode}
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &msg) == nil {
		e.Message = msg.Message
	}
	if len(e.Message) > 200 {
		e.Message = e.Message[:200]
	}
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (remaining == "0" || resp.Header.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(e.Message), "rate limit"))) {
		e.RateLimited = true
		if r, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && r > 0 {
			e.Reset = time.Unix(r, 0)
		} else if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			e.Reset = time.Now().Add(time.Duration(s) * time.Second)
		}
	}
	return e
}

func (c *Client) api(ctx context.Context, method, path string, a *auth, in, out any) (http.Header, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	return c.do(ctx, method, c.apiURL+path, path, a, body, ct, out)
}

func (c *Client) appAuth() (*auth, error) {
	jwt, err := c.AppJWT()
	if err != nil {
		return nil, err
	}
	return &auth{"Bearer", jwt}, nil
}

// CreateAppFromManifest completes the manifest flow: it converts the
// one-time code GitHub sent back into the new App's credentials.
func (c *Client) CreateAppFromManifest(ctx context.Context, code string) (service.GitHubAppCredentials, error) {
	if code == "" || strings.ContainsAny(code, "/?#") {
		return service.GitHubAppCredentials{}, errors.New("github: the manifest code is missing or malformed")
	}
	var out struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		HTMLURL       string `json:"html_url"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
	}
	_, err := c.do(ctx, http.MethodPost, c.apiURL+"/app-manifests/"+url.PathEscape(code)+"/conversions",
		"/app-manifests/CODE/conversions", nil, nil, "", &out)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && (ge.Status == http.StatusNotFound || ge.Status == http.StatusUnprocessableEntity) {
			return service.GitHubAppCredentials{}, fmt.Errorf("github: the App manifest code was not accepted (it is valid for one hour and only once); create the App again: %w", err)
		}
		return service.GitHubAppCredentials{}, err
	}
	if out.ID == 0 || out.PEM == "" || out.ClientID == "" {
		return service.GitHubAppCredentials{}, errors.New("github: the manifest conversion returned no App credentials")
	}
	return service.GitHubAppCredentials{
		AppID: out.ID, Slug: out.Slug, HTMLURL: out.HTMLURL,
		ClientID: out.ClientID, ClientSecret: out.ClientSecret,
		PrivateKeyPEM: out.PEM, WebhookSecret: out.WebhookSecret,
	}, nil
}

type apiInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string     `json:"repository_selection"`
	SuspendedAt         *time.Time `json:"suspended_at"`
}

// Installation reads an installation of the App.
func (c *Client) Installation(ctx context.Context, id int64) (service.GitHubInstallation, error) {
	a, err := c.appAuth()
	if err != nil {
		return service.GitHubInstallation{}, err
	}
	var in apiInstallation
	if _, err := c.api(ctx, http.MethodGet, "/app/installations/"+strconv.FormatInt(id, 10), a, nil, &in); err != nil {
		return service.GitHubInstallation{}, err
	}
	return service.GitHubInstallation{
		ID: in.ID, AccountID: in.Account.ID, AccountLogin: in.Account.Login, AccountType: in.Account.Type,
		RepositorySelection: in.RepositorySelection, SuspendedAt: in.SuspendedAt,
	}, nil
}

// InstallationToken returns a token for an installation, cached until
// shortly before it expires.
func (c *Client) InstallationToken(ctx context.Context, id int64) (string, time.Time, error) {
	c.mu.Lock()
	t, ok := c.tokens[id]
	c.mu.Unlock()
	if ok && c.now().Add(tokenSlack).Before(t.expires) {
		return t.token, t.expires, nil
	}
	a, err := c.appAuth()
	if err != nil {
		return "", time.Time{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := c.api(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(id, 10)+"/access_tokens", a, nil, &out); err != nil {
		return "", time.Time{}, err
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("github: GitHub returned no installation token")
	}
	c.mu.Lock()
	c.tokens[id] = installationToken{out.Token, out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, out.ExpiresAt, nil
}

// ForgetInstallationToken drops a cached token, for example after GitHub
// refused it or the installation was deleted.
func (c *Client) ForgetInstallationToken(id int64) {
	c.mu.Lock()
	delete(c.tokens, id)
	c.mu.Unlock()
}

// installationAPI calls the API with an installation token and retries
// once with a fresh token when GitHub refuses the cached one.
func (c *Client) installationAPI(ctx context.Context, id int64, method, path string, out any) (http.Header, error) {
	return c.installationCall(ctx, id, method, path, nil, out)
}

// installationCall is installationAPI with a JSON request body.
func (c *Client) installationCall(ctx context.Context, id int64, method, path string, in, out any) (http.Header, error) {
	for attempt := 0; ; attempt++ {
		tok, _, err := c.InstallationToken(ctx, id)
		if err != nil {
			return nil, err
		}
		h, err := c.api(ctx, method, path, &auth{"Bearer", tok}, in, out)
		if attempt == 0 && errors.Is(err, ErrUnauthorized) {
			c.ForgetInstallationToken(id)
			continue
		}
		return h, err
	}
}

// InstallationRepos lists every repository an installation can reach,
// following pagination.
func (c *Client) InstallationRepos(ctx context.Context, id int64) ([]service.GitHubRepo, error) {
	var repos []service.GitHubRepo
	path := "/installation/repositories?per_page=100"
	for page := 0; path != "" && page < 1000; page++ {
		var out struct {
			Repositories []struct {
				ID            int64  `json:"id"`
				FullName      string `json:"full_name"`
				DefaultBranch string `json:"default_branch"`
				Private       bool   `json:"private"`
			} `json:"repositories"`
		}
		h, err := c.installationAPI(ctx, id, http.MethodGet, path, &out)
		if err != nil {
			return nil, err
		}
		for _, r := range out.Repositories {
			repos = append(repos, service.GitHubRepo{ID: r.ID, FullName: r.FullName, DefaultBranch: r.DefaultBranch, Private: r.Private})
		}
		path = c.nextPath(h.Get("Link"))
	}
	return repos, nil
}

// nextPath returns the API path of the rel="next" link, or "" when there
// is none or it points elsewhere.
func (c *Client) nextPath(link string) string {
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		isNext := false
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		u := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		if !strings.HasPrefix(u, c.apiURL+"/") {
			return ""
		}
		return strings.TrimPrefix(u, c.apiURL)
	}
	return ""
}

// ExchangeOAuthCode turns a sign-in code into a user access token (PKCE).
func (c *Client) ExchangeOAuthCode(ctx context.Context, code, redirectURI, codeVerifier string) (string, error) {
	if c.clientID == "" || c.clientSecret == "" {
		return "", errors.New("github: the App's client id and secret are not configured")
	}
	form := url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"code":          {code},
	}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "vx")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("github: POST /login/oauth/access_token: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", apiError(http.MethodPost, "/login/oauth/access_token", resp, data)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("github: POST /login/oauth/access_token: unexpected response: %w", err)
	}
	if out.Error != "" {
		if out.Error == "bad_verification_code" {
			return "", errors.New("github: the sign-in code is invalid or expired; sign in again")
		}
		return "", fmt.Errorf("github: sign-in refused: %s: %s", out.Error, out.ErrorDescription)
	}
	if out.AccessToken == "" {
		return "", errors.New("github: GitHub returned no access token")
	}
	return out.AccessToken, nil
}

// User returns the user a user access token belongs to. Email is the
// primary verified email when GitHub shares it, else empty.
func (c *Client) User(ctx context.Context, userToken string) (service.GitHubUser, error) {
	a := &auth{"Bearer", userToken}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if _, err := c.api(ctx, http.MethodGet, "/user", a, nil, &u); err != nil {
		return service.GitHubUser{}, err
	}
	out := service.GitHubUser{ID: u.ID, Login: u.Login, Name: u.Name}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	// The public email on /user is not known to be verified; ask for the
	// primary verified one. Without the email permission this is a 403 or
	// 404: then there is no email.
	if _, err := c.api(ctx, http.MethodGet, "/user/emails", a, nil, &emails); err == nil {
		for _, e := range emails {
			if e.Primary && e.Verified {
				out.Email = e.Email
			}
		}
	} else if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrRateLimited) {
		return service.GitHubUser{}, err
	}
	return out, nil
}

// RepoPermission returns login's permission on owner/repo: admin,
// maintain, write, triage, read or none. Unknown users and repositories the
// installation cannot see are "none".
func (c *Client) RepoPermission(ctx context.Context, installationID int64, owner, repo, login string) (string, error) {
	var out struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/collaborators/" + url.PathEscape(login) + "/permission"
	if _, err := c.installationAPI(ctx, installationID, http.MethodGet, path, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "none", nil
		}
		return "", err
	}
	switch out.RoleName {
	case "admin", "maintain", "write", "triage", "read":
		return out.RoleName, nil
	}
	switch out.Permission {
	case "admin", "maintain", "write", "triage", "read":
		return out.Permission, nil
	}
	return "none", nil
}

// OrgMembership returns login's role in org: admin, member or none
// (pending invitations are none).
func (c *Client) OrgMembership(ctx context.Context, installationID int64, org, login string) (string, error) {
	var out struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	path := "/orgs/" + url.PathEscape(org) + "/memberships/" + url.PathEscape(login)
	if _, err := c.installationAPI(ctx, installationID, http.MethodGet, path, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "none", nil
		}
		return "", err
	}
	if out.State != "active" {
		return "none", nil
	}
	switch out.Role {
	case "admin", "member":
		return out.Role, nil
	}
	return "none", nil
}

// CreateCheckRun posts a completed check run on a commit of owner/repo.
// A refusal for want of the checks: write permission matches
// service.ErrGitHubForbidden.
func (c *Client) CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run service.GitHubCheckRun) error {
	in := map[string]any{
		"name": run.Name, "head_sha": run.HeadSHA, "status": "completed", "conclusion": run.Conclusion,
		"output": map[string]any{"title": run.Title, "summary": run.Summary, "text": run.Text},
	}
	if run.DetailsURL != "" {
		in["details_url"] = run.DetailsURL
	}
	if run.ExternalID != "" {
		in["external_id"] = run.ExternalID
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/check-runs"
	_, err := c.installationCall(ctx, installationID, http.MethodPost, path, in, nil)
	return err
}

// checkRunBody is the JSON of a check run to create or update. A run
// without Status, or with "completed", is completed with its Conclusion;
// "queued" and "in_progress" leave it open.
func checkRunBody(run service.GitHubCheckRun, create bool) map[string]any {
	status := run.Status
	if status == "" {
		status = "completed"
	}
	in := map[string]any{
		"name": run.Name, "status": status,
		"output": map[string]any{"title": run.Title, "summary": run.Summary, "text": run.Text},
	}
	if create {
		in["head_sha"] = run.HeadSHA
	}
	if status == "completed" {
		in["conclusion"] = run.Conclusion
	}
	if run.DetailsURL != "" {
		in["details_url"] = run.DetailsURL
	}
	if run.ExternalID != "" {
		in["external_id"] = run.ExternalID
	}
	return in
}

// StartCheckRun creates a check run on a commit of owner/repo and returns
// its id, for UpdateCheckRun. A refusal for want of the checks: write
// permission matches service.ErrGitHubForbidden.
func (c *Client) StartCheckRun(ctx context.Context, installationID int64, owner, repo string, run service.GitHubCheckRun) (int64, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/check-runs"
	var out struct {
		ID int64 `json:"id"`
	}
	if _, err := c.installationCall(ctx, installationID, http.MethodPost, path, checkRunBody(run, true), &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// UpdateCheckRun changes a check run StartCheckRun created: its status,
// conclusion and output.
func (c *Client) UpdateCheckRun(ctx context.Context, installationID int64, owner, repo string, id int64, run service.GitHubCheckRun) error {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/check-runs/" + strconv.FormatInt(id, 10)
	_, err := c.installationCall(ctx, installationID, http.MethodPatch, path, checkRunBody(run, false), nil)
	return err
}

var _ service.GitHubCheckRunsAPI = (*Client)(nil)

// AppSettings reads the App's permissions and webhook events (GET /app).
func (c *Client) AppSettings(ctx context.Context) (service.GitHubAppSettings, error) {
	a, err := c.appAuth()
	if err != nil {
		return service.GitHubAppSettings{}, err
	}
	var out struct {
		Permissions map[string]string `json:"permissions"`
		Events      []string          `json:"events"`
	}
	if _, err := c.api(ctx, http.MethodGet, "/app", a, nil, &out); err != nil {
		return service.GitHubAppSettings{}, err
	}
	return service.GitHubAppSettings{Permissions: out.Permissions, Events: out.Events}, nil
}

var _ service.GitHubChecksAPI = (*Client)(nil)
