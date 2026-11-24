// Package githubtest is a fake GitHub for tests: an httptest server with
// the parts of the REST API and the browser flows vx uses, plus helpers to
// create users, organizations and repositories, set permissions, install
// the App and fire signed webhook deliveries.
//
// The fake serves the browser side at URL (like https://github.com) and
// the API at APIURL = URL + "/api/v3" (like GitHub Enterprise Server), so
// tests exercise configurable base URLs.
//
//	gh := githubtest.New(t)
//	creds := gh.NewApp()                  // or the manifest flow
//	api := gh.Client(t)                   // *github.Client against the fake
//	alice := gh.AddUser("alice")
//	gh.AddOrg("acme")
//	gh.AddRepo("acme/shop", false)
//	gh.SetPermission("acme/shop", "alice", "write")
//	inst := gh.Install("acme")            // all repositories of acme
//	gh.Deliver(t, server.URL+"/hooks/github", "installation", gh.InstallationEvent("created", inst))
package githubtest

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/github"
	"github.com/TerracodeBA/vx/internal/service"
)

// Account is a GitHub user or organization.
type Account struct {
	ID    int64
	Login string
	Type  string // User or Organization
	Name  string
	Email string // primary verified email; empty: none shared
}

// Repo is a repository on the fake.
type Repo struct {
	ID            int64
	Owner         string
	Name          string
	DefaultBranch string
	Private       bool
}

// FullName is owner/name.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// Installation is an installation of the App.
type Installation struct {
	ID          int64
	Account     string // login
	All         bool   // repository_selection all
	Repos       []int64
	SuspendedAt *time.Time
}

// Server is a fake GitHub. Its methods are safe for concurrent use.
type Server struct {
	// URL is the browser base (WebURL), APIURL the REST API base.
	URL    string
	APIURL string
	// Now is the fake's clock, for token expiry and App JWT checks.
	Now func() time.Time
	// TokenTTL is how long installation tokens live (default one hour).
	TokenTTL time.Duration
	// PageSize is the page size of paginated lists (default 30), so tests
	// see pagination with few repositories.
	PageSize int

	srv *httptest.Server

	mu         sync.Mutex
	nextID     int64
	app        *service.GitHubAppCredentials
	appKey     *rsa.PublicKey
	manifest   map[string]any // the manifest the App was created from
	accounts   map[string]*Account
	repos      map[int64]*Repo
	perms      map[int64]map[string]string // repo id -> login -> permission
	members    map[string]map[string]string
	installs   map[int64]*Installation
	manifests  map[string][]byte // code -> manifest JSON
	oauthCodes map[string]oauthGrant
	userTokens map[string]string // token -> login
	instTokens map[string]instToken
	browser    string // login signed in at the fake's web pages
	deny       bool   // the signed-in person clicks Cancel on the authorize page
	calls      map[string]int
	failures   []failure
}

type oauthGrant struct {
	login, redirectURI, challenge string
	expires                       time.Time
}

type instToken struct {
	installation int64
	expires      time.Time
}

type failure struct {
	pattern string
	status  int
	left    int
}

// New starts a fake GitHub, closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{
		Now:        time.Now,
		TokenTTL:   time.Hour,
		PageSize:   30,
		nextID:     1000,
		accounts:   map[string]*Account{},
		repos:      map[int64]*Repo{},
		perms:      map[int64]map[string]string{},
		members:    map[string]map[string]string{},
		installs:   map[int64]*Installation{},
		manifests:  map[string][]byte{},
		oauthCodes: map[string]oauthGrant{},
		userTokens: map[string]string{},
		instTokens: map[string]instToken{},
		calls:      map[string]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	s.APIURL = s.srv.URL + "/api/v3"
	t.Cleanup(s.srv.Close)
	return s
}

// Close stops the server early; New already closes it at the end of the test.
func (s *Server) Close() { s.srv.Close() }

func (s *Server) id() int64 { s.nextID++; return s.nextID }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ---- App ----

// NewApp creates the App directly, as if the manifest flow had run, and
// returns its credentials.
func (s *Server) NewApp() service.GitHubAppCredentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createApp(map[string]any{"name": "vx test"})
}

