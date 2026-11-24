package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// hostEnv names the environment of one kind of host:
//
//	VX_<HOST>_URL             the host's web address repositories default to
//	VX_<HOST>_API_URL         its API, when not derived from the address
//	VX_<HOST>_TOKEN           the server's token for repositories on VX_<HOST>_URL
//	VX_<HOST>_WEBHOOK_SECRET  verifies webhooks of repositories without their own secret
//
// for HOST in GITLAB, GITEA, FORGEJO and BITBUCKET.
var hostEnv = []store.Provider{store.ProviderGitLab, store.ProviderGitea, store.ProviderForgejo, store.ProviderBitbucket}

func hostVar(p store.Provider, name string) string {
	return "VX_" + strings.ToUpper(string(p)) + "_" + name
}

// configureHosts applies VX_<HOST>_URL, _API_URL and _TOKEN to the bridge.
// Tokens are read when used, so they never sit in the bridge's config.
func configureHosts(b *gitbridge.Bridge) error {
	for _, p := range hostEnv {
		var cfg gitbridge.HostConfig
		for _, f := range []struct {
			name string
			into *string
		}{{"URL", &cfg.URL}, {"API_URL", &cfg.APIURL}} {
			v := strings.TrimRight(strings.TrimSpace(os.Getenv(hostVar(p, f.name))), "/")
			if v == "" {
				continue
			}
			if u, err := url.Parse(v); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return fmt.Errorf("%s %q is not an http(s) URL", hostVar(p, f.name), v)
			}
			*f.into = v
		}
		env := hostVar(p, "TOKEN")
		cfg.Token = func() string { return os.Getenv(env) }
		b.SetHost(p, cfg)
	}
	return nil
}

// hostSettings reads the server-wide webhook secrets.
func hostSettings() service.HostSettings {
	hs := service.HostSettings{WebhookSecrets: map[store.Provider]string{}}
	for _, p := range hostEnv {
		if v := strings.TrimSpace(os.Getenv(hostVar(p, "WEBHOOK_SECRET"))); v != "" {
			hs.WebhookSecrets[p] = v
		}
	}
	return hs
}
