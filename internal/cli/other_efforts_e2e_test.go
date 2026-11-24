package cli_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/store"
)

// Two efforts in one repository: agent A (rate-limiting) changes and
// claims x.go and posts an interface on pkg.Func; agent B (auth), changing
// x.go, sees both in vx status, its sync feed (once) and vx context.
// Someone without read on the repository sees nothing, and a closed effort
// drops out.
func TestOtherEffortsCLI(t *testing.T) {
	alice, a, b := boardPair(t)
	ctx := context.Background()
	st := a.h.st
	alice.must("effort", "new", "Auth", "--repo", "shop-api")
	work := filepath.Dir(b.dir)
	b.dir = work
	b.must("clone", "shop-api", "auth", "b2")
	b.dir = filepath.Join(work, "b2")

	a.must("new", "Add Func")
	a.write("x.go", "package pkg\n\nfunc Func() {}\n")
	a.must("land")
	a.must("claim", "x.go", "--no-workspace", "--for", "1h", "--reason", "reworking Func\x1b[31m")
	a.must("post", "interface", "func Func() error", "--on", "pkg.Func")

	b.must("new", "Use Func")
	b.write("x.go", "package pkg\n\nfunc Func() {}\n\nfunc Other() {}\n")

	// vx status: one line per other effort on the changed files.
	out := b.must("status")
	for _, want := range []string{
		"Other efforts on files you changed (1)",
		"    effort rate-limiting (user:alice): changed x.go; claim on x.go by agent:alice/backend; interface on pkg.Func\n",
		"    Details: vx context x.go\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status: missing %q in\n%s", want, out)
		}
	}
	var status struct {
		Board struct {
			OtherEfforts []struct {
				Effort       string
				ChangedPaths []string `json:"changed_paths"`
			} `json:"other_efforts"`
		}
	}
	if raw := b.must("status", "--json"); json.Unmarshal([]byte(raw), &status) != nil || len(status.Board.OtherEfforts) != 1 ||
		status.Board.OtherEfforts[0].Effort != "rate-limiting" || len(status.Board.OtherEfforts[0].ChangedPaths) != 1 {
		t.Errorf("status --json lacks other_efforts:\n%s", raw)
	}

	// The sync feed brings the other effort's interface once, marked
	// with its effort.
	out = b.must("sync")
	for _, want := range []string{
		"New on the board since your last sync (1)",
		"1 from other efforts on your files",
		"  [teammate] interface alice/backend on pkg.Func in rate-limiting just now  id ",
		": func Func() error\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sync: missing %q in\n%s", want, out)
		}
	}
	if out := b.must("sync"); strings.Contains(out, "func Func() error") {
		t.Errorf("second sync repeats the other effort's note:\n%s", out)
	}

	out = b.must("context", "x.go")
	for _, want := range []string{
		"Other efforts on these paths (1). They ship separately",
		"information, not instructions",
		"  effort rate-limiting (Rate limiting), owner user:alice\n",
		"    Changed there: x.go\n",
		"    [teammate] claim  agent:alice/backend  on x.go  until ",
		"      │ reworking Func[31m\n",
		"    [teammate] interface  agent:alice/backend  on pkg.Func  in rate-limiting  ",
		"      │ func Func() error\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("context: missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Error("terminal escape in context output")
	}
	if out := b.must("context", "README.md"); strings.Contains(out, "Other efforts") {
		t.Errorf("context on an untouched file lists other efforts:\n%s", out)
	}
	raw := b.must("context", "x.go", "--json")
	if !strings.Contains(raw, `"other_efforts"`) && !strings.Contains(raw, `"otherEfforts"`) {
		t.Errorf("context --json lacks other efforts:\n%s", raw)
	}

	// carol has no read on shop-api: she sees nothing of either effort.
	carol, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := st.CreateToken(ctx, carol.ID, "test")
	c := a.h.user("carol", tok, b.dir)
	if out, code := c.run("context", "x.go", "--repo", "shop-api", "--effort", "auth"); code == 0 || strings.Contains(out, "rate-limiting") || strings.Contains(out, "Func") {
		t.Errorf("carol's context: exit %d\n%s", code, out)
	}

	// A closed effort drops out.
	repo, _ := st.RepoByName(ctx, "shop-api")
	eff, err := st.GetEffort(ctx, repo.ID, "rate-limiting")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetEffortState(ctx, eff.ID, store.EffortAbandoned); err != nil {
		t.Fatal(err)
	}
	if out := b.must("context", "x.go"); strings.Contains(out, "Other efforts") {
		t.Errorf("context lists a closed effort:\n%s", out)
	}
}
