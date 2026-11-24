package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/cli"
	"github.com/TerracodeBA/vx/internal/store"
)

func TestAgentAndTokenCommands(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)

	if out := alice.must("agent", "list"); !strings.Contains(out, "No agents yet") {
		t.Fatalf("empty agent list = %q", out)
	}
	var created struct{ Token string }
	if err := json.Unmarshal([]byte(alice.must("agent", "add", "backend", "--json")), &created); err != nil || created.Token == "" {
		t.Fatalf("agent add: %v", err)
	}
	backend := h.user("backend", created.Token, work)
	backend.must("whoami") // uses the token once

	out := alice.must("agent", "list")
	if !strings.Contains(out, "alice/backend") || !strings.Contains(out, "1 active") || strings.Contains(out, "never") {
		t.Fatalf("agent list = %q", out)
	}
	var agents struct {
		Agents []struct {
			Agent        struct{ Handle string }
			ActiveTokens int    `json:"active_tokens"`
			LastUsedAt   string `json:"last_used_at"`
		}
	}
	if err := json.Unmarshal([]byte(alice.must("agent", "list", "--json")), &agents); err != nil || len(agents.Agents) != 1 ||
		agents.Agents[0].Agent.Handle != "alice/backend" || agents.Agents[0].ActiveTokens != 1 || agents.Agents[0].LastUsedAt == "" {
		t.Fatalf("agent list --json = %+v, %v", agents, err)
	}
	if out, code := backend.run("agent", "list"); code == 0 || !strings.Contains(out, "only people have agents") {
		t.Fatalf("agent listing agents = %d %q", code, out)
	}

	// Token lists: her own marks the token in use; the agent's by name.
	if out := alice.must("token", "list"); !strings.Contains(out, "Tokens of user:alice") || !strings.Contains(out, "(in use now)") {
		t.Fatalf("token list = %q", out)
	}
	type tokens struct {
		Principal struct{ Ref string }
		Tokens    []struct {
			ID        string `json:"id"`
			Name      string
			RevokedAt string `json:"revoked_at"`
			Current   bool
		}
	}
	var bt tokens
	if err := json.Unmarshal([]byte(alice.must("token", "list", "--agent", "backend", "--json")), &bt); err != nil ||
		bt.Principal.Ref != "agent:alice/backend" || len(bt.Tokens) != 1 || bt.Tokens[0].Current {
		t.Fatalf("token list --agent = %+v, %v", bt, err)
	}

	// Bob cannot see or revoke Alice's agent's token.
	ctx := context.Background()
	bob, _ := h.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	bobToken, _ := h.st.CreateToken(ctx, bob.ID, "test")
	bobU := h.user("bob", bobToken, work)
	if out, code := bobU.run("token", "list", "--agent", "alice/backend"); code == 0 || !strings.Contains(out, "only your own") {
		t.Fatalf("bob listing alice's agent = %d %q", code, out)
	}
	if out, code := bobU.run("token", "revoke", bt.Tokens[0].ID); code == 0 || !strings.Contains(out, "belongs to agent:alice/backend") {
		t.Fatalf("bob revoking alice's agent token = %d %q", code, out)
	}
	backend.must("whoami")

	// Alice revokes it by its short id; the agent is locked out at once.
	if out := alice.must("token", "revoke", bt.Tokens[0].ID[:8]); !strings.Contains(out, "Revoked token "+bt.Tokens[0].ID[:8]) {
		t.Fatalf("revoke = %q", out)
	}
	if out, code := backend.run("whoami"); code == 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("revoked agent still works: %d %q", code, out)
	}
	if out := alice.must("token", "revoke", bt.Tokens[0].ID); !strings.Contains(out, "already revoked") {
		t.Fatalf("second revoke = %q", out)
	}
	if out := alice.must("agent", "list"); !strings.Contains(out, "0 active, 1 revoked") {
		t.Fatalf("agent list after revoke = %q", out)
	}

	// Her own token in use needs --current, and then she is signed out.
	var own tokens
	if err := json.Unmarshal([]byte(alice.must("token", "list", "--json")), &own); err != nil || len(own.Tokens) != 1 || !own.Tokens[0].Current {
		t.Fatalf("own tokens = %+v, %v", own, err)
	}
	out, code := alice.run("token", "revoke", own.Tokens[0].ID)
	if code != cli.ExitError || !strings.Contains(out, "--current") {
		t.Fatalf("revoking the token in use = %d %q", code, out)
	}
	var revoked struct {
		Token struct {
			RevokedAt string `json:"revoked_at"`
			Current   bool
		}
	}
	if err := json.Unmarshal([]byte(alice.must("token", "revoke", own.Tokens[0].ID, "--current", "--json")), &revoked); err != nil ||
		revoked.Token.RevokedAt == "" || !revoked.Token.Current {
		t.Fatalf("revoke --current = %+v, %v", revoked, err)
	}
	if _, code := alice.run("whoami"); code == 0 {
		t.Fatal("alice's revoked token still works")
	}
}

