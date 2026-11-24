package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// A server with a GitHub App loads it at start with VX_SECRET_KEY, serves
// the webhook endpoint, and refuses to start without the key.
func TestServerLoadsGitHubApp(t *testing.T) {
	ctx := context.Background()
	dbURL := storetest.NewDatabase(t)
	gh := githubtest.New(t)
	key := bytes.Repeat([]byte{3}, 32)
	cfg := Config{DatabaseURL: dbURL, DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		GitHub: service.GitHubSettings{PublicURL: "https://vx.test", SecretKey: key, WebURL: gh.URL, APIURL: gh.APIURL}}

	srv, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	creds := gh.NewApp()
	if err := srv.Service.SaveGitHubApp(ctx, creds, nil); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	// Restart: the App loads, with the default client against the fake.
	srv, err = New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	gh.AddOrg("acme")
	id := gh.Install("acme")
	api, err := srv.Service.GitHubClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Installation(ctx, id); err != nil {
		t.Fatalf("default client: %v", err)
	}
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	if code := gh.Deliver(t, ts.URL+"/hooks/github", "ping", map[string]any{"zen": "hi"}); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}

	cfg.GitHub.SecretKey = nil
	if _, err := New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "VX_SECRET_KEY") {
		t.Fatalf("start without the key: %v", err)
	}
}
