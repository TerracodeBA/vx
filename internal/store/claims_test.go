package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

func TestClaimsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := storetest.New(t)
	bob, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	agent, _ := s.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "bob/backend", ParentID: &bob.ID})
	repo, err := s.CreateRepo(ctx, store.Repo{Name: "shop-api"})
	if err != nil {
		t.Fatal(err)
	}
	base := hash.Of(hash.KindTree, []byte("base"))
	eff, err := s.CreateEffort(ctx, store.Effort{RepoID: repo.ID, Name: "Rate limiting", BaseBranch: "main", Base: base, OwnerID: bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(ctx, store.Workspace{EffortID: eff.ID, OwnerID: agent.ID, Intent: "limit", Base: base})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	a, err := s.InsertClaim(ctx, store.Claim{EffortID: eff.ID, OwnerID: agent.ID, WorkspaceID: &ws.ID, Targets: []string{"internal/http/"},
		Reason: "limiter", CreatedAt: t0, ExpiresAt: t0.Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if a.RepoID != repo.ID || a.RepoName != "shop-api" || a.EffortSlug != eff.Slug || a.WorkspaceID == nil || *a.WorkspaceID != ws.ID ||
		!a.CreatedAt.Equal(t0) || a.ReleasedAt != nil || a.Reason != "limiter" || !a.Active(t0) || !a.WorkspaceOpen ||
		!a.Active(t0.Add(30*time.Minute)) { // tied to an open workspace: active past its expiry
		t.Fatalf("claim = %+v", a)
	}
	b, err := s.InsertClaim(ctx, store.Claim{EffortID: eff.ID, OwnerID: bob.ID, Targets: []string{"auth.Login"}, CreatedAt: t0, ExpiresAt: t0.Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}

	// The schema wants targets, a short reason and an expiry after creation.
	for name, bad := range map[string]store.Claim{
		"no targets": {EffortID: eff.ID, OwnerID: bob.ID, Targets: []string{}, CreatedAt: t0, ExpiresAt: t0.Add(time.Minute)},
		"reason":     {EffortID: eff.ID, OwnerID: bob.ID, Targets: []string{"x"}, Reason: strings.Repeat("x", 301), CreatedAt: t0, ExpiresAt: t0.Add(time.Minute)},
		"expiry":     {EffortID: eff.ID, OwnerID: bob.ID, Targets: []string{"x"}, CreatedAt: t0, ExpiresAt: t0},
	} {
		if _, err := s.InsertClaim(ctx, bad); err == nil {
			t.Errorf("%s: insert accepted", name)
		}
	}

	active := func(at time.Time) int {
		t.Helper()
		cs, err := s.ActiveClaims(ctx, eff.ID, at)
		if err != nil {
			t.Fatal(err)
		}
		return len(cs)
	}
	if n := active(t0); n != 2 {
		t.Fatalf("active = %d", n)
	}
	if n := active(t0.Add(5 * time.Minute)); n != 1 {
		t.Fatalf("active after b expired = %d", n)
	}
	if n := active(t0.Add(5 * time.Hour)); n != 1 {
		t.Fatalf("the open workspace's claim expired: active = %d", n)
	}

	got, err := s.ClaimByPrefix(ctx, strings.ToUpper(b.ID.String()[:8]))
	if err != nil || got.ID != b.ID {
		t.Fatalf("prefix = %+v %v", got, err)
	}
	if _, err := s.ClaimByPrefix(ctx, "zz"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bad prefix err = %v", err)
	}
	if _, err := s.ClaimByPrefix(ctx, ""); !errors.Is(err, store.ErrAmbiguous) {
		t.Fatalf("empty prefix err = %v", err)
	}

	// An expired claim cannot be released; an active one once.
	if _, ok, err := s.ReleaseClaim(ctx, b.ID, t0.Add(10*time.Minute)); err != nil || ok {
		t.Fatalf("release expired = %v %v", ok, err)
	}
	at := t0.Add(time.Minute)
	if c, ok, err := s.ReleaseClaim(ctx, b.ID, at); err != nil || !ok || c.ReleasedAt == nil || !c.ReleasedAt.Equal(at) {
		t.Fatalf("release = %+v %v %v", c, ok, err)
	}
	if _, ok, _ := s.ReleaseClaim(ctx, b.ID, at); ok {
		t.Fatal("released twice")
	}

	// Releasing by workspace honours the owner filter.
	if cs, err := s.ReleaseWorkspaceClaims(ctx, ws.ID, &bob.ID, at); err != nil || len(cs) != 0 {
		t.Fatalf("bob's workspace release = %v %v", cs, err)
	}
	cs, err := s.ReleaseWorkspaceClaims(ctx, ws.ID, nil, at)
	if err != nil || len(cs) != 1 || cs[0].ID != a.ID || cs[0].ReleasedAt == nil || cs[0].RepoName != "shop-api" {
		t.Fatalf("workspace release = %+v %v", cs, err)
	}
	if n := active(t0); n != 0 {
		t.Fatalf("active after release = %d", n)
	}
}
