package main

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

func TestGitHubSettingsFromEnv(t *testing.T) {
	for _, k := range []string{"VX_PUBLIC_URL", "VX_SECRET_KEY", "VX_SIGNUP", "VX_GITHUB_URL", "VX_GITHUB_API_URL"} {
		t.Setenv(k, "")
	}
	gs, err := githubSettings()
	if err != nil || gs.Signup != service.SignupInvite || gs.SecretKey != nil || gs.PublicURL != "" {
		t.Fatalf("defaults: %+v %v", gs, err)
	}
	t.Setenv("VX_PUBLIC_URL", "https://vx.example.com/")
	t.Setenv("VX_SECRET_KEY", strings.Repeat("ab", 32))
	t.Setenv("VX_SIGNUP", "Open")
	t.Setenv("VX_GITHUB_URL", "https://ghe.example.com")
	t.Setenv("VX_GITHUB_API_URL", "https://ghe.example.com/api/v3/")
	gs, err = githubSettings()
	if err != nil || gs.PublicURL != "https://vx.example.com" || len(gs.SecretKey) != 32 || gs.Signup != service.SignupOpen ||
		gs.WebURL != "https://ghe.example.com" || gs.APIURL != "https://ghe.example.com/api/v3" {
		t.Fatalf("set: %+v %v", gs, err)
	}
	for k, v := range map[string]string{
		"VX_PUBLIC_URL":     "vx.example.com",
		"VX_SECRET_KEY":     "short",
		"VX_SIGNUP":         "sometimes",
		"VX_GITHUB_API_URL": "",
	} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			_, err := githubSettings()
			if err == nil {
				t.Fatalf("%s=%q accepted", k, v)
			}
			if k == "VX_SECRET_KEY" && strings.Contains(err.Error(), v) {
				t.Fatalf("error repeats the key: %v", err)
			}
		})
	}
}
