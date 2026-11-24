package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/store"
)

// TestOrgsCLI: alice creates organization acme and repository acme/api;
// bob cannot see it; read lets him see it but not start an effort; write
// through a team lets him; agents cannot manage anything.
func TestOrgsCLI(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	ctx := context.Background()
	bobP, err := h.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	bobToken, _ := h.st.CreateToken(ctx, bobP.ID, "test")
	bob := h.user("bob", bobToken, work)

	if out := alice.must("org", "create", "acme", "--name", "Acme Inc"); !strings.Contains(out, "you are its owner") {
		t.Errorf("org create = %q", out)
	}
	if out := alice.must("repo", "create", "acme/api"); !strings.Contains(out, "Created repo acme/api") {
		t.Errorf("repo create = %q", out)
	}
	var created struct {
		Repo struct{ Name string }
	}
	if err := json.Unmarshal([]byte(alice.must("repo", "create", "tools", "--org", "acme", "--json")), &created); err != nil || created.Repo.Name != "tools" {
		t.Fatalf("repo create --org: %v %+v", err, created)
	}
	if out, code := bob.run("repo", "create", "x", "--org", "acme"); code == 0 || !strings.Contains(out, "not found") {
		t.Errorf("bob creating in acme = %d %q", code, out)
	}

	// Bob is not a member: acme/api does not exist for him.
	if out := bob.must("repo", "list"); strings.Contains(out, "acme/api") {
		t.Errorf("bob lists %q", out)
	}
	if out, code := bob.run("effort", "new", "Sneak", "--repo", "acme/api"); code == 0 || !strings.Contains(out, "repo acme/api: not found") {
		t.Errorf("bob effort new = %d %q", code, out)
	}
	if out, code := bob.run("org", "show", "acme"); code == 0 || !strings.Contains(out, "not found") {
		t.Errorf("bob org show = %d %q", code, out)
	}

	// Read: he sees it, cannot start an effort, and is told who can grant.
	if out := alice.must("repo", "grant", "acme/api", "bob", "--role", "read"); !strings.Contains(out, "Granted user:bob read on acme/api") {
		t.Errorf("grant = %q", out)
	}
	if out := bob.must("repo", "list"); !strings.Contains(out, "acme/api") {
		t.Errorf("bob with read lists %q", out)
	}
	out, code := bob.run("effort", "new", "Rate limiting", "--repo", "acme/api")
	if code == 0 || !strings.Contains(out, "needs write") || !strings.Contains(out, "vx repo grant acme/api user:bob --role write") {
		t.Errorf("bob effort new with read = %d %q", code, out)
	}

	// Write through a team.
	alice.must("org", "member", "acme", "bob", "--role", "member")
	alice.must("org", "set-default", "acme", "none")
	alice.must("team", "create", "acme/backend")
	if out := alice.must("team", "add", "acme/backend", "bob"); !strings.Contains(out, "user:bob") {
		t.Errorf("team add = %q", out)
	}
	alice.must("repo", "grant", "acme/api", "team:acme/backend", "--role", "write")
	alice.must("repo", "revoke", "acme/api", "user:bob")
	bob.must("effort", "new", "Rate limiting", "--repo", "acme/api")

	// Who has access and why.
	var access struct {
		Grants []struct{ Subject, Role string }
		People []struct {
			Person struct{ Ref string }
			Role   string
			Via    []string
		}
	}
	if err := json.Unmarshal([]byte(alice.must("repo", "access", "acme/api", "--json")), &access); err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, p := range access.People {
		roles[p.Person.Ref] = p.Role + " " + strings.Join(p.Via, ",")
	}
	if len(access.Grants) != 1 || access.Grants[0].Subject != "team:acme/backend" ||
		roles["user:alice"] != "admin org owner" || roles["user:bob"] != "write team:acme/backend" {
		t.Errorf("access = %+v %v", access.Grants, roles)
	}
	if out, code := bob.run("repo", "access", "acme/api"); code == 0 || !strings.Contains(out, "needs admin") {
		t.Errorf("bob repo access = %d %q", code, out)
	}

	// Org show and list.
	show := alice.must("org", "show", "acme")
	for _, want := range []string{"Acme Inc", "Default role for members: none", "user:bob", "teams: backend", "acme/api", "tools"} {
		if !strings.Contains(show, want) {
			t.Errorf("org show lacks %q:\n%s", want, show)
		}
	}
	if out := bob.must("org", "list"); !strings.Contains(out, "acme") || !strings.Contains(out, "personal") {
		t.Errorf("bob org list = %q", out)
	}

	// Agents act with their person's role and cannot manage.
	var agent struct{ Token string }
	if err := json.Unmarshal([]byte(alice.must("agent", "add", "helper", "--json")), &agent); err != nil {
		t.Fatal(err)
	}
	helper := h.user("helper", agent.Token, work)
	for _, args := range [][]string{
		{"org", "create", "agentorg"},
		{"org", "member", "acme", "bob", "--role", "admin"},
		{"team", "create", "acme/agents"},
		{"repo", "grant", "acme/api", "bob", "--role", "admin"},
	} {
		if out, code := helper.run(args...); code == 0 || !strings.Contains(out, "only people can") {
			t.Errorf("agent vx %s = %d %q", strings.Join(args, " "), code, out)
		}
	}
	helper.must("effort", "new", "Agent work", "--repo", "acme/api")

	// The last owner stays; removing bob takes away his team access.
	if out, code := alice.run("org", "remove", "acme", "alice"); code == 0 || !strings.Contains(out, "last owner") {
		t.Errorf("remove last owner = %d %q", code, out)
	}
	alice.must("org", "remove", "acme", "bob")
	if out := bob.must("repo", "list"); strings.Contains(out, "acme/api") {
		t.Errorf("bob after removal lists %q", out)
	}
	if out, code := alice.run("team", "remove", "acme/backend", "alice"); code == 0 || !strings.Contains(out, "not in team") {
		t.Errorf("team remove non-member = %d %q", code, out)
	}
	alice.must("team", "delete", "acme/backend")
}