func (s *Server) createApp(manifest map[string]any) service.GitHubAppCredentials {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	name, _ := manifest["name"].(string)
	slug := slugify(name)
	if slug == "" {
		slug = "vx"
	}
	creds := service.GitHubAppCredentials{
		AppID:         s.id(),
		Slug:          slug,
		HTMLURL:       s.URL + "/apps/" + slug,
		ClientID:      "Iv1." + randHex(8),
		ClientSecret:  randHex(20),
		PrivateKeyPEM: pemText,
		WebhookSecret: randHex(16),
	}
	s.app = &creds
	s.appKey = &key.PublicKey
	s.manifest = manifest
	return creds
}

func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// App returns the App's credentials; ok is false before it exists.
func (s *Server) App() (service.GitHubAppCredentials, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.app == nil {
		return service.GitHubAppCredentials{}, false
	}
	return *s.app, true
}

// Manifest returns the manifest the App was created from (decoded JSON).
func (s *Server) Manifest() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.manifest
}

// ManifestCode registers a manifest as if a site admin had posted it to
// GitHub and confirmed, and returns the one-time code GitHub sends back.
func (s *Server) ManifestCode(manifestJSON []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := randHex(10)
	s.manifests[code] = append([]byte(nil), manifestJSON...)
	return code
}

// Client returns a github.Client for the App against the fake, creating
// the App first if it does not exist yet.
func (s *Server) Client(t testing.TB) *github.Client {
	creds, ok := s.App()
	if !ok {
		creds = s.NewApp()
	}
	c, err := github.New(github.Config{
		APIURL: s.APIURL, WebURL: s.URL,
		AppID: creds.AppID, PrivateKeyPEM: creds.PrivateKeyPEM,
		ClientID: creds.ClientID, ClientSecret: creds.ClientSecret,
		Now: s.Now,
	})
	if err != nil {
		t.Fatalf("githubtest: client: %v", err)
	}
	return c
}

// SetupClient returns a client without App credentials, which can only
// complete the manifest flow.
func (s *Server) SetupClient(t testing.TB) *github.Client {
	c, err := github.New(github.Config{APIURL: s.APIURL, WebURL: s.URL, Now: s.Now})
	if err != nil {
		t.Fatalf("githubtest: client: %v", err)
	}
	return c
}

// ---- accounts, repositories, permissions ----

// AddUser creates a user with name = login and email login@example.com.
func (s *Server) AddUser(login string) Account {
	return s.addAccount(login, "User")
}

// AddOrg creates an organization.
func (s *Server) AddOrg(login string) Account {
	return s.addAccount(login, "Organization")
}

func (s *Server) addAccount(login, typ string) Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(login)
	if a, ok := s.accounts[key]; ok {
		return *a
	}
	a := &Account{ID: s.id(), Login: login, Type: typ}
	if typ == "User" {
		a.Name, a.Email = login, strings.ToLower(login)+"@example.com"
	}
	s.accounts[key] = a
	return *a
}

// Account returns an account by login.
func (s *Server) Account(login string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[strings.ToLower(login)]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

// SetProfile sets a user's name and primary verified email ("" for none).
func (s *Server) SetProfile(login, name, email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.mustAccount(login)
	a.Name, a.Email = name, email
}

// RenameAccount changes a user's or organization's login (the id stays).
func (s *Server) RenameAccount(login, newLogin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.mustAccount(login)
	delete(s.accounts, strings.ToLower(login))
	old := a.Login
	a.Login = newLogin
	s.accounts[strings.ToLower(newLogin)] = a
	for _, r := range s.repos {
		if strings.EqualFold(r.Owner, old) {
			r.Owner = newLogin
		}
	}
	for _, in := range s.installs {
		if strings.EqualFold(in.Account, old) {
			in.Account = newLogin
		}
	}
}

func (s *Server) mustAccount(login string) *Account {
	a, ok := s.accounts[strings.ToLower(login)]
	if !ok {
		panic("githubtest: no account " + login)
	}
	return a
}

// AddRepo creates owner/name (the owner must exist) with default branch main.
func (s *Server) AddRepo(fullName string, private bool) Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		panic("githubtest: repository must be owner/name: " + fullName)
	}
	a := s.mustAccount(owner)
	if r := s.repoLocked(fullName); r != nil {
		return *r
	}
	r := &Repo{ID: s.id(), Owner: a.Login, Name: name, DefaultBranch: "main", Private: private}
	s.repos[r.ID] = r
	if a.Type == "User" {
		s.perms[r.ID] = map[string]string{strings.ToLower(a.Login): "admin"}
	}
	return *r
}

