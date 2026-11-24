package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// When a [[generated]] command is missing or fails, or files still have
// conflict markers, vx keeps the warning and says exactly what to run.
func TestRegenerateSaysWhatToRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	wc, err := workcopy.Create(dir, workcopy.Config{Repo: "r", Effort: "e"})
	if err != nil {
		t.Fatal(err)
	}
	toml := "[[generated]]\nglob = \"gen/**\"\ncommand = \"vx-no-such-generator --all\"\n\n[[generated]]\nglob = \"out/**\"\ncommand = \"echo boom; exit 4\"\n"
	if err := os.WriteFile(filepath.Join(dir, "vx.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &App{Out: &out, Dir: dir}
	files := []engine.GeneratedFile{{Path: "gen/a.go"}, {Path: "out/b.txt"}, {Path: "x.pb.go"}}
	results, stale := a.regenerate(ctx, wc, files)
	if len(stale) != 3 {
		t.Fatalf("stale = %v, want all three", stale)
	}
	a.printRegenProblems(results)
	for _, want := range []string{
		"Not regenerated: `vx-no-such-generator` is not on PATH. Run `vx-no-such-generator --all` at the root of the working copy.\n",
		"Not regenerated: `echo boom; exit 4` failed (exit status 4):\n    boom\n  Fix that, then run `echo boom; exit 4` at the root of the working copy.\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// Conflict markers anywhere: the generator would read them; wait.
	if err := os.WriteFile(filepath.Join(dir, "src.txt"), []byte("<<<<<<< local\na\n=======\nb\n>>>>>>> effort\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vx.toml"), []byte("[[generated]]\nglob = \"out/**\"\ncommand = \"true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, stale = a.regenerate(ctx, wc, []engine.GeneratedFile{{Path: "out/b.txt"}})
	if len(stale) != 1 || results[0].Ran || results[0].Problem != "conflict markers remain in src.txt" {
		t.Fatalf("markers should hold the generator back: %+v", results)
	}
}

func TestCommandProgram(t *testing.T) {
	for in, want := range map[string]string{
		"buf generate":                      "buf",
		"VX_REQUIRE_DB=1 go generate ./...": "go",
		"(cd proto && buf generate)":        "",
		"":                                  "",
	} {
		if got := commandProgram(in); got != want {
			t.Errorf("commandProgram(%q) = %q, want %q", in, got, want)
		}
	}
}
