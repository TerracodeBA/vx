package service_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func code(err error) service.Code {
	var se *service.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

func TestAgentAndTokenPermissions(t *testing.T) {
	e := newEnv(t)
	ctx, st := e.ctx, e.svc.Store
	bob, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	bobActor, _ := e.svc.ResolveActor(ctx, bob)
	_, bobAgentToken, err := e.svc.CreateAgent(ctx, bobActor, "helper", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.CreateAgent(ctx, e.alice, "backend", "")
	if err != nil {
		t.Fatal(err)
	}
	aliceSecret, _ := st.CreateToken(ctx, e.alice.ID, "laptop")
	_, aliceTokenID, _ := st.AuthenticateToken(ctx, aliceSecret)
	_, bobAgentTokenID, _ := st.AuthenticateToken(ctx, bobAgentToken)
	alice := e.alice
	alice.TokenID = aliceTokenID

	// Alice sees her two agents with their token summaries.
	agents, err := e.svc.ListAgents(ctx, alice)
	if err != nil || len(agents) != 2 {
		t.Fatalf("ListAgents = %d, %v", len(agents), err)
	}
	for _, a := range agents {
		if !strings.HasPrefix(a.Agent.Handle, "alice/") {
			t.Errorf("alice sees %s", a.Agent.Handle)
		}
	}
	if agents[0].Agent.Handle != "alice/backend" || agents[0].ActiveTokens != 1 {
		t.Errorf("backend summary = %+v", agents[0])
	}
	if _, err := e.svc.ListAgents(ctx, e.agent); code(err) != service.CodePermission {
		t.Errorf("agent listing agents: %v", err)
	}

	// Tokens: her own, her agents' by name or handle, never Bob's.
	if p, toks, err := e.svc.ListTokens(ctx, alice, ""); err != nil || p.ID != alice.ID || len(toks) != 1 {
		t.Fatalf("own tokens = %v %d %v", p.Handle, len(toks), err)
	}
	if p, toks, err := e.svc.ListTokens(ctx, alice, "backend"); err != nil || p.Handle != "alice/backend" || len(toks) != 1 {
		t.Fatalf("agent tokens = %v %d %v", p.Handle, len(toks), err)
	}
	if _, _, err := e.svc.ListTokens(ctx, alice, "alice/backend"); err != nil {
		t.Fatalf("agent tokens by handle: %v", err)
	}
	if _, _, err := e.svc.ListTokens(ctx, alice, "bob/helper"); code(err) != service.CodePermission {
		t.Fatalf("alice listing bob's agent: %v", err)
	}
	if _, _, err := e.svc.ListTokens(ctx, alice, "nope"); code(err) != service.CodeNotFound {
		t.Fatalf("missing agent: %v", err)
	}
	// An agent sees its own tokens but not its siblings'.
	if _, _, err := e.svc.ListTokens(ctx, e.agent, ""); err != nil {
		t.Fatalf("agent's own tokens: %v", err)
	}
	if _, _, err := e.svc.ListTokens(ctx, e.agent, "backend"); code(err) != service.CodePermission {
		t.Fatalf("agent listing a sibling: %v", err)
	}

	// Alice cannot revoke Bob's agent's token, by id or by prefix.
	if _, _, _, err := e.svc.RevokeToken(ctx, alice, service.RevokeInput{ID: bobAgentTokenID.String()}); code(err) != service.CodePermission {
		t.Fatalf("revoking bob's token: %v", err)
	}
	if _, _, _, err := e.svc.RevokeToken(ctx, alice, service.RevokeInput{ID: bobAgentTokenID.String()[:8]}); code(err) != service.CodeNotFound {
		t.Fatalf("revoking bob's token by prefix: %v", err)
	}
	if _, _, _, err := e.svc.RevokeToken(ctx, alice, service.RevokeInput{ID: "abc"}); code(err) != service.CodeInvalid {
		t.Fatalf("short prefix: %v", err)
	}
	if tok, _ := st.TokenByID(ctx, bobAgentTokenID); !tok.Active() {
		t.Fatal("bob's token was revoked")
	}

	// She revokes her agent's token by prefix, once; the second time is a no-op.
	_, backendToks, _ := e.svc.ListTokens(ctx, alice, "backend")
	tok, owner, already, err := e.svc.RevokeToken(ctx, alice, service.RevokeInput{ID: backendToks[0].ID.String()[:8]})
	if err != nil || already || owner.Handle != "alice/backend" || tok.Active() {
		t.Fatalf("revoke agent token = %+v %v %v %v", tok, owner.Handle, already, err)
	}
	if _, _, already, err := e.svc.RevokeToken(ctx, alice, service.RevokeInput{ID: tok.ID.String()}); err != nil || !already {
		t.Fatalf("revoke twice = %v %v", already, err)
	}
	ops, err := st.ListActorOps(ctx, alice.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	revokes := 0
	for _, op := range ops {
		if op.Op == "token.revoke" {
			revokes++
			if op.Target != "agent:alice/backend" || !strings.Contains(string(op.After), tok.ID.String()) {
				t.Errorf("op = %+v", op)
			}
		}
	}
	if revokes != 1 {
		t.Fatalf("token.revoke ops = %d", revokes)
	}

	// The token in use needs an explicit flag.
	if _, _, _, err := e.svc.RevokeToken(ctx, alice, service.RevokeInput{ID: aliceTokenID.String()}); code(err) != service.CodeFailedPrecondition {
		t.Fatalf("revoking the current token: %v", err)
	}
	if _, _, _, err := e.svc.RevokeToken(ctx, alice, service.RevokeInput{ID: aliceTokenID.String(), AllowCurrent: true}); err != nil {
		t.Fatalf("revoking the current token with the flag: %v", err)
	}
	if _, err := st.Authenticate(ctx, aliceSecret); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}
}

func TestCreateToken(t *testing.T) {
	e := newEnv(t)
	ctx, st := e.ctx, e.svc.Store
	bob, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	if _, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "bob/helper", ParentID: &bob.ID}); err != nil {
		t.Fatal(err)
	}

	// Alice creates one for herself; the secret works and names its owner.
	tok, owner, secret, err := e.svc.CreateToken(ctx, e.alice, "", "  laptop  ")
	if err != nil || owner.ID != e.alice.ID || tok.Name != "laptop" || !tok.Active() {
		t.Fatalf("own token = %+v %v %v", tok, owner.Handle, err)
	}
	if p, id, err := st.AuthenticateToken(ctx, secret); err != nil || p.ID != e.alice.ID || id != tok.ID {
		t.Fatalf("authenticate new token = %v %v %v", p.Handle, id, err)
	}

	// And one for her agent, by name, with the default label.
	tok, owner, secret, err = e.svc.CreateToken(ctx, e.alice, "claude", "")
	if err != nil || owner.Handle != "alice/claude" || tok.Name != "created by user:alice" {
		t.Fatalf("agent token = %+v %v %v", tok, owner.Handle, err)
	}
	if p, err := st.Authenticate(ctx, secret); err != nil || p.Handle != "alice/claude" {
		t.Fatalf("agent token authenticates as %v %v", p.Handle, err)
	}
	ops, _ := st.ListActorOps(ctx, e.alice.ID, 10)
	creates := 0
	for _, op := range ops {
		if op.Op == "token.create" {
			creates++
			if strings.Contains(string(op.After), secret) {
				t.Errorf("op log holds the secret: %s", op.After)
			}
		}
	}
	if creates != 2 {
		t.Fatalf("token.create ops = %d", creates)
	}

	// Not for other people's agents, nor for people.
	if _, _, _, err := e.svc.CreateToken(ctx, e.alice, "bob/helper", ""); code(err) != service.CodePermission {
		t.Fatalf("token for bob's agent: %v", err)
	}
	if _, _, _, err := e.svc.CreateToken(ctx, e.alice, "nope", ""); code(err) != service.CodeNotFound {
		t.Fatalf("token for a missing agent: %v", err)
	}
	// Agents cannot create tokens, not even for themselves.
	if _, _, _, err := e.svc.CreateToken(ctx, e.agent, "", ""); code(err) != service.CodePermission || !strings.Contains(err.Error(), "--agent claude") {
		t.Fatalf("agent creating its own token: %v", err)
	}
	if _, _, _, err := e.svc.CreateToken(ctx, e.agent, "alice/other", ""); code(err) != service.CodePermission {
		t.Fatalf("agent creating a sibling's token: %v", err)
	}
	// Labels are one short line.
	if _, _, _, err := e.svc.CreateToken(ctx, e.alice, "", strings.Repeat("x", service.MaxTokenName+1)); code(err) != service.CodeInvalid {
		t.Fatalf("long name: %v", err)
	}
	if _, _, _, err := e.svc.CreateToken(ctx, e.alice, "", "a\nb"); code(err) != service.CodeInvalid {
		t.Fatalf("multi-line name: %v", err)
	}
}
