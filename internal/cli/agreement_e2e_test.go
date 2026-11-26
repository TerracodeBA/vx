package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

// TestOrgAgreementCLI: vx org agreement shows the status and saves the
// text; work is refused with the owners named until an owner accepts in
// the web UI (there is no CLI to accept).
func TestOrgAgreementCLI(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("org", "create", "acme")
	if out := alice.must("org", "agreement", "acme"); !strings.Contains(out, "does not ask") {
		t.Errorf("off: %q", out)
	}

	dir := t.TempDir()
	const text = "# Test agreement, version 1\n\nFake fixture text.\n"
	if err := os.WriteFile(filepath.Join(dir, "v1.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"versions":[{"id":"v1","file":"v1.md","published":"2026-01-01","effective":"2026-01-01"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	vs, err := service.LoadAgreementVersions(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.Agreement = service.AgreementSettings{Required: true, Versions: vs}

	out, code := alice.run("repo", "create", "acme/api")
	if code == 0 || !strings.Contains(out, "An owner of acme must accept the data processing agreement: Organization > Agreement") || !strings.Contains(out, "Owners: alice") {
		t.Errorf("repo create without an agreement = %d %q", code, out)
	}
	out = alice.must("org", "agreement", "acme")
	for _, want := range []string{"In effect: version v1", "Not accepted", "As an owner, you accept version v1 in the web UI, signed in as a person: " + h.server + "/ui/orgs/acme#agreement"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if out := alice.must("org", "agreement", "acme", "--text"); out != text {
		t.Errorf("--text = %q", out)
	}
	alice.must("org", "agreement", "acme", "--download")
	if got := alice.read("dpa-acme-v1.md"); got != text {
		t.Errorf("downloaded %q", got)
	}
	ctx := context.Background()
	p, err := h.st.PrincipalByHandle(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := h.svc.ResolveActor(ctx, p)
	if _, err := h.svc.AcceptAgreement(ctx, owner, "acme", "v1", vs[0].SHA256); err != nil {
		t.Fatal(err)
	}
	var st struct {
		Usable  bool
		History []struct{ Id, Version string }
	}
	if err := json.Unmarshal([]byte(alice.must("org", "agreement", "acme", "--json")), &st); err != nil || !st.Usable || len(st.History) != 1 || st.History[0].Version != "v1" {
		t.Fatalf("status after accepting = %+v, %v", st, err)
	}
	alice.must("org", "agreement", "acme", "--download", "--acceptance", st.History[0].Id, "-o", "accepted.md")
	if got := alice.read("accepted.md"); got != text {
		t.Errorf("accepted text %q", got)
	}
	alice.must("repo", "create", "acme/api")
}