// Repo returns a repository by full name.
func (s *Server) Repo(fullName string) (Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repoLocked(fullName)
	if r == nil {
		return Repo{}, false
	}
	return *r, true
}

func (s *Server) repoLocked(fullName string) *Repo {
	for _, r := range s.repos {
		if strings.EqualFold(r.FullName(), fullName) {
			return r
		}
	}
	return nil
}

func (s *Server) mustRepo(fullName string) *Repo {
	r := s.repoLocked(fullName)
	if r == nil {
		panic("githubtest: no repository " + fullName)
	}
	return r
}

// RenameRepo renames a repository (its id stays).
func (s *Server) RenameRepo(fullName, newName string) Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRepo(fullName)
	r.Name = newName
	return *r
}

// TransferRepo moves a repository to another owner (its id stays).
func (s *Server) TransferRepo(fullName, newOwner string) Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRepo(fullName)
	r.Owner = s.mustAccount(newOwner).Login
	return *r
}

// SetPrivate changes a repository's visibility.
func (s *Server) SetPrivate(fullName string, private bool) Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRepo(fullName)
	r.Private = private
	return *r
}

// SetDefaultBranch changes a repository's default branch.
func (s *Server) SetDefaultBranch(fullName, branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustRepo(fullName).DefaultBranch = branch
}

// DeleteRepo deletes a repository and returns what it was.
func (s *Server) DeleteRepo(fullName string) Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRepo(fullName)
	delete(s.repos, r.ID)
	delete(s.perms, r.ID)
	for _, in := range s.installs {
		in.Repos = without(in.Repos, r.ID)
	}
	return *r
}

// SetPermission sets login's permission on a repository: admin, maintain,
// write, triage, read, or none/"" to remove it.
func (s *Server) SetPermission(fullName, login, perm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mustRepo(fullName)
	s.mustAccount(login)
	switch perm {
	case "admin", "maintain", "write", "triage", "read":
	case "", "none":
		delete(s.perms[r.ID], strings.ToLower(login))
		return
	default:
		panic("githubtest: unknown permission " + perm)
	}
	if s.perms[r.ID] == nil {
		s.perms[r.ID] = map[string]string{}
	}
	s.perms[r.ID][strings.ToLower(login)] = perm
}

// SetOrgMember sets login's role in an organization: admin, member, or ""
// to remove them.
func (s *Server) SetOrgMember(org, login, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustAccount(org)
	s.mustAccount(login)
	o := strings.ToLower(org)
	if role == "" || role == "none" {
		delete(s.members[o], strings.ToLower(login))
		return
	}
	if s.members[o] == nil {
		s.members[o] = map[string]string{}
	}
	s.members[o][strings.ToLower(login)] = role
}

// permission is login's permission on r: explicit, else the owner's, else
// org admins get admin; public repositories give read.
func (s *Server) permission(r *Repo, login string) string {
	l := strings.ToLower(login)
	if p, ok := s.perms[r.ID][l]; ok {
		return p
	}
	if s.members[strings.ToLower(r.Owner)][l] == "admin" {
		return "admin"
	}
	if !r.Private {
		if _, ok := s.accounts[l]; ok {
			return "read"
		}
	}
	return "none"
}

// ---- installations ----

// Install installs the App on an account. With no repositories it gets all
// of the account's repositories, now and later.
func (s *Server) Install(account string, repos ...string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.app == nil {
		s.createApp(map[string]any{"name": "vx test"})
	}
	a := s.mustAccount(account)
	in := &Installation{ID: s.id(), Account: a.Login, All: len(repos) == 0}
	for _, full := range repos {
		in.Repos = append(in.Repos, s.mustRepo(full).ID)
	}
	s.installs[in.ID] = in
	return in.ID
}

// AddInstallationRepos adds repositories to a "selected" installation.
func (s *Server) AddInstallationRepos(id int64, repos ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.mustInstall(id)
	for _, full := range repos {
		rid := s.mustRepo(full).ID
		in.Repos = append(without(in.Repos, rid), rid)
	}
}

// RemoveInstallationRepos removes repositories from an installation (an
// "all" installation becomes "selected").
func (s *Server) RemoveInstallationRepos(id int64, repos ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.mustInstall(id)
	if in.All {
		in.All = false
		in.Repos = nil
		for _, r := range s.installReposLocked(&Installation{Account: in.Account, All: true}) {
			in.Repos = append(in.Repos, r.ID)
		}
	}
	for _, full := range repos {
		in.Repos = without(in.Repos, s.mustRepo(full).ID)
	}
}

