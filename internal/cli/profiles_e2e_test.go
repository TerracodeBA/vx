package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/cli"
	"github.com/TerracodeBA/vx/internal/store"
)

// profileRun runs vx with only the given environment.
func profileRun(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	app := &cli.App{Out: &out, Err: &errb, Getenv: func(k string) string { return env[k] }, Dir: env["HOME"]}
	code := app.Run(context.Background(), args)
	return code, out.String(), errb.String()
}

func hostPort(t *testing.T, server string) string {
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// TestProfiles: an old single-server config becomes a profile on first
// use; vx login adds profiles named after the host (or --profile); vx use,
// --profile and VX_PROFILE pick one; VX_SERVER and VX_TOKEN keep priority;
// vx profiles lists them and remove forgets one.
func TestProfiles(t *testing.T) {
	h1, tok1 := newHarness(t)
	h2, _ := newHarness(t)
	ctx := context.Background()
	bob, err := h2.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob", Email: "bob@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := h2.st.CreateToken(ctx, bob.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cfg := filepath.Join(home, "config.json")
	env := map[string]string{"VX_CONFIG": cfg, "HOME": home}
	run := func(args ...string) (int, string, string) { return profileRun(t, env, args...) }
	must := func(args ...string) string {
		t.Helper()
		code, out, errText := run(args...)
		if code != 0 {
			t.Fatalf("vx %s = %d %s %s", strings.Join(args, " "), code, out, errText)
		}
		return out
	}

	// A config file from before profiles keeps working, and is migrated.
	if err := os.WriteFile(cfg, []byte(`{"server":"`+h1.server+`","token":"`+tok1+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := must("whoami"); out != "user:alice\n" {
		t.Fatalf("whoami with an old config = %q", out)
	}
	host1, host2 := hostPort(t, h1.server), hostPort(t, h2.server)
	var file struct {
		Current  string                     `json:"current"`
		Server   string                     `json:"server"`
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	data, _ := os.ReadFile(cfg)
	if err := json.Unmarshal(data, &file); err != nil || file.Current != host1 || file.Server != "" || len(file.Profiles) != 1 {
		t.Fatalf("migrated config = %s (%v)", data, err)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", fi.Mode())
	}

	// A second server: a new profile named after its host, now current.
	if out := must("login", "--server", h2.server, "--token", tok2); !strings.Contains(out, "as user:bob (profile "+host2+", now current)") {
		t.Fatalf("login to the second server = %q", out)
	}
	if out := must("whoami"); out != "user:bob\n" {
		t.Fatalf("whoami after login = %q", out)
	}
	if out := must("use", host1); !strings.Contains(out, "Now using "+host1+" ("+h1.server) {
		t.Fatalf("use = %q", out)
	}
	if out := must("whoami"); out != "user:alice\n" {
		t.Fatalf("whoami after use = %q", out)
	}
	// By server URL too.
	must("use", h2.server)
	if out := must("whoami"); out != "user:bob\n" {
		t.Fatalf("whoami after use URL = %q", out)
	}
	// One command with another profile, by flag or VX_PROFILE.
	if out := must("--profile", host1, "whoami"); out != "user:alice\n" {
		t.Fatalf("--profile whoami = %q", out)
	}
	env["VX_PROFILE"] = host1
	if out := must("whoami"); out != "user:alice\n" {
		t.Fatalf("VX_PROFILE whoami = %q", out)
	}
	delete(env, "VX_PROFILE")
	// A named profile.
	if out := must("login", "--server", h1.server, "--token", tok1, "--profile", "work"); !strings.Contains(out, "(profile work, now current)") {
		t.Fatalf("login --profile = %q", out)
	}
	out := must("profiles")
	for _, want := range []string{"* work", "  " + host1, "  " + host2, "user:bob"} {
		if !strings.Contains(out, want) {
			t.Errorf("profiles lacks %q:\n%s", want, out)
		}
	}
	var listed struct {
		Current  string `json:"current"`
		Profiles []struct {
			Name, Server string
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(must("profiles", "--json")), &listed); err != nil || listed.Current != "work" || len(listed.Profiles) != 3 {
		t.Fatalf("profiles --json = %+v (%v)", listed, err)
	}
	if strings.Contains(must("profiles", "--json"), "vxt_") {
		t.Fatal("vx profiles prints tokens")
	}

	// Unknown profiles say what exists.
	if code, _, errText := run("--profile", "nope", "whoami"); code != 1 || !strings.Contains(errText, `no profile "nope" (saved: `+host1) || !strings.Contains(errText, "vx login --server URL --profile nope") {
		t.Fatalf("unknown profile = %d %q", code, errText)
	}
	if code, _, errText := run("use", "nope"); code != 1 || !strings.Contains(errText, `no profile "nope"`) {
		t.Fatalf("use unknown = %d %q", code, errText)
	}

	// Agent env files keep priority over the current profile.
	env["VX_SERVER"], env["VX_TOKEN"] = h2.server, tok2
	if out := must("whoami"); out != "user:bob\n" {
		t.Fatalf("whoami with VX_TOKEN = %q", out)
	}
	if code, _, errText := run("--profile", "work", "whoami"); code != 1 || !strings.Contains(errText, "--profile and VX_TOKEN are both set") {
		t.Fatalf("--profile with VX_TOKEN = %d %q", code, errText)
	}
	if _, _, errText := run("use", host1); !strings.Contains(errText, "takes precedence over saved profiles") {
		t.Fatalf("use with VX_TOKEN set = %q", errText)
	}
	delete(env, "VX_SERVER")
	delete(env, "VX_TOKEN")

	// Forgetting a profile.
	must("use", "work")
	if out := must("profiles", "remove", "work"); !strings.Contains(out, "Removed profile work") {
		t.Fatalf("remove = %q", out)
	}
	if code, _, errText := run("whoami"); code != 1 || !strings.Contains(errText, "not logged in") {
		t.Fatalf("whoami without a current profile = %d %q", code, errText)
	}
	must("use", host2)
	if out := must("whoami"); out != "user:bob\n" {
		t.Fatalf("whoami = %q", out)
	}
}
