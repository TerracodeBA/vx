package githubtest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
)

func TestGitHubFakeManifestFormAndDelivery(t *testing.T) {
	gh := githubtest.New(t)
	manifest := `{"name":"vx (vx.test)","redirect_url":"https://vx.test/ui/admin/github/created","hook_attributes":{"url":"https://vx.test/hooks/github"}}`
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.PostForm(gh.URL+"/settings/apps/new?state=s1", url.Values{"manifest": {manifest}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Host != "vx.test" || loc.Query().Get("state") != "s1" || loc.Query().Get("code") == "" {
		t.Fatalf("manifest form: %d %s", resp.StatusCode, loc)
	}
	creds, err := gh.SetupClient(t).CreateAppFromManifest(context.Background(), loc.Query().Get("code"))
	if err != nil {
		t.Fatal(err)
	}
	if gh.Manifest()["name"] != "vx (vx.test)" {
		t.Fatalf("manifest not kept: %v", gh.Manifest())
	}

	var got http.Header
	var body []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hook.Close()
	gh.AddOrg("acme")
	gh.AddRepo("acme/app", true)
	id := gh.Install("acme")
	if code := gh.Deliver(t, hook.URL, "installation", gh.InstallationEvent("created", id)); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if got.Get("X-GitHub-Event") != "installation" || got.Get("X-GitHub-Delivery") == "" ||
		got.Get("X-Hub-Signature-256") != githubtest.Sign(creds.WebhookSecret, body) || !strings.HasPrefix(got.Get("X-Hub-Signature-256"), "sha256=") {
		t.Fatalf("headers %v", got)
	}
	var p struct {
		Action       string `json:"action"`
		Installation struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		} `json:"installation"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Action != "created" || p.Installation.ID != id || p.Installation.Account.Login != "acme" || len(p.Repositories) != 1 || p.Repositories[0].FullName != "acme/app" {
		t.Fatalf("payload %s", body)
	}
	gh.DeliverRaw(t, hook.URL, githubtest.Delivery{Event: "ping", Body: []byte(`{}`), Signature: "-"})
	if got.Get("X-Hub-Signature-256") != "" {
		t.Fatal("unsigned delivery was signed")
	}
}