// SuspendInstallation suspends or unsuspends an installation.
func (s *Server) SuspendInstallation(id int64, suspended bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.mustInstall(id)
	if suspended {
		t := s.Now().UTC().Truncate(time.Second)
		in.SuspendedAt = &t
	} else {
		in.SuspendedAt = nil
	}
}

// DeleteInstallation uninstalls the App; its tokens stop working.
func (s *Server) DeleteInstallation(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.installs, id)
	for tok, it := range s.instTokens {
		if it.installation == id {
			delete(s.instTokens, tok)
		}
	}
}

// Installation returns an installation.
func (s *Server) Installation(id int64) (Installation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.installs[id]
	if !ok {
		return Installation{}, false
	}
	return *in, true
}

func (s *Server) mustInstall(id int64) *Installation {
	in, ok := s.installs[id]
	if !ok {
		panic(fmt.Sprintf("githubtest: no installation %d", id))
	}
	return in
}

// installReposLocked lists the repositories an installation reaches, by id.
func (s *Server) installReposLocked(in *Installation) []*Repo {
	var out []*Repo
	if in.All {
		for _, r := range s.repos {
			if strings.EqualFold(r.Owner, in.Account) {
				out = append(out, r)
			}
		}
	} else {
		for _, id := range in.Repos {
			if r, ok := s.repos[id]; ok {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func without(ids []int64, id int64) []int64 {
	out := ids[:0:0]
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

// ---- tokens, sign-in ----

// InstallationTokenFor mints an installation token directly.
func (s *Server) InstallationTokenFor(id int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mintInstallToken(id)
}

func (s *Server) mintInstallToken(id int64) string {
	tok := "ghs_" + randHex(18)
	s.instTokens[tok] = instToken{installation: id, expires: s.Now().Add(s.TokenTTL)}
	return tok
}

// ExpireTokens makes every installation and user token invalid.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instTokens = map[string]instToken{}
	s.userTokens = map[string]string{}
}

// SignIn makes login the person signed in at the fake's web pages, so
// /login/oauth/authorize approves for them right away. "" signs out.
func (s *Server) SignIn(login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.browser = login
}

// DenyAuthorization makes the authorize page redirect back with
// error=access_denied, as when the person cancels.
func (s *Server) DenyAuthorization(deny bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deny = deny
}

// OAuthCode mints a sign-in code for login, as GitHub's authorize page
// would after the person approved. codeChallenge is the PKCE S256
// challenge ("" for none); redirectURI must match the exchange's.
func (s *Server) OAuthCode(login, redirectURI, codeChallenge string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustAccount(login)
	return s.mintOAuthCode(login, redirectURI, codeChallenge)
}

func (s *Server) mintOAuthCode(login, redirectURI, challenge string) string {
	code := randHex(10)
	s.oauthCodes[code] = oauthGrant{login: login, redirectURI: redirectURI, challenge: challenge, expires: s.Now().Add(10 * time.Minute)}
	return code
}

// UserToken mints a user access token for login.
func (s *Server) UserToken(login string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustAccount(login)
	tok := "ghu_" + randHex(18)
	s.userTokens[tok] = login
	return tok
}

// ---- call counting and failures ----

// Calls returns how many requests matched "METHOD /path" so far, with
// numbers and names in the path as sent, e.g.
// "POST /app/installations/1001/access_tokens". A pattern ending in "*"
// matches by prefix. API paths are given without the /api/v3 prefix.
func (s *Server) Calls(pattern string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, v := range s.calls {
		if matches(pattern, k) {
			n += v
		}
	}
	return n
}

// Fail makes the next n requests matching pattern (see Calls) fail with
// status. 403 and 429 come back as rate limits, as GitHub sends them.
func (s *Server) Fail(pattern string, status, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failure{pattern: pattern, status: status, left: n})
}

func matches(pattern, call string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(call, p)
	}
	return pattern == call
}

// ---- webhooks ----

// Sign returns the X-Hub-Signature-256 value of body with the App's
// webhook secret.
func (s *Server) Sign(body []byte) string {
	creds, ok := s.App()
	if !ok {
		panic("githubtest: no App yet")
	}
	return Sign(creds.WebhookSecret, body)
}

// Sign returns the X-Hub-Signature-256 value of body with secret.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Delivery is one webhook delivery.
type Delivery struct {
	Event     string
	ID        string // X-GitHub-Delivery; empty: a new random id
	Body      []byte
	Signature string // X-Hub-Signature-256; empty: signed with the webhook secret, "-": no header
}

// Deliver posts a webhook event signed with the App's webhook secret, with
// a new delivery id made at s.Now(), and returns the response status code.
func (s *Server) Deliver(t testing.TB, hookURL, event string, payload any) int {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("githubtest: payload: %v", err)
	}
	return s.DeliverRaw(t, hookURL, Delivery{Event: event, Body: body})
}

// DeliverRaw posts a delivery as given, for tests of bad signatures and
// replays, and returns the response status code.
func (s *Server) DeliverRaw(t testing.TB, hookURL string, d Delivery) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, hookURL, strings.NewReader(string(d.Body)))
	if err != nil {
		t.Fatalf("githubtest: delivery: %v", err)
	}
	if d.ID == "" {
		d.ID = DeliveryIDAt(s.Now())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GitHub-Hookshot/fake")
	req.Header.Set("X-GitHub-Event", d.Event)
	req.Header.Set("X-GitHub-Delivery", d.ID)
	if creds, ok := s.App(); ok {
		req.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.FormatInt(creds.AppID, 10))
	}
	switch d.Signature {
	case "":
		req.Header.Set("X-Hub-Signature-256", s.Sign(d.Body))
	case "-":
	default:
		req.Header.Set("X-Hub-Signature-256", d.Signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("githubtest: delivery: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// NewDeliveryID returns a delivery id like GitHub's: a version 1 UUID,
// which carries the time it was made (now).
func NewDeliveryID() string { return DeliveryIDAt(time.Now()) }

// DeliveryIDAt returns a delivery id made at t, for tests of stale
// deliveries.
func DeliveryIDAt(t time.Time) string {
	var b [16]byte
	if _, err := rand.Read(b[8:]); err != nil {
		panic(err)
	}
	// 100-nanosecond intervals since 1582-10-15, the UUID epoch.
	ts := uint64(t.UnixNano()/100) + 0x01B21DD213814000
	b[0], b[1], b[2], b[3] = byte(ts>>24), byte(ts>>16), byte(ts>>8), byte(ts)
	b[4], b[5] = byte(ts>>40), byte(ts>>32)
	b[6], b[7] = byte(ts>>56)&0x0f|0x10, byte(ts>>48)
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ---- HTTP ----

var pathNumber = func(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMsg(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg, "documentation_url": "https://docs.github.com/rest"})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	api := false
	if p, ok := strings.CutPrefix(path, "/api/v3"); ok {
		path, api = p, true
		if path == "" {
			path = "/"
		}
	}
	call := r.Method + " " + path
	s.mu.Lock()
	s.calls[call]++
	for i := range s.failures {
		f := &s.failures[i]
		if f.left > 0 && matches(f.pattern, call) {
			f.left--
			s.mu.Unlock()
			if f.status == http.StatusForbidden || f.status == http.StatusTooManyRequests {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(s.Now().Add(time.Hour).Unix(), 10))
				writeMsg(w, f.status, "API rate limit exceeded")
				return
			}
			writeMsg(w, f.status, http.StatusText(f.status))
			return
		}
	}
	s.mu.Unlock()

	if !api {
		s.serveWeb(w, r, path)
		return
	}
	seg := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && len(seg) == 3 && seg[0] == "app-manifests" && seg[2] == "conversions":
		s.convertManifest(w, seg[1])
	case r.Method == http.MethodGet && len(seg) == 3 && seg[0] == "app" && seg[1] == "installations":
		s.getInstallation(w, r, seg[2])
	case r.Method == http.MethodGet && len(seg) == 2 && seg[0] == "app" && seg[1] == "installations":
		s.listInstallations(w, r)
	case r.Method == http.MethodGet && len(seg) == 1 && seg[0] == "app":
		s.getApp(w, r)
	case r.Method == http.MethodPost && len(seg) == 4 && seg[0] == "app" && seg[1] == "installations" && seg[3] == "access_tokens":
		s.createInstallToken(w, r, seg[2])
	case r.Method == http.MethodGet && path == "/installation/repositories":
		s.listInstallRepos(w, r)
	case r.Method == http.MethodGet && path == "/user":
		s.getUser(w, r)
	case r.Method == http.MethodGet && path == "/user/emails":
		s.getEmails(w, r)
	case r.Method == http.MethodGet && len(seg) == 6 && seg[0] == "repos" && seg[3] == "collaborators" && seg[5] == "permission":
		s.getPermission(w, r, seg[1], seg[2], seg[4])
	case r.Method == http.MethodGet && len(seg) == 4 && seg[0] == "orgs" && seg[2] == "memberships":
		s.getMembership(w, r, seg[1], seg[3])
	case r.Method == http.MethodGet && len(seg) == 3 && seg[0] == "repos":
		s.getRepo(w, r, seg[1], seg[2])
	default:
		writeMsg(w, http.StatusNotFound, "Not Found")
	}
}

// serveWeb handles the browser side: creating the App from a manifest form,
// the OAuth authorize page and the token exchange.
func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request, path string) {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && path == "/settings/apps/new":
		// the manifest form: GitHub shows a confirmation, then redirects
		s.manifestForm(w, r)
	case r.Method == http.MethodPost && len(seg) == 5 && seg[0] == "organizations" && seg[2] == "settings" && seg[3] == "apps" && seg[4] == "new":
		s.manifestForm(w, r)
	case r.Method == http.MethodGet && path == "/login/oauth/authorize":
		s.authorize(w, r)
	case r.Method == http.MethodPost && path == "/login/oauth/access_token":
		s.accessToken(w, r)
	case r.Method == http.MethodGet && len(seg) == 4 && seg[0] == "apps" && seg[2] == "installations" && seg[3] == "new":
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html><body>Install %s</body></html>", seg[1])
	default:
		writeMsg(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) manifestForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeMsg(w, http.StatusBadRequest, "bad form")
		return
	}
	raw := r.PostForm.Get("manifest")
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		writeMsg(w, http.StatusUnprocessableEntity, "manifest is not JSON")
		return
	}
	redirect, _ := m["redirect_url"].(string)
	if redirect == "" {
		writeMsg(w, http.StatusUnprocessableEntity, "manifest has no redirect_url")
		return
	}
	code := s.ManifestCode([]byte(raw))
	u, err := url.Parse(redirect)
	if err != nil {
		writeMsg(w, http.StatusUnprocessableEntity, "bad redirect_url")
		return
	}
	q := u.Query()
	q.Set("code", code)
	if st := r.URL.Query().Get("state"); st != "" {
		q.Set("state", st)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	app, browser := s.app, s.browser
	s.mu.Unlock()
	if app == nil || q.Get("client_id") != app.ClientID {
		writeMsg(w, http.StatusNotFound, "unknown client_id")
		return
	}
	if browser == "" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>Sign in to GitHub (fake): call SignIn(login) first</body></html>")
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		if cb, ok := s.Manifest()["callback_urls"].([]any); ok && len(cb) > 0 {
			redirect, _ = cb[0].(string)
		}
	}
	if q.Get("code_challenge") != "" && q.Get("code_challenge_method") != "S256" {
		writeMsg(w, http.StatusBadRequest, "code_challenge_method must be S256")
		return
	}
	u, err := url.Parse(redirect)
	if err != nil || redirect == "" {
		writeMsg(w, http.StatusBadRequest, "bad redirect_uri")
		return
	}
	rq := u.Query()
	s.mu.Lock()
	if s.deny {
		rq.Set("error", "access_denied")
		rq.Set("error_description", "The user has denied your application access.")
	} else {
		rq.Set("code", s.mintOAuthCode(browser, q.Get("redirect_uri"), q.Get("code_challenge")))
	}
	s.mu.Unlock()
	if st := q.Get("state"); st != "" {
		rq.Set("state", st)
	}
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) accessToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeMsg(w, http.StatusBadRequest, "bad form")
		return
	}
	f := r.PostForm
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.app == nil || f.Get("client_id") != s.app.ClientID || f.Get("client_secret") != s.app.ClientSecret {
		writeJSON(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials", "error_description": "The client_id and/or client_secret passed are incorrect."})
		return
	}
	g, ok := s.oauthCodes[f.Get("code")]
	delete(s.oauthCodes, f.Get("code"))
	if !ok || s.Now().After(g.expires) {
		writeJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code", "error_description": "The code passed is incorrect or expired."})
		return
	}
	if g.redirectURI != "" && f.Get("redirect_uri") != g.redirectURI {
		writeJSON(w, http.StatusOK, map[string]string{"error": "redirect_uri_mismatch", "error_description": "The redirect_uri does not match."})
		return
	}
	if g.challenge != "" {
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			writeJSON(w, http.StatusOK, map[string]string{"error": "invalid_grant", "error_description": "The code_verifier does not match the code_challenge."})
			return
		}
	}
	tok := "ghu_" + randHex(18)
	s.userTokens[tok] = g.login
	writeJSON(w, http.StatusOK, map[string]string{"access_token": tok, "token_type": "bearer", "scope": ""})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	for _, p := range []string{"Bearer ", "bearer ", "token "} {
		if t, ok := strings.CutPrefix(h, p); ok {
			return t
		}
	}
	return ""
}

