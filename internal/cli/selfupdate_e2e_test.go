package cli_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/cli"
	"github.com/TerracodeBA/vx/internal/version"
)

// fakeReleases serves release v9.9.9 like GitHub does; corrupt makes its
// checksum wrong.
func fakeReleases(t *testing.T, corrupt *bool) *httptest.Server {
	t.Helper()
	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	bin := []byte("#!/bin/sh\necho vx v9.9.9\n")
	for _, f := range []struct {
		name string
		data []byte
	}{{"README.md", []byte("readme")}, {"vx", bin}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.data)
	}
	tw.Close()
	zw.Close()
	archive := fmt.Sprintf("vx_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(tgz.Bytes())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download/v9.9.9/"+archive, func(w http.ResponseWriter, _ *http.Request) { w.Write(tgz.Bytes()) })
	mux.HandleFunc("GET /download/v9.9.9/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		s := hex.EncodeToString(sum[:])
		if *corrupt {
			s = strings.Repeat("0", 64)
		}
		fmt.Fprintf(w, "%s  vx_9.9.9_other_arch.tar.gz\n%s  %s\n", strings.Repeat("1", 64), s, archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// An older vx says so (at most hourly), and vx self-update installs the
// server's release over the running vx after checking its checksum; dev
// builds are refused; an unwritable directory gets ~/.local/bin instead.
func TestSelfUpdate(t *testing.T) {
	f := &fakeOldServer{}
	mux := http.NewServeMux()
	mux.Handle(vxv1connect.NewPublicServiceHandler(f))
	mux.Handle(vxv1connect.NewAuthServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	corrupt := false
	rel := fakeReleases(t, &corrupt)

	home := t.TempDir()
	binDir := filepath.Join(home, "opt")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(binDir, "vx")
	if err := os.WriteFile(self, []byte("old vx"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"VX_CONFIG": filepath.Join(home, "config.json"), "HOME": home, "VX_RELEASES_URL": rel.URL}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		app := &cli.App{Out: &out, Err: &errb, Getenv: func(k string) string { return env[k] }, Dir: home,
			Executable: func() (string, error) { return self, nil }}
		code := app.Run(context.Background(), args)
		return code, out.String(), errb.String()
	}

	// A dev build is never replaced.
	if code, _, errText := run("login", "--server", srv.URL, "--token", "vxt_fake"); code != 0 {
		t.Fatalf("login = %d %s", code, errText)
	}
	if code, _, errText := run("self-update"); code != 1 || !strings.Contains(errText, "is a dev build") || !strings.Contains(errText, srv.URL+"/install.sh") {
		t.Fatalf("self-update of a dev build = %d %q", code, errText)
	}

	old := version.Version
	t.Cleanup(func() { version.Version = old })
	version.Version = "v0.3.0"
	notice := "vx: vx v0.3.0 is older than the server (v9.9.9): vx self-update\n"
	if code, _, errText := run("whoami"); code != 0 || errText != notice {
		t.Fatalf("whoami of an older vx = %d %q", code, errText)
	}
	if _, _, errText := run("whoami"); errText != "" {
		t.Fatalf("the notice is repeated at most hourly: %q", errText)
	}
	if code, out, _ := run("self-update", "--check"); code != 0 || !strings.Contains(out, "vx v0.3.0 can be updated to v9.9.9") {
		t.Fatalf("self-update --check = %d %q", code, out)
	}

	corrupt = true
	if code, _, errText := run("self-update"); code != 1 || !strings.Contains(errText, "checksum mismatch") || !strings.Contains(errText, "nothing was installed") {
		t.Fatalf("self-update with a bad checksum = %d %q", code, errText)
	}
	if data, _ := os.ReadFile(self); string(data) != "old vx" {
		t.Fatalf("a failed update changed vx: %q", data)
	}
	corrupt = false
	code, out, errText := run("self-update")
	if code != 0 || !strings.Contains(out, "Updated vx v0.3.0 to v9.9.9 at "+self+".") {
		t.Fatalf("self-update = %d %q %q", code, out, errText)
	}
	if data, _ := os.ReadFile(self); !strings.Contains(string(data), "echo vx v9.9.9") {
		t.Fatalf("vx after the update: %q", data)
	}

	// Up to date: nothing to do.
	version.Version = "v9.9.9"
	if code, out, _ := run("self-update"); code != 0 || !strings.Contains(out, "matches the server's release (v9.9.9): nothing to do") {
		t.Fatalf("self-update when up to date = %d %q", code, out)
	}

	// The running vx is in a directory vx cannot write: ~/.local/bin.
	if os.Getuid() == 0 {
		return // root writes anywhere
	}
	version.Version = "v0.3.0"
	if err := os.Chmod(binDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(binDir, 0o755) })
	code, out, errText = run("self-update")
	local := filepath.Join(home, ".local", "bin", "vx")
	if code != 0 || !strings.Contains(out, "Updated vx v0.3.0 to v9.9.9 at "+local+".") || !strings.Contains(out, "put "+filepath.Join(home, ".local", "bin")+" ahead of it on your PATH") {
		t.Fatalf("self-update into ~/.local/bin = %d %q %q", code, out, errText)
	}
}
