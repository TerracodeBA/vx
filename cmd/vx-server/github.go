package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/TerracodeBA/vx/internal/secret"
	"github.com/TerracodeBA/vx/internal/service"
)

// githubSettings reads the GitHub configuration from the environment:
//
//	VX_PUBLIC_URL      this server's public address, e.g. https://vx.example.com
//	VX_SECRET_KEY      32 bytes (64 hex characters or base64) sealing secrets at rest
//	VX_SIGNUP          invite (default), open or closed
//	VX_GITHUB_URL      GitHub's web address (GitHub Enterprise: https://HOST)
//	VX_GITHUB_API_URL  GitHub's API (GitHub Enterprise: https://HOST/api/v3)
//
// The key may be missing until a GitHub App exists; the server refuses to
// start without it after that.
func githubSettings() (service.GitHubSettings, error) {
	var gs service.GitHubSettings
	if v := strings.TrimSpace(os.Getenv("VX_PUBLIC_URL")); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return gs, fmt.Errorf("VX_PUBLIC_URL %q must be an http(s) address without a path, like https://vx.example.com", v)
		}
		gs.PublicURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("VX_SECRET_KEY"); strings.TrimSpace(v) != "" {
		key, err := secret.ParseKey(v)
		if err != nil {
			return gs, err
		}
		gs.SecretKey = key
	}
	mode, err := service.ParseSignupMode(os.Getenv("VX_SIGNUP"))
	if err != nil {
		return gs, fmt.Errorf("VX_SIGNUP: %w", err)
	}
	gs.Signup = mode
	for _, f := range []struct {
		env  string
		into *string
	}{{"VX_GITHUB_URL", &gs.WebURL}, {"VX_GITHUB_API_URL", &gs.APIURL}} {
		v := strings.TrimRight(strings.TrimSpace(os.Getenv(f.env)), "/")
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return gs, fmt.Errorf("%s %q is not an http(s) URL", f.env, v)
		}
		*f.into = v
	}
	if (gs.WebURL == "") != (gs.APIURL == "") {
		return gs, fmt.Errorf("set both VX_GITHUB_URL and VX_GITHUB_API_URL for GitHub Enterprise, or neither for github.com")
	}
	return gs, nil
}
