package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

func TestListAndRevokeTokens(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	alice, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	agent, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/claude", ParentID: &alice.ID})
	laptop, err := s.CreateToken(ctx, alice.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, alice.ID, "ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, agent.ID, "agent"); err != nil {
		t.Fatal(err)
	}

	p, laptopID, err := s.AuthenticateToken(ctx, laptop)
	if err != nil || p.ID != alice.ID || laptopID == uuid.Nil {
		t.Fatalf("AuthenticateToken = %v %v %v", p.Handle, laptopID, err)
	}
	tokens, err := s.ListTokens(ctx, alice.ID)
	if err != nil || len(tokens) != 2 {
		t.Fatalf("ListTokens(alice) = %d, %v", len(tokens), err)
	}
	var used store.Token
	for _, tk := range tokens {
		if tk.PrincipalID != alice.ID || !tk.Active() || tk.CreatedAt.IsZero() {
			t.Errorf("token %+v", tk)
		}
		if tk.ID == laptopID {
			used = tk
		} else if tk.LastUsedAt != nil {
			t.Errorf("unused token %s has last_used_at", tk.Name)
		}
	}
	if used.Name != "laptop" || used.LastUsedAt == nil {
		t.Fatalf("used token = %+v", used)
	}
	if both, _ := s.ListTokens(ctx, alice.ID, agent.ID); len(both) != 3 {
		t.Fatalf("ListTokens(alice, agent) = %d", len(both))
	}

	tok, revoked, err := s.RevokeToken(ctx, laptopID)
	if err != nil || !revoked || tok.RevokedAt == nil || tok.Active() {
		t.Fatalf("RevokeToken = %+v %v %v", tok, revoked, err)
	}
	again, revoked, err := s.RevokeToken(ctx, laptopID)
	if err != nil || revoked || again.RevokedAt == nil || !again.RevokedAt.Equal(*tok.RevokedAt) {
		t.Fatalf("second RevokeToken = %+v %v %v", again, revoked, err)
	}
	if _, err := s.Authenticate(ctx, laptop); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked token still works: %v", err)
	}
	if _, _, err := s.RevokeToken(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoking a missing token = %v", err)
	}
	// Active tokens list before revoked ones.
	tokens, _ = s.ListTokens(ctx, alice.ID)
	if len(tokens) != 2 || !tokens[0].Active() || tokens[1].ID != laptopID {
		t.Fatalf("order after revoke = %+v", tokens)
	}
}
