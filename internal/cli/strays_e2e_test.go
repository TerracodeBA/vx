package cli_test

import (
	"strings"
	"testing"
)

// vx status and vx land list new binary and large files; vx land refuses
// executables unless --allow-binary; images stay allowed below the limit.
func TestStrayBinariesCLI(t *testing.T) {
	_, a, _ := boardPair(t)
	a.must("new", "Add the server")
	elf := "\x7fELF\x02\x01\x01\x00" + strings.Repeat("\x00", 64)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" + strings.Repeat("\x00", 32)
	a.write("vx-server", elf)
	a.write("assets/logo.png", png)
	a.write("data/dump.bin", "\x00\x01\x02\x03")
	a.write("big.txt", strings.Repeat("x", 1<<20+1))
	a.write("main.go", "package main\n")

	out := a.must("status")
	for _, want := range []string{
		"Warning: new files that look like they should not land (binary, or above 1.0 MiB):",
		"vx-server", "ELF executable",
		"data/dump.bin", "binary",
		"big.txt", "1.0 MiB  large",
		"add them to .vxignore (or .gitignore). vx land refuses executables unless --allow-binary.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status: missing %q in\n%s", want, out)
		}
	}
	if warn := out[strings.Index(out, "Warning: new files"):]; strings.Contains(warn, "logo.png") || strings.Contains(warn, "main.go") {
		t.Errorf("status lists an image or a source file:\n%s", out)
	}

	out, code := a.run("land")
	if code == 0 || !strings.Contains(out, "not landing compiled programs: vx-server (ELF executable, 72 B)") || !strings.Contains(out, "--allow-binary") {
		t.Fatalf("land with an executable = %d\n%s", code, out)
	}
	out = a.must("land", "--allow-binary")
	if !strings.Contains(out, "data/dump.bin") || !strings.Contains(out, "Landed ") {
		t.Fatalf("land --allow-binary:\n%s", out)
	}

	// [files] large raises the limit; a large image warns, a small one not.
	a.must("new", "Bigger limit")
	a.write("vx.toml", "[files]\nlarge = \"4 MiB\"\n")
	a.write("big2.txt", strings.Repeat("y", 2<<20))
	a.write("assets/hero.png", png+strings.Repeat("\x00", 5<<20))
	out = a.must("status")
	if warn := out[strings.Index(out, "Warning: new files"):]; strings.Contains(warn, "big2.txt") || !strings.Contains(out, "assets/hero.png") || !strings.Contains(out, "above 4.0 MiB") {
		t.Fatalf("status with [files] large:\n%s", out)
	}
}
