package cli_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const cliPipelineRules = `[[check]]
name   = "build"
runner = "agent:build"
blocks = "nothing"

[[check]]
name   = "e2e"
runner = "agent:browser"
after  = ["build"]
inputs = ["build.preview_url"]
blocks = "nothing"

[[check]]
name   = "security"
run    = "true"
blocks = "nothing"
`

// TestPipelineStatusCLI: vx check status draws the pipeline (a check
// indented under the one it runs after), says where each check comes
// from, skips the checks after a failure with the reason, and says when
// the change edits vx.toml; vx rules lists the organization's checks and
// the vx.toml check they override.
func TestPipelineStatusCLI(t *testing.T) {
	_, work, clone := speedSetup(t)
	b := clone("b")
	org := filepath.Join(work, "org-checks.toml")
	if err := os.WriteFile(org, []byte("[[check]]\nname   = \"security\"\nrunner = \"human\"\nblocks = \"nothing\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.must("org", "checks", "alice", "--file", org)
	b.must("new", "Checks")
	b.write("vx.toml", cliPipelineRules)
	b.must("land")

	out := b.must("check", "status")
	for _, want := range []string{
		`Warning: vx.toml [[check]] "security" is ignored: organization alice defines a check of that name`,
		"[from organization alice (version 1)]",
		"[from vx.toml line 1]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`(?m)^  build +missing .*\n  {3}e2e +waiting +waits for build`).MatchString(out) {
		t.Errorf("e2e is not drawn under build, waiting:\n%s", out)
	}
	b.must("check", "report", "build", "--status", "fail", "--summary", "does not compile")
	out = b.must("check", "status")
	if !regexp.MustCompile(`(?m)^  {3}e2e +skipped +skipped: build failed`).MatchString(out) {
		t.Errorf("e2e is not skipped after build failed:\n%s", out)
	}
	out = b.must("rules")
	if !strings.Contains(out, "Organization checks, from organization alice (version 1)") || !strings.Contains(out, "security") {
		t.Errorf("rules:\n%s", out)
	}

	// A change that edits vx.toml is held to the checks of the effort's
	// base until it lands, and status says so.
	b.must("new", "Drop e2e")
	b.write("vx.toml", strings.Split(cliPipelineRules, "[[check]]\nname   = \"e2e\"")[0])
	out = b.must("check", "status")
	if !strings.Contains(out, "this change edits vx.toml: the checks of the effort's base apply until it lands") {
		t.Errorf("status of a change to vx.toml:\n%s", out)
	}
}