// appAuthorized checks the App JWT: RS256 with the App key, iss the App
// id, not expired and at most ten minutes long.
func (s *Server) appAuthorized(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	key, app := s.appKey, s.app
	s.mu.Unlock()
	fail := func(msg string) bool {
		writeMsg(w, http.StatusUnauthorized, msg)
		return false
	}
	if key == nil {
		return fail("no App")
	}
	parts := strings.Split(bearer(r), ".")
	if len(parts) != 3 {
		return fail("A JSON web token could not be decoded")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if json.Unmarshal(hb, &header) != nil || header.Alg != "RS256" {
		return fail("'alg' must be RS256")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fail("bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return fail("A JSON web token could not be decoded")
	}
	var claims struct {
		Iat int64           `json:"iat"`
		Exp int64           `json:"exp"`
		Iss json.RawMessage `json:"iss"`
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(cb, &claims) != nil {
		return fail("bad claims")
	}
	iss := strings.Trim(string(claims.Iss), `"`)
	if iss != strconv.FormatInt(app.AppID, 10) && iss != app.ClientID {
		return fail("'Issuer' claim ('iss') must be the App id")
	}
	now := s.Now().Unix()
	if claims.Exp <= now {
		return fail("'Expiration time' claim ('exp') must be a numeric value representing the future time")
	}
	if claims.Exp-claims.Iat > 600 {
		return fail("'Expiration time' claim ('exp') is too far in the future")
	}
	if claims.Iat > now+60 {
		return fail("'Issued at' claim ('iat') must be an Integer representing a time in the past")
	}
	return true
}

// installationFor returns the installation an installation token belongs to.
func (s *Server) installationFor(w http.ResponseWriter, r *http.Request) (*Installation, bool) {
	tok := bearer(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.instTokens[tok]
	if !ok || s.Now().After(it.expires) {
		writeMsg(w, http.StatusUnauthorized, "Bad credentials")
		return nil, false
	}
	in, ok := s.installs[it.installation]
	if !ok {
		writeMsg(w, http.StatusUnauthorized, "Bad credentials")
		return nil, false
	}
	if in.SuspendedAt != nil {
		writeMsg(w, http.StatusForbidden, "This installation has been suspended")
		return nil, false
	}
	return in, true
}

func (s *Server) convertManifest(w http.ResponseWriter, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.manifests[code]
	delete(s.manifests, code)
	if !ok {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	creds := s.createApp(m)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": creds.AppID, "slug": creds.Slug, "html_url": creds.HTMLURL, "name": m["name"],
		"client_id": creds.ClientID, "client_secret": creds.ClientSecret,
		"pem": creds.PrivateKeyPEM, "webhook_secret": creds.WebhookSecret,
		"owner": map[string]any{"login": "vx-admin", "type": "User"},
	})
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	if !s.appAuthorized(w, r) {
		return
	}
	creds, _ := s.App()
	writeJSON(w, http.StatusOK, map[string]any{"id": creds.AppID, "slug": creds.Slug, "html_url": creds.HTMLURL, "client_id": creds.ClientID})
}

func (s *Server) installationJSON(in *Installation) map[string]any {
	a := s.accounts[strings.ToLower(in.Account)]
	sel := "selected"
	if in.All {
		sel = "all"
	}
	var susp any
	if in.SuspendedAt != nil {
		susp = in.SuspendedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id":                   in.ID,
		"account":              map[string]any{"id": a.ID, "login": a.Login, "type": a.Type},
		"repository_selection": sel,
		"suspended_at":         susp,
		"target_type":          a.Type,
		"app_id":               s.app.AppID,
	}
}

func (s *Server) getInstallation(w http.ResponseWriter, r *http.Request, idText string) {
	if !s.appAuthorized(w, r) {
		return
	}
	id, _ := pathNumber(idText)
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.installs[id]
	if !ok {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.installationJSON(in))
}

func (s *Server) listInstallations(w http.ResponseWriter, r *http.Request) {
	if !s.appAuthorized(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id := range s.installs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []map[string]any{}
	for _, id := range ids {
		out = append(out, s.installationJSON(s.installs[id]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createInstallToken(w http.ResponseWriter, r *http.Request, idText string) {
	if !s.appAuthorized(w, r) {
		return
	}
	id, _ := pathNumber(idText)
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.installs[id]
	if !ok {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	if in.SuspendedAt != nil {
		writeMsg(w, http.StatusForbidden, "This installation has been suspended")
		return
	}
	tok := s.mintInstallToken(id)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": tok, "expires_at": s.instTokens[tok].expires.UTC().Format(time.RFC3339),
		"permissions": map[string]string{"metadata": "read", "contents": "write", "pull_requests": "write", "checks": "read", "members": "read"},
	})
}

func repoJSON(rp *Repo) map[string]any {
	return map[string]any{
		"id": rp.ID, "name": rp.Name, "full_name": rp.FullName(), "private": rp.Private,
		"default_branch": rp.DefaultBranch, "owner": map[string]any{"login": rp.Owner},
	}
}

func (s *Server) listInstallRepos(w http.ResponseWriter, r *http.Request) {
	in, ok := s.installationFor(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.installReposLocked(in)
	per := s.PageSize
	if n, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && n > 0 && n < per {
		per = n
	}
	page := 1
	if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && n > 0 {
		page = n
	}
	start := min((page-1)*per, len(all))
	end := min(start+per, len(all))
	out := []map[string]any{}
	for _, rp := range all[start:end] {
		out = append(out, repoJSON(rp))
	}
	if end < len(all) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		next := s.APIURL + "/installation/repositories?" + q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(all), "repositories": out})
}

func (s *Server) userFor(w http.ResponseWriter, r *http.Request) (*Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	login, ok := s.userTokens[bearer(r)]
	if !ok {
		writeMsg(w, http.StatusUnauthorized, "Bad credentials")
		return nil, false
	}
	a, ok := s.accounts[strings.ToLower(login)]
	if !ok {
		writeMsg(w, http.StatusUnauthorized, "Bad credentials")
		return nil, false
	}
	return a, true
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	a, ok := s.userFor(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "login": a.Login, "name": a.Name, "email": nil, "type": "User"})
}

func (s *Server) getEmails(w http.ResponseWriter, r *http.Request) {
	a, ok := s.userFor(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	if a.Email != "" {
		out = append(out, map[string]any{"email": a.Email, "primary": true, "verified": true, "visibility": "private"})
	}
	writeJSON(w, http.StatusOK, out)
}

// repoInInstallation finds owner/name among the installation's repositories.
func (s *Server) repoInInstallation(in *Installation, owner, name string) *Repo {
	for _, rp := range s.installReposLocked(in) {
		if strings.EqualFold(rp.Owner, owner) && strings.EqualFold(rp.Name, name) {
			return rp
		}
	}
	return nil
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request, owner, name string) {
	in, ok := s.installationFor(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.repoInInstallation(in, owner, name)
	if rp == nil {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, repoJSON(rp))
}

func (s *Server) getPermission(w http.ResponseWriter, r *http.Request, owner, name, login string) {
	in, ok := s.installationFor(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.repoInInstallation(in, owner, name)
	if rp == nil {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	if _, ok := s.accounts[strings.ToLower(login)]; !ok {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	role := s.permission(rp, login)
	legacy := role
	switch role {
	case "maintain":
		legacy = "write"
	case "triage":
		legacy = "read"
	}
	writeJSON(w, http.StatusOK, map[string]any{"permission": legacy, "role_name": role, "user": map[string]any{"login": login}})
}

func (s *Server) getMembership(w http.ResponseWriter, r *http.Request, org, login string) {
	in, ok := s.installationFor(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.EqualFold(in.Account, org) {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	role, ok := s.members[strings.ToLower(org)][strings.ToLower(login)]
	if !ok {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": "active", "role": role, "organization": map[string]any{"login": org}, "user": map[string]any{"login": login}})
}
