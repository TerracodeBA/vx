package service_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func hmacSig(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type hostHookEnv struct {
	*env
	up *recordingUpstream
}

func newHostHookEnv(t *testing.T) *hostHookEnv {
	e := &hostHookEnv{env: newEnv(t), up: &recordingUpstream{}}
	e.svc.Upstream = e.up
	e.svc.GitHub.SecretKey = []byte(strings.Repeat("k", 32))
	e.svc.GitHub.PublicURL = "https://vx.example.com"
	return e
}

// create registers a hosted repository and returns its webhook secret.
func (e *hostHookEnv) create(name string, p store.Provider, remote, base string) string {
	e.t.Helper()
	if _, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: name, Provider: p, Remote: remote, BaseURL: base}); err != nil {
		e.t.Fatalf("create %s: %v", name, err)
	}
	h, err := e.svc.SetRepoHost(e.ctx, e.alice, service.SetRepoHostInput{Repo: name, RotateWebhookSecret: true})
	if err != nil {
		e.t.Fatal(err)
	}
	if h.Secret == "" || h.SecretSource != "repository" || h.URL != "https://vx.example.com"+service.HostWebhookPath(p) {
		e.t.Fatalf("hook = %+v", h)
	}
	return h.Secret
}

func (e *hostHookEnv) syncs() []string {
	e.up.mu.Lock()
	defer e.up.mu.Unlock()
	return append([]string(nil), e.up.syncs...)
}

func gitlabPush(project, web, ref, after string) map[string]any {
	return map[string]any{"object_kind": "push", "ref": ref, "after": after,
		"project": map[string]any{"path_with_namespace": project, "web_url": web}}
}

