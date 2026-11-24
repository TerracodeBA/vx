package cli_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Two agents add the same method in different files of one package. The
// text merges cleanly, so only the Go check can tell; vx sync must say so
// before anyone runs go build (both cases happened while building vx).
func TestSyncWarnsAboutDuplicateMethods(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	a := h.user("a", aliceToken, work)
	b := h.user("b", aliceToken, work)
	a.must("clone", "svc", "feature", "a")
	a.dir = filepath.Join(work, "a")
	a.must("new", "Base package")
	a.write("app/app.go", "package app\n\ntype App struct{}\n")
	a.write("app/env_test.go", "package app\n\ntype env struct{}\n")
	a.must("land")
	b.must("clone", "svc", "feature", "b")
	b.dir = filepath.Join(work, "b")

	// A adds App.printClaim in feed.go and env.person in orgs_test.go and lands.
	a.must("new", "Feed")
	a.write("app/feed.go", "package app\n\nfunc (a *App) printClaim() {}\n")
	a.write("app/orgs_test.go", "package app\n\nfunc (e *env) person() {}\n")
	a.must("land")

	// B adds the same names in other files, then syncs.
	b.must("new", "Claims")
	b.write("app/claim.go", "package app\n\nfunc (a App) printClaim() {}\n")
	b.write("app/objects_test.go", "package app\n\nfunc (e *env) person() {}\n")
	out := b.must("sync")
	for _, want := range []string{
		"Warning: duplicate method App.printClaim in app (claim.go, feed.go)",
		"Warning: duplicate method env.person in app (objects_test.go, orgs_test.go)",
		"Remove one before landing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sync output lacks %q:\n%s", want, out)
		}
	}

	// Nothing new: no repeat.
	if out := b.must("sync"); strings.Contains(out, "Warning:") {
		t.Errorf("an up-to-date sync repeats the warning:\n%s", out)
	}

	// JSON carries the warnings; land --run's sync shows them too.
	a.must("new", "More")
	a.write("app/more.go", "package app\n\nfunc (a *App) helper() {}\n")
	a.must("land")
	b.write("app/helper.go", "package app\n\nfunc (a *App) helper() {}\n")
	var res struct {
		Warnings []struct {
			Name  string   `json:"name"`
			Decl  string   `json:"decl"`
			Files []string `json:"files"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(b.must("sync", "--json")), &res); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, w := range res.Warnings {
		names = append(names, w.Decl+" "+w.Name)
	}
	if got := strings.Join(names, ","); got != "method App.helper,method App.printClaim,method env.person" {
		t.Errorf("json warnings = %s", got)
	}

	// Different receivers or plain functions with the method's name are fine.
	b.write("app/claim.go", "package app\n\ntype Other struct{}\n\nfunc (o Other) printClaim() {}\n\nfunc printClaim() {}\n")
	b.write("app/objects_test.go", "package app\n")
	b.write("app/helper.go", "package app\n")
	a.must("new", "Unrelated")
	a.write("app/z.go", "package app\n")
	a.must("land")
	if out := b.must("sync"); strings.Contains(out, "Warning:") {
		t.Errorf("no duplicates left, but sync warns:\n%s", out)
	}
}
