package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/service"
)

func (b *boardEnv) claim(actor service.Actor, ws string, reason string, targets ...string) (service.Claim, []service.Claim) {
	b.t.Helper()
	c, others, err := b.svc.MakeClaim(b.ctx, actor, service.ClaimInput{Repo: "shop-api", Effort: b.effort.Slug, Targets: targets, Reason: reason, WorkspaceID: ws})
	if err != nil {
		b.t.Fatal(err)
	}
	return c, others
}

func (b *boardEnv) listClaims(actor service.Actor, targets ...string) []service.Claim {
	b.t.Helper()
	cs, err := b.svc.ListClaims(b.ctx, actor, "shop-api", b.effort.Slug, targets)
	if err != nil {
		b.t.Fatal(err)
	}
	return cs
}

func claimOwners(cs []service.Claim) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Owner.Handle)
	}
	return strings.Join(out, ",")
}

func TestClaimMakeValidatesAndOverlaps(t *testing.T) {
	b := newBoardEnv(t)
	c, others := b.claim(b.agent, "", "  moving\x00 the\nlimiter‮ ", "internal/http/", " auth.Login ", "internal/http/")
	if c.Reason != "moving the limiter" || strings.Join(c.Targets, ",") != "internal/http/,auth.Login" || len(others) != 0 {
		t.Fatalf("claim = %+v, others = %v", c, others)
	}
	if !c.CreatedAt.Equal(b.now) || !c.ExpiresAt.Equal(b.now.Add(service.DefaultClaimTTL)) || c.ReleasedAt != nil ||
		c.WorkspaceID != nil || c.Owner.ID != b.agent.ID || c.OwnerParent == nil || c.OwnerParent.ID != b.alice.ID ||
		c.RepoName != "shop-api" || c.EffortSlug != b.effort.Slug {
		t.Fatalf("claim = %+v", c)
	}
	if got := service.ClaimTrust(b.alice, c); got != service.TrustSamePerson {
		t.Fatalf("trust for alice = %s", got)
	}
	if got := service.ClaimTrust(b.bob, c); got != service.TrustTeammate {
		t.Fatalf("trust for bob = %s", got)
	}

	// Bob's agent claims a file inside the claimed directory: it gets the
	// claim anyway and sees alice's agent's claim. A symbol claim that does
	// not match stays out.
	b.claim(b.tests, "", "", "auth.Logout")
	mine, others := b.claim(b.bobBE, "", "", "internal/http/limit.go")
	if mine.ID == c.ID || claimOwners(others) != "alice/claude" {
		t.Fatalf("others = %v", claimOwners(others))
	}
	// The caller's own claims never count as overlapping, other principals of
	// the same person do.
	_, others = b.claim(b.agent, "", "", "internal/http/limit.go", "auth")
	if claimOwners(others) != "alice/tests,bob/backend" {
		t.Fatalf("own overlap = %v", claimOwners(others))
	}

	in := func(ttl time.Duration, reason string, targets ...string) error {
		_, _, err := b.svc.MakeClaim(b.ctx, b.agent, service.ClaimInput{Repo: "shop-api", Effort: b.effort.Slug, Targets: targets, Reason: reason, TTL: ttl})
		return err
	}
	wantCode(t, in(0, ""), service.CodeInvalid, "say what you are claiming")
	wantCode(t, in(0, "", "a b"), service.CodeInvalid, "whitespace")
	wantCode(t, in(0, strings.Repeat("x", service.MaxClaimReason+1), "x.go"), service.CodeInvalid, "keep it to 300")
	wantCode(t, in(-time.Minute, "", "x.go"), service.CodeInvalid, "negative")
	wantCode(t, in(service.MaxClaimTTL+time.Second, "", "x.go"), service.CodeInvalid, "at most 4h0m0s")
	if err := in(service.MaxClaimTTL, strings.Repeat("é", service.MaxClaimReason), "x.go"); err != nil {
		t.Fatal(err)
	}
	_, _, err := b.svc.MakeClaim(b.ctx, b.agent, service.ClaimInput{Repo: "shop-api", Effort: "nope", Targets: []string{"x.go"}})
	wantCode(t, err, service.CodeNotFound, "no effort nope")

	// Ops are logged.
	ops, err := b.svc.Store.ListOps(b.ctx, b.repo.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range ops {
		if o.Op == "claim.make" {
			n++
		}
	}
	if n != 5 {
		t.Fatalf("claim.make ops = %d", n)
	}
}

func TestClaimWorkspaceRules(t *testing.T) {
	b := newBoardEnv(t)
	ws, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", b.effort.Slug, "Limiter")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := b.claim(b.agent, ws.Workspace.ID.String(), "", "auth.go")
	if c.WorkspaceID == nil || *c.WorkspaceID != ws.Workspace.ID {
		t.Fatalf("workspace = %v", c.WorkspaceID)
	}
	in := func(actor service.Actor, wsID string) error {
		_, _, err := b.svc.MakeClaim(b.ctx, actor, service.ClaimInput{Repo: "shop-api", Effort: b.effort.Slug, Targets: []string{"x.go"}, WorkspaceID: wsID})
		return err
	}
	wantCode(t, in(b.bobBE, ws.Workspace.ID.String()), service.CodePermission, "belongs to another principal")
	wantCode(t, in(b.agent, "abc"), service.CodeInvalid, "not a full id")
	wantCode(t, in(b.agent, uuid.NewString()), service.CodeNotFound, "no workspace")

	other, err := b.svc.CreateEffort(b.ctx, b.alice, service.CreateEffortInput{Repo: "shop-api", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	ows, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", other.Slug, "Elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, in(b.agent, ows.Workspace.ID.String()), service.CodeInvalid, "in another effort")

	// Abandoning the workspace releases its claims.
	if _, err := b.svc.Abandon(b.ctx, b.agent, ws.Workspace.ID); err != nil {
		t.Fatal(err)
	}
	if cs := b.listClaims(b.bob); len(cs) != 0 {
		t.Fatalf("claims after abandon = %v", claimOwners(cs))
	}
	got, err := b.svc.Store.ClaimByID(b.ctx, c.ID)
	if err != nil || got.ReleasedAt == nil || !got.ReleasedAt.Equal(b.now) {
		t.Fatalf("released = %+v %v", got, err)
	}
	wantCode(t, in(b.agent, ws.Workspace.ID.String()), service.CodeFailedPrecondition, "already abandoned")
}

func TestClaimExpiryAndList(t *testing.T) {
	b := newBoardEnv(t)
	short, _, err := b.svc.MakeClaim(b.ctx, b.agent, service.ClaimInput{Repo: "shop-api", Effort: b.effort.Slug, Targets: []string{"internal/http/"}, TTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	b.now = b.now.Add(time.Minute)
	b.claim(b.bobBE, "", "", "auth.Login")
	if got := claimOwners(b.listClaims(b.bob)); got != "alice/claude,bob/backend" {
		t.Fatalf("all claims = %s", got)
	}
	if got := claimOwners(b.listClaims(b.bob, "internal/http/limit.go")); got != "alice/claude" {
		t.Fatalf("matching claims = %s", got)
	}
	ctx, err := b.svc.Context(b.ctx, b.bob, "shop-api", b.effort.Slug, []string{"auth"})
	if err != nil || claimOwners(ctx.Claims) != "bob/backend" {
		t.Fatalf("context claims = %v %v", claimOwners(ctx.Claims), err)
	}
	ctx, err = b.svc.Context(b.ctx, b.bob, "shop-api", b.effort.Slug, nil)
	if err != nil || claimOwners(ctx.Claims) != "alice/claude,bob/backend" {
		t.Fatalf("context claims, no targets = %v %v", claimOwners(ctx.Claims), err)
	}

	// Five minutes after it was made the short claim has expired: exactly at
	// its expiry it no longer counts.
	b.now = short.ExpiresAt
	if got := claimOwners(b.listClaims(b.bob)); got != "bob/backend" {
		t.Fatalf("claims after expiry = %s", got)
	}
	_, err = b.svc.ReleaseClaims(b.ctx, b.agent, short.ID.String(), "")
	wantCode(t, err, service.CodeFailedPrecondition, "already expired")
}

func TestClaimRelease(t *testing.T) {
	b := newBoardEnv(t)
	c, _ := b.claim(b.agent, "", "", "auth.go")
	_, err := b.svc.ReleaseClaims(b.ctx, b.bobBE, c.ID.String(), "")
	wantCode(t, err, service.CodePermission, "only its owner releases it")
	_, err = b.svc.ReleaseClaims(b.ctx, b.agent, c.ID.String()[:7], "")
	wantCode(t, err, service.CodeInvalid, "too short")
	_, err = b.svc.ReleaseClaims(b.ctx, b.agent, "", "")
	wantCode(t, err, service.CodeInvalid, "say which claim")
	_, err = b.svc.ReleaseClaims(b.ctx, b.agent, "0000000000", "")
	wantCode(t, err, service.CodeNotFound, "no claim has an id starting with")

	b.now = b.now.Add(time.Minute)
	got, err := b.svc.ReleaseClaims(b.ctx, b.agent, c.ID.String()[:8], "")
	if err != nil || len(got) != 1 || got[0].ID != c.ID || got[0].ReleasedAt == nil || !got[0].ReleasedAt.Equal(b.now) {
		t.Fatalf("released = %+v %v", got, err)
	}
	_, err = b.svc.ReleaseClaims(b.ctx, b.agent, c.ID.String(), "")
	wantCode(t, err, service.CodeFailedPrecondition, "already released")

	// Releasing by workspace takes only the caller's claims for it.
	ws, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", b.effort.Slug, "Limiter")
	if err != nil {
		t.Fatal(err)
	}
	b.claim(b.agent, ws.Workspace.ID.String(), "", "a.go")
	b.claim(b.agent, ws.Workspace.ID.String(), "", "b.go")
	b.claim(b.agent, "", "", "c.go")
	got, err = b.svc.ReleaseClaims(b.ctx, b.bobBE, "", ws.Workspace.ID.String())
	if err != nil || len(got) != 0 {
		t.Fatalf("bob released %v %v", got, err)
	}
	got, err = b.svc.ReleaseClaims(b.ctx, b.agent, "", ws.Workspace.ID.String())
	if err != nil || len(got) != 2 {
		t.Fatalf("released %v %v", got, err)
	}
	if cs := b.listClaims(b.agent); len(cs) != 1 || cs[0].Targets[0] != "c.go" {
		t.Fatalf("left = %+v", cs)
	}
	ops, err := b.svc.Store.ListOps(b.ctx, b.repo.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range ops {
		if o.Op == "claim.release" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("claim.release ops = %d", n)
	}
}

func TestClaimLandOverlapAndAutoRelease(t *testing.T) {
	b := newBoardEnv(t)
	bobClaim, _ := b.claim(b.bobBE, "", "reworking the limiter", "internal/http/")
	b.claim(b.bobBE, "", "", "docs/")
	b.claim(b.tests, "", "", "auth.go")

	ws, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", b.effort.Slug, "Limiter")
	if err != nil {
		t.Fatal(err)
	}
	own, _ := b.claim(b.agent, ws.Workspace.ID.String(), "", "internal/http/limit.go")
	b.claim(b.agent, "", "", "internal/http/")
	tree := b.snapshot(b.repo, ws.Workspace.Base, map[string]*string{"internal/http/limit.go": s("package http\n"), "auth.go": s("package auth\n")})
	res, err := b.svc.Land(b.ctx, b.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
	if err != nil || !res.Landed {
		t.Fatalf("land: %+v %v", res, err)
	}
	// Bob's agent's claim on the directory and alice's other agent's claim
	// on auth.go are reported; the caller's own claims and docs/ are not.
	if got := claimOwners(res.OverlappingClaims); got != "bob/backend,alice/tests" {
		t.Fatalf("overlapping = %s", got)
	}
	if res.OverlappingClaims[0].ID != bobClaim.ID || res.OverlappingClaims[0].Reason != "reworking the limiter" {
		t.Fatalf("overlap = %+v", res.OverlappingClaims[0])
	}
	// The landed workspace's claim is released, the untied one stays.
	got, err := b.svc.Store.ClaimByID(b.ctx, own.ID)
	if err != nil || got.ReleasedAt == nil {
		t.Fatalf("own claim after land = %+v %v", got, err)
	}
	left := 0
	for _, c := range b.listClaims(b.agent) {
		if c.Owner.ID == b.agent.ID {
			left++
		}
	}
	if left != 1 {
		t.Fatalf("own claims left = %d", left)
	}

	// A land that touches nothing claimed reports nothing.
	ws2, err := b.svc.OpenWorkspace(b.ctx, b.bobBE, "shop-api", b.effort.Slug, "Readme")
	if err != nil {
		t.Fatal(err)
	}
	tree = b.snapshot(b.repo, ws2.Workspace.Base, map[string]*string{"README.md": s("hi\n")})
	res, err = b.svc.Land(b.ctx, b.bobBE, service.LandInput{WorkspaceID: ws2.Workspace.ID, Tree: tree})
	if err != nil || !res.Landed || len(res.OverlappingClaims) != 0 {
		t.Fatalf("land readme: %+v %v", res.OverlappingClaims, err)
	}
}