// GitLab deliveries are verified with X-Gitlab-Token: good, bad, missing.
// A push to the default branch imports it; a deleted branch, another
// branch or another GitLab server's project with the same path do not.
func TestGitLabWebhook(t *testing.T) {
	e := newHostHookEnv(t)
	secret := e.create("acme/shop", store.ProviderGitLab, "acme/platform/shop", "https://gitlab.example.com")
	body := jsonBody(t, gitlabPush("acme/platform/shop", "https://gitlab.example.com/acme/platform/shop", "refs/heads/main", "abc123"))
	deliver := func(token, id string, body []byte) error {
		return e.svc.HandleHostWebhook(e.ctx, store.ProviderGitLab, service.HostDelivery{Event: "Push Hook", ID: id, Token: token, Body: body})
	}
	n := len(e.syncs())
	if err := deliver("", "d1", body); !isCode(err, service.CodePermission) || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("missing token: %v", err)
	}
	if err := deliver("wrong", "d1", body); !isCode(err, service.CodePermission) {
		t.Fatalf("bad token: %v", err)
	}
	if len(e.syncs()) != n {
		t.Fatal("an unverified delivery imported")
	}
	if err := deliver(secret, "d1", body); err != nil {
		t.Fatalf("good token: %v", err)
	}
	if got := e.syncs(); len(got) != n+1 || got[n] != "acme/shop@main from acme/platform/shop" {
		t.Fatalf("push imports %v", got)
	}
	// The same delivery again is a replay.
	if err := deliver(secret, "d1", body); !isCode(err, service.CodeExists) {
		t.Fatalf("replay: %v", err)
	}
	for _, b := range [][]byte{
		jsonBody(t, gitlabPush("acme/platform/shop", "https://gitlab.example.com/acme/platform/shop", "refs/heads/main", "0000000000000000000000000000000000000000")),
		jsonBody(t, gitlabPush("acme/platform/shop", "https://gitlab.example.com/acme/platform/shop", "refs/heads/topic", "abc")),
		jsonBody(t, gitlabPush("acme/platform/shop", "https://gitlab.example.com/acme/platform/shop", "refs/tags/v1", "abc")),
	} {
		if err := deliver(secret, "", b); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.syncs()) != n+1 {
		t.Fatalf("deleted branch, unfollowed branch or tag imported: %v", e.syncs())
	}
	// Same path on another GitLab: no repository follows it there.
	other := jsonBody(t, gitlabPush("acme/platform/shop", "https://gitlab.com/acme/platform/shop", "refs/heads/main", "abc"))
	if err := deliver(secret, "", other); !isCode(err, service.CodePermission) {
		t.Fatalf("other host: %v", err)
	}
	// A merged vx merge request imports its target branch.
	merged := jsonBody(t, map[string]any{"object_kind": "merge_request",
		"project":           map[string]any{"path_with_namespace": "acme/platform/shop", "web_url": "https://gitlab.example.com/acme/platform/shop"},
		"object_attributes": map[string]any{"action": "merge", "state": "merged", "source_branch": "vx/rate-limiting", "target_branch": "main"}})
	if err := deliver(secret, "d2", merged); err != nil {
		t.Fatal(err)
	}
	if got := e.syncs(); len(got) != n+2 || got[n+1] != "acme/shop@main from acme/platform/shop" {
		t.Fatalf("merged MR imports %v", got)
	}
	// After a new secret the old one stops working.
	h, err := e.svc.SetRepoHost(e.ctx, e.alice, service.SetRepoHostInput{Repo: "acme/shop", RotateWebhookSecret: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := deliver(secret, "d3", body); !isCode(err, service.CodePermission) {
		t.Fatalf("old secret after rotation: %v", err)
	}
	if err := deliver(h.Secret, "d3", body); err != nil {
		t.Fatalf("new secret: %v", err)
	}
}

// Gitea and Forgejo sign deliveries with HMAC-SHA256 (X-Gitea-Signature,
// X-Forgejo-Signature); one endpoint serves both.
func TestGiteaWebhook(t *testing.T) {
	e := newHostHookEnv(t)
	gitea := e.create("acme/shop", store.ProviderGitea, "acme/shop", "https://gitea.example.com")
	forgejo := e.create("acme/site", store.ProviderForgejo, "acme/site", "https://codeberg.org")
	push := func(full, web string) []byte {
		return jsonBody(t, map[string]any{"ref": "refs/heads/main", "after": "abc",
			"repository": map[string]any{"full_name": full, "html_url": web}})
	}
	deliver := func(kind store.Provider, event, sig string, body []byte) error {
		return e.svc.HandleHostWebhook(e.ctx, kind, service.HostDelivery{Event: event, Signature: sig, Body: body})
	}
	n := len(e.syncs())
	body := push("acme/shop", "https://gitea.example.com/acme/shop")
	if err := deliver(store.ProviderGitea, "push", "", body); !isCode(err, service.CodePermission) {
		t.Fatalf("missing signature: %v", err)
	}
	if err := deliver(store.ProviderGitea, "push", hmacSig("wrong", body), body); !isCode(err, service.CodePermission) {
		t.Fatalf("bad signature: %v", err)
	}
	if err := deliver(store.ProviderGitea, "push", hmacSig(gitea, body), body); err != nil {
		t.Fatalf("good signature: %v", err)
	}
	// Forgejo through /hooks/forgejo and through /hooks/gitea.
	fb := push("acme/site", "https://codeberg.org/acme/site")
	if err := deliver(store.ProviderForgejo, "push", hmacSig(forgejo, fb), fb); err != nil {
		t.Fatal(err)
	}
	if err := deliver(store.ProviderGitea, "push", hmacSig(forgejo, fb), fb); err != nil {
		t.Fatal(err)
	}
	// One repository's secret does not open another's deliveries.
	if err := deliver(store.ProviderGitea, "push", hmacSig(gitea, fb), fb); !isCode(err, service.CodePermission) {
		t.Fatalf("other repo's secret: %v", err)
	}
	merged := jsonBody(t, map[string]any{"action": "closed",
		"repository":   map[string]any{"full_name": "acme/shop", "html_url": "https://gitea.example.com/acme/shop"},
		"pull_request": map[string]any{"merged": true, "head": map[string]any{"ref": "vx/x"}, "base": map[string]any{"ref": "main"}}})
	if err := deliver(store.ProviderGitea, "pull_request", hmacSig(gitea, merged), merged); err != nil {
		t.Fatal(err)
	}
	// Closed without merging: nothing.
	closed := jsonBody(t, map[string]any{"action": "closed",
		"repository":   map[string]any{"full_name": "acme/shop", "html_url": "https://gitea.example.com/acme/shop"},
		"pull_request": map[string]any{"merged": false, "head": map[string]any{"ref": "vx/x"}, "base": map[string]any{"ref": "main"}}})
	if err := deliver(store.ProviderGitea, "pull_request", hmacSig(gitea, closed), closed); err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/shop@main from acme/shop", "acme/site@main from acme/site", "acme/site@main from acme/site", "acme/shop@main from acme/shop"}
	if got := e.syncs()[n:]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("imports %v, want %v", got, want)
	}
}

