package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/store"
)

// TestRepoSupportCLI: a superadmin cannot see a customer's repository
// through the CLI until its admin gives support access with vx repo
// support, which shows on vx repo access and ends with --revoke.
func TestRepoSupportCLI(t *testing.T) {
	s := newAuthzServer(t)
	work := t.TempDir()
	alice := s.user("alice", s.tokens["alice"], work)
	alice.must("repo", "create", "acme/shop")
	alice.must("effort", "new", "Checkout", "--repo", "acme/shop")
	ctx := context.Background()
	root, _ := s.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "root", SiteAdmin: true})
	rootTok, _ := s.st.CreateToken(ctx, root.ID, "test")
	op := s.user("root", rootTok, work)

	if out, code := op.run("effort", "list", "--repo", "acme/shop"); code == 0 || !strings.Contains(out, "not found") {
		t.Fatalf("superadmin lists efforts = %d %q", code, out)
	}
	if out := alice.must("repo", "support", "acme/shop"); !strings.Contains(out, "No support access on acme/shop") {
		t.Fatalf("empty list = %q", out)
	}
	if out, code := alice.run("repo", "support", "acme/shop", "root", "--role", "admin"); code == 0 || !strings.Contains(out, "never includes admin") {
		t.Fatalf("admin support = %d %q", code, out)
	}
	out := alice.must("repo", "support", "acme/shop", "root", "--for", "72h", "--reason", "ticket 4711")
	if !strings.Contains(out, "Gave user:root read support access to acme/shop until ") || !strings.Contains(out, "vx repo support acme/shop root --revoke") {
		t.Fatalf("grant = %q", out)
	}
	if out := alice.must("repo", "support", "acme/shop"); !strings.Contains(out, "user:root") || !strings.Contains(out, "granted by user:alice: ticket 4711") {
		t.Fatalf("list = %q", out)
	}
	if out := alice.must("repo", "access", "acme/shop"); !strings.Contains(out, "operator (support access) until ") {
		t.Fatalf("access = %q", out)
	}
	if out := op.must("effort", "list", "--repo", "acme/shop"); !strings.Contains(out, "checkout") {
		t.Fatalf("superadmin with support access = %q", out)
	}
	if out := alice.must("repo", "support", "acme/shop", "root", "--revoke"); !strings.Contains(out, "Ended the support access of root to acme/shop.") {
		t.Fatalf("revoke = %q", out)
	}
	if out, code := op.run("effort", "list", "--repo", "acme/shop"); code == 0 || !strings.Contains(out, "not found") {
		t.Fatalf("after revoke = %d %q", code, out)
	}
}
