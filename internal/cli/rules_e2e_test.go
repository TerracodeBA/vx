package cli_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const e2eRules = `[[unique]]
name = "migration"
glob = "db/migrations/*.sql"
key  = '^(\d+)_'

[[generated]]
glob    = "gen/**"
command = "buf generate"
`

// Two agents each regenerate gen/ and each add migration 002. vx sync
// keeps the local generated file (no markers), says to regenerate, and
// warns about the shared number; the land, vx log and --json say so too.
func TestSyncAndLandApplyRepoRules(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	a := h.user("a", aliceToken, work)
	b := h.user("b", aliceToken, work)
	a.must("clone", "svc", "feature", "a")
	a.dir = filepath.Join(work, "a")
	a.must("new", "Rules")
	a.write("vx.toml", e2eRules)
	a.write("gen/api.go", "package gen\n\nvar A = 1\n\nvar B = 1\n")
	a.write("db/migrations/001_init.sql", "init")
	a.must("land")
	b.must("clone", "svc", "feature", "b")
	b.dir = filepath.Join(work, "b")

	a.must("new", "One")
	a.write("gen/api.go", "package gen\n\nvar A = 2\n\nvar B = 1\n")
	a.write("db/migrations/002_one.sql", "one")
	a.must("land")

	b.must("new", "Two")
	b.write("gen/api.go", "package gen\n\nvar A = 1\n\nvar B = 2\n")
	b.write("db/migrations/002_two.sql", "two")
	out := b.must("sync")
	for _, want := range []string{
		"Warning: generated file gen/api.go: regenerate with `buf generate`",
		"Warning: migration 002 is used by 002_one.sql and 002_two.sql (rule migration in vx.toml)",
		"Regenerate them before landing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sync output lacks %q:\n%s", want, out)
		}
	}
	if got := b.read("gen/api.go"); got != "package gen\n\nvar A = 1\n\nvar B = 2\n" {
		t.Fatalf("gen/api.go should be the local version without markers:\n%s", got)
	}

	// JSON: both warnings, with their kinds.
	a.must("new", "Three")
	a.write("gen/api.go", "package gen\n\nvar A = 3\n\nvar B = 1\n")
	a.must("land")
	var res struct {
		Warnings []struct {
			Kind   string `json:"kind"`
			Detail string `json:"detail"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(b.must("sync", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, w := range res.Warnings {
		kinds = append(kinds, w.Kind)
	}
	if strings.Join(kinds, ",") != "generated,rule" {
		t.Fatalf("json warnings = %+v", res.Warnings)
	}

	// The land reports the clash, and it stays on the change.
	out = b.must("land")
	if !strings.Contains(out, "Warning: migration 002 is used by 002_one.sql and 002_two.sql (rule migration in vx.toml)") {
		t.Fatalf("land output:\n%s", out)
	}
	if log := b.must("log"); !strings.Contains(log, "warning: migration 002 is used by") {
		t.Fatalf("log:\n%s", log)
	}
}

// Shipping replays the effort onto a branch that regenerated the same
// file: no conflict, the effort's version, and a regenerate warning in the
// plan and the pull request body.
func TestShipPlanKeepsGeneratedFiles(t *testing.T) {
	up := newGitUpstream(t)
	up.commit("Initial", map[string]string{
		"vx.toml":       e2eRules + "\n[checks]\nrequired = [\"true\"]\n",
		"gen/api.go":    "package gen\n\nvar A = 1\n\nvar B = 1\n",
		"proto/a.proto": "a\nb\nc\n",
	})
	h, aliceToken := newHarnessWith(t, true)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "shop", "--git", up.dir)
	alice.must("effort", "new", "Effort A", "--repo", "shop")
	alice.must("clone", "shop", "effort-a", "a")
	alice.dir = filepath.Join(work, "a")
	alice.must("new", "Field A")
	alice.write("proto/a.proto", "a2\nb\nc\n")
	alice.write("gen/api.go", "package gen\n\nvar A = 2\n\nvar B = 1\n")
	alice.must("land")

	up.commit("Field C (another effort)", map[string]string{
		"proto/a.proto": "a\nb\nc2\n",
		"gen/api.go":    "package gen\n\nvar A = 1\n\nvar B = 3\n",
	})
	out := alice.must("ship", "plan")
	if strings.Contains(out, "conflicts with") || !strings.Contains(out, "Warning: generated file gen/api.go: regenerate with `buf generate`") ||
		!strings.Contains(out, "regenerate them and land the result") {
		t.Fatalf("ship plan:\n%s", out)
	}
	var plan struct {
		Plan struct {
			Body string `json:"body"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(alice.must("ship", "plan", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Plan.Body, "## Warnings\n\n- generated file gen/api.go: regenerate with `buf generate`") ||
		!strings.Contains(plan.Plan.Body, ": not verified: required check `true` (vx.toml) was not run by vx") {
		t.Fatalf("PR body:\n%s", plan.Plan.Body)
	}
}

// vx land without --run for a required check says so, and the change,
// vx log and --json keep it.
func TestLandRequiredChecksNotVerified(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	alice.must("clone", "svc", "feature", "a")
	alice.dir = filepath.Join(work, "a")
	alice.must("new", "Rules")
	alice.write("vx.toml", "[checks]\nrequired = [\"true\", \"echo ok\"]\n")
	out := alice.must("land", "--run", " echo ok ")
	if !strings.Contains(out, "Not verified: required check `true` (vx.toml) was not run by vx") || strings.Contains(out, "`echo ok` (vx.toml)") {
		t.Fatalf("land output:\n%s", out)
	}
	if log := alice.must("log"); !strings.Contains(log, "not verified: required check `true` (vx.toml) was not run by vx") {
		t.Fatalf("log:\n%s", log)
	}
	alice.must("new", "More")
	alice.write("a.txt", "a")
	var res struct {
		Changeset struct {
			Evidence struct {
				NotVerified []string `json:"not_verified"`
			} `json:"evidence"`
		} `json:"changeset"`
	}
	if err := json.Unmarshal([]byte(alice.must("land", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Changeset.Evidence.NotVerified, ",") != "true,echo ok" {
		t.Fatalf("json: %+v", res)
	}
	alice.must("new", "Verified")
	alice.write("b.txt", "b")
	if out := alice.must("land", "--run", "true", "--run", "echo ok"); strings.Contains(out, "Not verified") {
		t.Fatalf("all required checks ran:\n%s", out)
	}
}