// Bitbucket signs with X-Hub-Signature when the webhook has a secret;
// hooks without one may carry it in the URL (?secret=).
func TestBitbucketWebhook(t *testing.T) {
	e := newHostHookEnv(t)
	secret := e.create("acme/shop", store.ProviderBitbucket, "acme/shop", "https://bitbucket.org")
	body := jsonBody(t, map[string]any{
		"repository": map[string]any{"full_name": "acme/shop", "links": map[string]any{"html": map[string]any{"href": "https://bitbucket.org/acme/shop"}}},
		"push": map[string]any{"changes": []any{
			map[string]any{"new": map[string]any{"type": "branch", "name": "main"}},
			map[string]any{"new": nil}, // a deleted branch
			map[string]any{"new": map[string]any{"type": "tag", "name": "v1"}},
		}},
	})
	deliver := func(d service.HostDelivery) error {
		d.Event, d.Body = "repo:push", body
		return e.svc.HandleHostWebhook(e.ctx, store.ProviderBitbucket, d)
	}
	n := len(e.syncs())
	if err := deliver(service.HostDelivery{}); !isCode(err, service.CodePermission) {
		t.Fatalf("missing: %v", err)
	}
	if err := deliver(service.HostDelivery{Signature: "sha256=" + hmacSig("wrong", body)}); !isCode(err, service.CodePermission) {
		t.Fatalf("bad signature: %v", err)
	}
	if err := deliver(service.HostDelivery{URLSecret: "wrong"}); !isCode(err, service.CodePermission) {
		t.Fatalf("bad URL secret: %v", err)
	}
	if err := deliver(service.HostDelivery{Signature: "sha256=" + hmacSig(secret, body)}); err != nil {
		t.Fatalf("good signature: %v", err)
	}
	if err := deliver(service.HostDelivery{URLSecret: secret}); err != nil {
		t.Fatalf("good URL secret: %v", err)
	}
	if got := e.syncs()[n:]; len(got) != 2 || got[0] != "acme/shop@main from acme/shop" {
		t.Fatalf("imports %v", got)
	}
	fulfilled := jsonBody(t, map[string]any{
		"repository":  map[string]any{"full_name": "acme/shop", "links": map[string]any{"html": map[string]any{"href": "https://bitbucket.org/acme/shop"}}},
		"pullrequest": map[string]any{"state": "MERGED", "source": map[string]any{"branch": map[string]any{"name": "vx/x"}}, "destination": map[string]any{"branch": map[string]any{"name": "main"}}},
	})
	if err := e.svc.HandleHostWebhook(e.ctx, store.ProviderBitbucket, service.HostDelivery{Event: "pullrequest:fulfilled", Signature: "sha256=" + hmacSig(secret, fulfilled), Body: fulfilled}); err != nil {
		t.Fatal(err)
	}
	if got := e.syncs()[n:]; len(got) != 3 {
		t.Fatalf("merged PR imports %v", got)
	}
}

