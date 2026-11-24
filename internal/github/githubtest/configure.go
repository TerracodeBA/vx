package githubtest

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/TerracodeBA/vx/internal/github"
	"github.com/TerracodeBA/vx/internal/service"
)

// Connector returns a service.GitHubSettings.Connect function that makes
// clients against the fake.
func (s *Server) Connector() func(service.GitHubAppCredentials) (service.GitHubAPI, error) {
	return func(creds service.GitHubAppCredentials) (service.GitHubAPI, error) {
		return github.New(github.Config{
			APIURL: s.APIURL, WebURL: s.URL,
			AppID: creds.AppID, PrivateKeyPEM: creds.PrivateKeyPEM,
			ClientID: creds.ClientID, ClientSecret: creds.ClientSecret,
			Now: s.Now,
		})
	}
}

// Prepare points svc at the fake without creating an App: a random server
// key, the fake's URLs, Connect, and PublicURL http://vx.test when empty.
// The manifest flow can then run.
func (s *Server) Prepare(t testing.TB, svc *service.Service) {
	t.Helper()
	if len(svc.GitHub.SecretKey) == 0 {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		svc.GitHub.SecretKey = key
	}
	if svc.GitHub.PublicURL == "" {
		svc.GitHub.PublicURL = "http://vx.test"
	}
	svc.GitHub.WebURL, svc.GitHub.APIURL = s.URL, s.APIURL
	svc.GitHub.Connect = s.Connector()
}

// Configure is Prepare plus an App: it creates the App on the fake (if it
// does not exist) and saves it in svc, so svc.GitHubClient, svc.GitHubApp
// and webhook verification work.
func (s *Server) Configure(t testing.TB, svc *service.Service) service.GitHubAppCredentials {
	t.Helper()
	s.Prepare(t, svc)
	creds, ok := s.App()
	if !ok {
		creds = s.NewApp()
	}
	if err := svc.SaveGitHubApp(context.Background(), creds, nil); err != nil {
		t.Fatalf("githubtest: save App: %v", err)
	}
	return creds
}