func TestTokenCreateCommand(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("agent", "add", "backend")

	// For herself: printed once, works, shows up in the list.
	out := alice.must("token", "create", "--name", "laptop")
	i := strings.Index(out, "vxt_")
	if i < 0 || !strings.Contains(out, "for user:alice") || !strings.Contains(out, "shown once") {
		t.Fatalf("token create = %q", out)
	}
	secret := strings.Fields(out[i:])[0]
	if got := h.user("laptop", secret, work).must("whoami"); !strings.Contains(got, "user:alice") {
		t.Fatalf("new token is %q", got)
	}
	if out := alice.must("token", "list"); !strings.Contains(out, "laptop") {
		t.Fatalf("token list = %q", out)
	}

	// For her agent, into an env file: never printed, mode 600, and it works.
	env := filepath.Join(work, "agents", "backend.env")
	out = alice.must("token", "create", "--agent", "backend", "--env-file", env)
	if strings.Contains(out, "vxt_") || !strings.Contains(out, "agent:alice/backend") {
		t.Fatalf("token create --env-file = %q", out)
	}
	info, err := os.Stat(env)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode = %v, %v", info, err)
	}
	data, _ := os.ReadFile(env)
	m := regexp.MustCompile(`VX_TOKEN="(vxt_[a-z0-9]+)"`).FindSubmatch(data)
	if m == nil || !strings.Contains(string(data), h.server) {
		t.Fatalf("env file = %s", data)
	}
	backend := h.user("backend", string(m[1]), work)
	if got := backend.must("whoami"); !strings.Contains(got, "agent:alice/backend") {
		t.Fatalf("agent token is %q", got)
	}
	// An existing file is refused before any token is created.
	before := alice.must("token", "list", "--agent", "backend")
	if out, code := alice.run("token", "create", "--agent", "backend", "--env-file", env); code == 0 || !strings.Contains(out, "already exists") {
		t.Fatalf("existing env file = %d %q", code, out)
	}
	if after := alice.must("token", "list", "--agent", "backend"); after != before {
		t.Fatalf("a token was created anyway:\n%s", after)
	}
	// --json carries the token record but not the secret when writing a file.
	var res struct {
		Token   struct{ ID, Name string }
		EnvFile string `json:"env_file"`
	}
	env2 := filepath.Join(work, "agents", "ci.env")
	raw := alice.must("token", "create", "--agent", "backend", "--name", "ci", "--env-file", env2, "--json")
	if err := json.Unmarshal([]byte(raw), &res); err != nil || res.Token.Name != "ci" || res.EnvFile != env2 || strings.Contains(raw, "vxt_") {
		t.Fatalf("token create --json = %s, %v", raw, err)
	}

	// Agents cannot create tokens, for themselves or anyone else.
	if out, code := backend.run("token", "create"); code == 0 || !strings.Contains(out, "only people can create tokens") {
		t.Fatalf("agent creating a token = %d %q", code, out)
	}
	// Nor can Bob create one for Alice's agent.
	ctx := context.Background()
	bob, _ := h.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	bobToken, _ := h.st.CreateToken(ctx, bob.ID, "test")
	if out, code := h.user("bob", bobToken, work).run("token", "create", "--agent", "alice/backend"); code == 0 || !strings.Contains(out, "only for yourself and your agents") {
		t.Fatalf("bob creating alice's agent token = %d %q", code, out)
	}
}