// Without a secret of its own a repository's deliveries are verified with
// the server's secret for the host; with none at all they are refused.
func TestHostWebhookServerSecret(t *testing.T) {
	e := newHostHookEnv(t)
	if _, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "acme/shop", Provider: store.ProviderGitLab, Remote: "acme/shop", BaseURL: "https://gitlab.com"}); err != nil {
		t.Fatal(err)
	}
	body := jsonBody(t, gitlabPush("acme/shop", "https://gitlab.com/acme/shop", "refs/heads/main", "abc"))
	deliver := func(tok string, body []byte) error {
		return e.svc.HandleHostWebhook(e.ctx, store.ProviderGitLab, service.HostDelivery{Token: tok, Body: body})
	}
	if err := deliver("server-secret", body); !isCode(err, service.CodePermission) {
		t.Fatalf("no secret anywhere: %v", err)
	}
	e.svc.Hosts.WebhookSecrets = map[store.Provider]string{store.ProviderGitLab: "server-secret"}
	n := len(e.syncs())
	if err := deliver("server-secret", body); err != nil {
		t.Fatal(err)
	}
	if len(e.syncs()) != n+1 {
		t.Fatal("not imported")
	}
	// A project vx does not follow, signed by this server: accepted, ignored.
	if err := deliver("server-secret", jsonBody(t, gitlabPush("other/thing", "https://gitlab.com/other/thing", "refs/heads/main", "abc"))); err != nil {
		t.Fatal(err)
	}
	if err := deliver("nope", jsonBody(t, gitlabPush("other/thing", "https://gitlab.com/other/thing", "refs/heads/main", "abc"))); !isCode(err, service.CodePermission) {
		t.Fatalf("unknown project, bad secret: %v", err)
	}
	if err := deliver("server-secret", []byte("{not json")); !isCode(err, service.CodeInvalid) {
		t.Fatalf("not JSON: %v", err)
	}
}

// Creating hosted repositories checks the path, the address and the key
// a token needs; tokens never come back.
func TestCreateHostedRepo(t *testing.T) {
	e := newHostHookEnv(t)
	for _, c := range []struct {
		p              store.Provider
		remote, base   string
		wantErr        string
		wantRemote     string
		wantBaseURL    string
		withoutKeyFail bool
	}{
		{p: store.ProviderGitLab, remote: "acme/sub/shop.git", base: "https://gitlab.example.com/", wantRemote: "acme/sub/shop", wantBaseURL: "https://gitlab.example.com"},
		{p: store.ProviderGitea, remote: "acme/sub/shop", base: "https://gitea.example.com", wantErr: "owner/repo"},
		{p: store.ProviderGitea, remote: "acme/shop", wantErr: "--gitea-url"},
		{p: store.ProviderBitbucket, remote: "acme/shop", base: "ftp://bitbucket.org", wantErr: "http(s) URL"},
		{p: store.ProviderForgejo, remote: "acme/shop", base: "https://user:pw@codeberg.org", wantErr: "without credentials"},
	} {
		r, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "x/" + string(c.p) + "-" + strings.ReplaceAll(c.remote, "/", "-"), Provider: c.p, Remote: c.remote, BaseURL: c.base, Token: "tok"})
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s %s %s: err = %v, want %q", c.p, c.remote, c.base, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.p, err)
		}
		if r.Remote != c.wantRemote || r.BaseURL != c.wantBaseURL {
			t.Errorf("repo = %+v", r)
		}
		tok, err := e.svc.HostRepoToken(e.ctx, r)
		if err != nil || tok != "tok" {
			t.Errorf("stored token = %q, %v", tok, err)
		}
		sec, _ := e.svc.Store.GetUpstreamSecrets(e.ctx, r.ID)
		if strings.Contains(string(sec.TokenEnc), "tok") {
			t.Error("the token is stored in plaintext")
		}
		empty := ""
		if _, err := e.svc.SetRepoHost(e.ctx, e.alice, service.SetRepoHostInput{Repo: r.Name, Token: &empty}); err != nil {
			t.Fatal(err)
		}
		if tok, _ := e.svc.HostRepoToken(e.ctx, r); tok != "" {
			t.Errorf("token after clearing = %q", tok)
		}
	}
	if _, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "x/gh", Provider: store.ProviderGitHub, Remote: "a/b", Token: "t"}); err == nil {
		t.Error("a token for a GitHub repo was accepted")
	}
	e.svc.GitHub.SecretKey = nil
	if _, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "x/nokey", Provider: store.ProviderGitLab, Remote: "a/b", BaseURL: "https://gitlab.com", Token: "t"}); err == nil || !strings.Contains(err.Error(), "VX_SECRET_KEY") {
		t.Errorf("token without a server key: %v", err)
	}
}
