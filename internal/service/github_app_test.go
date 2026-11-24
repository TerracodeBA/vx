package service_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func (e *env) siteAdmin(handle string) service.Actor {
	e.t.Helper()
	p, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: handle, SiteAdmin: true})
	if err != nil {
		e.t.Fatal(err)
	}
	a, _ := e.svc.ResolveActor(e.ctx, p)
	return a
}

func TestGitHubManifestRoundTrip(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	gh := githubtest.New(t)
	gh.Prepare(t, svc)
	svc.GitHub.PublicURL = "https://vx.example.com/"
	root := e.siteAdmin("root")

	if _, err := svc.GitHubClient(ctx); !errors.Is(err, service.ErrGitHubNotConfigured) {
		t.Fatalf("before setup: %v", err)
	}
	if u, err := svc.GitHubAppInstallURL(ctx); err != nil || u != "" {
		t.Fatalf("install URL before setup: %q %v", u, err)
	}
	// Only site admins, never agents.
	if _, _, err := svc.GitHubSetupManifest(ctx, e.alice); code(err) != service.CodePermission {
		t.Fatalf("non-admin: %v", err)
	}
	rootAgent := e.agentOf(root, "bot")
	if _, _, err := svc.GitHubSetupManifest(ctx, rootAgent); code(err) != service.CodePermission {
		t.Fatalf("agent: %v", err)
	}

	manifest, state, err := svc.GitHubSetupManifest(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name string `json:"name"`
		URL  string `json:"url"`
		Hook struct {
			URL    string `json:"url"`
			Active bool   `json:"active"`
		} `json:"hook_attributes"`
		Redirect  string            `json:"redirect_url"`
		Callbacks []string          `json:"callback_urls"`
		SetupURL  string            `json:"setup_url"`
		OAuth     bool              `json:"request_oauth_on_install"`
		Perms     map[string]string `json:"default_permissions"`
		Events    []string          `json:"default_events"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "vx (vx.example.com)" || m.URL != "https://vx.example.com" || m.Hook.URL != "https://vx.example.com/hooks/github" || !m.Hook.Active ||
		m.Redirect != "https://vx.example.com/ui/admin/github/created" || len(m.Callbacks) != 1 || m.Callbacks[0] != "https://vx.example.com/auth/github/callback" ||
		m.SetupURL != "https://vx.example.com/ui/github/installed" || m.OAuth {
		t.Fatalf("manifest %s", manifest)
	}
	if m.Perms["contents"] != "write" || m.Perms["pull_requests"] != "write" || m.Perms["metadata"] != "read" || m.Perms["checks"] != "write" || m.Perms["statuses"] != "read" || m.Perms["members"] != "read" {
		t.Fatalf("permissions %v", m.Perms)
	}
	if !strings.Contains(strings.Join(m.Events, ","), "push,pull_request") || !strings.Contains(strings.Join(m.Events, ","), "team_add") {
		t.Fatalf("events %v", m.Events)
	}
	if got := svc.GitHubManifestFormURL("acme", state); !strings.HasPrefix(got, gh.URL+"/organizations/acme/settings/apps/new?state=") {
		t.Fatalf("form URL %s", got)
	}

	code1 := gh.ManifestCode(manifest)
	// The state belongs to root and this flow.
	if err := svc.CompleteGitHubSetup(ctx, root, code1, state+"x"); code(err) != service.CodeInvalid {
		t.Fatalf("bad state: %v", err)
	}
	other := e.siteAdmin("other")
	if err := svc.CompleteGitHubSetup(ctx, other, code1, state); code(err) != service.CodeInvalid {
		t.Fatalf("someone else's state: %v", err)
	}
	if err := svc.CompleteGitHubSetup(ctx, e.alice, code1, state); code(err) != service.CodePermission {
		t.Fatalf("non-admin completes: %v", err)
	}
	if err := svc.CompleteGitHubSetup(ctx, root, code1, state); err != nil {
		t.Fatal(err)
	}
	creds, _ := gh.App()
	app, err := svc.GitHubApp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if app.AppID != creds.AppID || app.ClientID != creds.ClientID || app.InstallURL != creds.HTMLURL+"/installations/new" || app.AuthorizeURL != gh.URL+"/login/oauth/authorize" {
		t.Fatalf("app %+v", app)
	}
	if u, _ := svc.GitHubAppInstallURL(ctx); u != app.InstallURL {
		t.Fatalf("install URL %q", u)
	}

	// Secrets are sealed in the database.
	row, err := svc.Store.GitHubApp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"client secret": {string(row.ClientSecretEnc), creds.ClientSecret},
		"private key":   {string(row.PrivateKeyEnc), creds.PrivateKeyPEM},
		"webhook":       {string(row.WebhookSecretEnc), creds.WebhookSecret},
	} {
		if pair[0] == "" || strings.Contains(pair[0], pair[1]) || bytes.Contains([]byte(pair[0]), []byte("PRIVATE KEY")) {
			t.Errorf("%s stored in plaintext", name)
		}
	}

	// The client works, and a second server on the same database loads it.
	gh.AddOrg("acme")
	id := gh.Install("acme")
	api, err := svc.GitHubClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if in, err := api.Installation(ctx, id); err != nil || in.AccountLogin != "acme" {
		t.Fatalf("installation %+v %v", in, err)
	}
	svc2 := service.New(svc.Store, e.blobs, nil)
	svc2.GitHub = service.GitHubSettings{SecretKey: svc.GitHub.SecretKey, WebURL: gh.URL, APIURL: gh.APIURL, Connect: gh.Connector()}
	if err := svc2.LoadGitHubApp(ctx); err != nil {
		t.Fatal(err)
	}
	if api2, err := svc2.GitHubClient(ctx); err != nil {
		t.Fatal(err)
	} else if _, err := api2.Installation(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Without the key, or with another, the server says what to do.
	svc3 := service.New(svc.Store, e.blobs, nil)
	svc3.GitHub = service.GitHubSettings{Connect: gh.Connector()}
	if err := svc3.LoadGitHubApp(ctx); err == nil || !strings.Contains(err.Error(), "VX_SECRET_KEY") {
		t.Fatalf("no key: %v", err)
	}
	svc3.GitHub.SecretKey = bytes.Repeat([]byte{7}, 32)
	if err := svc3.LoadGitHubApp(ctx); err == nil || !strings.Contains(err.Error(), "VX_SECRET_KEY") {
		t.Fatalf("wrong key: %v", err)
	}
	// A code works once.
	_, state2, _ := svc.GitHubSetupManifest(ctx, root)
	if err := svc.CompleteGitHubSetup(ctx, root, code1, state2); code(err) != service.CodeUnavailable {
		t.Fatalf("reused code: %v", err)
	}
}

func TestGitHubSetupNeedsPublicURLAndKey(t *testing.T) {
	e := newEnv(t)
	gh := githubtest.New(t)
	root := e.siteAdmin("root")
	e.svc.GitHub.Connect = gh.Connector()
	_, _, err := e.svc.GitHubSetupManifest(e.ctx, root)
	if code(err) != service.CodeFailedPrecondition || !strings.Contains(err.Error(), "VX_PUBLIC_URL") {
		t.Fatalf("no public URL: %v", err)
	}
	e.svc.GitHub.PublicURL = "https://vx.example.com"
	_, _, err = e.svc.GitHubSetupManifest(e.ctx, root)
	if code(err) != service.CodeFailedPrecondition || !strings.Contains(err.Error(), "VX_SECRET_KEY") {
		t.Fatalf("no key: %v", err)
	}
	// An expired state is refused.
	gh.Prepare(t, e.svc)
	_, state, err := e.svc.GitHubSetupManifest(e.ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := e.svc.CompleteGitHubSetup(e.ctx, root, "code", state); code(err) != service.CodeInvalid {
		t.Fatalf("expired state: %v", err)
	}
}
