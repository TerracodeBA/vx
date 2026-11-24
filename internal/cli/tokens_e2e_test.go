package cli_test

import (
	"context"
	"encoding/json"
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
