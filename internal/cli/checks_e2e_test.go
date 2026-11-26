package cli_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const cliCheckRules = `[[check]]
name  = "unit"
run   = "echo unit ok"
paths = ["**/*.go"]

[[check]]
name     = "signoff"
runner   = "human"
reporter = "self"
paths    = ["**/*.go"]
outputs  = { artifacts = ["*.png"] }
`

// The check commands end to end: a land refused by checks that block it,
// vx check status, vx check run, vx check report with a log and an
// artifact, the land that then goes in without --run, vx check show
// --download, and vx rules listing the checks.
func TestCheckCommands(t *testing.T) {
	_, _, clone := speedSetup(t)
	b := clone("b")
	b.must("new", "Checks")
	b.write("vx.toml", cliCheckRules)
	b.must("land")

	b.must("new", "Add pay")
	b.write("pay.go", "package pay\n")
	out, code := b.run("land")
	if code == 0 || !strings.Contains(out, "not landed: 2 checks that block land have no passing result on this change's tree") ||
		!strings.Contains(out, "unit     missing  vx land --run 'echo unit ok' (or vx check run unit)") {
		t.Fatalf("land without results: %d\n%s", code, out)
	}
	out = b.must("check", "status")
	for _, want := range []string{"Checks of your workspace's change ", "unit     missing  vx land --run 'echo unit ok'", "[blocks land]", "Still needed to land: unit, signoff."} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	out = b.must("check", "run", "unit")
	if !strings.Contains(out, "pass echo unit ok") || !strings.Contains(out, "Reported pass for unit on change ") || !strings.Contains(out, "It counts: unit is passed on this tree.") {
		t.Fatalf("check run:\n%s", out)
	}
	tmp := t.TempDir()
	shot, log := filepath.Join(tmp, "shot.png"), filepath.Join(tmp, "notes.txt")
	os.WriteFile(shot, []byte("\x89PNG\r\n\x1a\nfake"), 0o644)
	os.WriteFile(log, []byte("looked at pay.go\n"), 0o644)
	if out, code := b.run("check", "report", "signoff", "--status", "pass", "--summary", "ok", "--artifact", log); code == 0 || !strings.Contains(out, `artifact "notes.txt" does not match the outputs`) {
		t.Fatalf("an artifact the check does not allow: %d\n%s", code, out)
	}
	out = b.must("check", "report", "signoff", "--status", "pass", "--summary", "Reads well", "--artifact", shot, "--log", log)
	if !strings.Contains(out, "with 1 artifact(s)") || !strings.Contains(out, "It counts: signoff is passed on this tree.") {
		t.Fatalf("check report:\n%s", out)
	}
	id := regexp.MustCompile(`as result ([0-9a-f]{8})`).FindStringSubmatch(out)
	if id == nil {
		t.Fatalf("no result id:\n%s", out)
	}
	out = b.must("land")
	if !strings.Contains(out, "Landed ") {
		t.Fatalf("land with every result on these files:\n%s", out)
	}
	out = b.must("check", "status")
	if !strings.Contains(out, "(the effort head)") || !strings.Contains(out, "unit     passed   pass by user:alice") || !strings.Contains(out, "Nothing more needed") {
		t.Fatalf("status of the head:\n%s", out)
	}
	dl := filepath.Join(tmp, "dl")
	out = b.must("check", "show", id[1], "--download", dl)
	if !strings.Contains(out, "signoff pass") || !strings.Contains(out, "Artifact: shot.png (image/png") {
		t.Fatalf("check show:\n%s", out)
	}
	if data, err := os.ReadFile(filepath.Join(dl, "shot.png")); err != nil || !strings.HasSuffix(string(data), "fake") {
		t.Fatalf("downloaded artifact: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(dl, "log.txt")); err != nil || string(data) != "looked at pay.go\n" {
		t.Fatalf("downloaded log: %q %v", data, err)
	}
	out = b.must("rules")
	if !strings.Contains(out, "Checks (vx check status shows what a change still needs):") || !strings.Contains(out, "signoff  when land   blocks land    reporter self        runner human") {
		t.Fatalf("vx rules:\n%s", out)
	}
	// A new tree needs new evidence.
	b.must("new", "Change pay")
	b.write("pay.go", "package pay\n\nconst X = 1\n")
	if out, code := b.run("land", "--run", "echo unit ok"); code == 0 || !strings.Contains(out, "1 check that blocks land has") || !strings.Contains(out, "signoff") {
		t.Fatalf("a new tree with the old sign-off: %d\n%s", code, out)
	}
}

// A failed --run shows the failing Go tests and packages, not the end of
// the output; checks stop at the first failure and one line says what
// failed and what did not run; a test -z check without output gets a
// hint; the cheapest check runs first.
func TestCheckFailureOutput(t *testing.T) {
	_, _, clone := speedSetup(t)
	b := clone("b")
	b.must("new", "Add x")
	b.write("x.txt", "x\n")
	gotest := `sleep 0.2; printf '=== RUN   TestA\n--- FAIL: TestA (0.00s)\n    a_test.go:3: boom\n=== RUN   TestB\n--- PASS: TestB (0.00s)\nFAIL\nFAIL\tex/a\t0.01s\nok  \tex/b\t0.01s\n'; exit 1`
	out, code := b.run("land", "--run", gotest, "--run", "true")
	if code == 0 {
		t.Fatalf("land should fail:\n%s", out)
	}
	for _, want := range []string{"  --- FAIL: TestA (0.00s)\n", "      a_test.go:3: boom\n", "  FAIL\tex/a\t0.01s\n", "The failing tests and packages of 8 lines; full output: ",
		"1 of 2 checks failed: " + gotest + "; not run after it: true (--keep-going runs every check)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"  === RUN   TestB", "  --- PASS", "  ok  \tex/b", "pass true"} {
		if strings.Contains(out, not) {
			t.Errorf("output has %q:\n%s", not, out)
		}
	}
	out, _ = b.run("land", "--fresh", "--keep-going", "--run", gotest, "--run", "true")
	if !strings.Contains(out, "pass true") || !strings.Contains(out, "1 of 2 checks failed: "+gotest+"\n") {
		t.Fatalf("--keep-going runs every check:\n%s", out)
	}
	// true ran faster than the Go test stand-in: now it goes first, and
	// the failure still stops the land.
	out, _ = b.run("land", "--fresh", "--run", gotest, "--run", "true")
	if i, j := strings.Index(out, "pass true"), strings.Index(out, "FAIL "+gotest); i < 0 || j < 0 || i > j {
		t.Fatalf("the cheapest check should run first:\n%s", out)
	}
	out, _ = b.run("land", "--fresh", "--run", `test -z "$(echo bad.go)"`)
	if !strings.Contains(out, "(no output; exit status 1)") || !strings.Contains(out, "Hint: test -z fails when `echo bad.go` prints something, and the test swallows what it printed: run `echo bad.go` to see it.") {
		t.Fatalf("test -z hint:\n%s", out)
	}
}
