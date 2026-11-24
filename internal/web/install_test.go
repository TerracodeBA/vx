package web_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// fakeReleases serves GitHub-Releases-shaped URLs for vx v9.9.9: /latest
// redirects to the tag, /download/v9.9.9/ has the archive for this machine
// and checksums.txt (wrong when corrupt is set).
func fakeReleases(t *testing.T, corrupt bool) (string, string) {
	t.Helper()
	archive := fmt.Sprintf("vx_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	bin := []byte("#!/bin/sh\necho 'vx 9.9.9 (fake)'\n")
	for name, body := range map[string][]byte{"vx": bin, "LICENSE": []byte("MIT\n"), "README.md": []byte("# vx\n")} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	gz.Close()
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	if corrupt {
		sum[0] ^= 0xff
	}
	checksums := fmt.Sprintf("%x  vx-server_9.9.9_linux_amd64.tar.gz\n%x  %s\n", sha256.Sum256([]byte("other")), sum, archive)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v9.9.9", http.StatusFound)
	})
	mux.HandleFunc("GET /releases/tag/v9.9.9", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "release page") })
	mux.HandleFunc("GET /releases/download/v9.9.9/"+archive, func(w http.ResponseWriter, r *http.Request) { w.Write(data) })
	mux.HandleFunc("GET /releases/download/v9.9.9/checksums.txt", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, checksums) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/releases", archive
}

// runInstall fetches /install.sh from a vx server and runs it with sh in a
// fresh HOME, never touching the real ~/.local/bin.
func runInstall(t *testing.T, server string, env ...string) (string, error) {
	t.Helper()
	resp, err := http.Get(server + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(script), "#!/bin/sh") {
		t.Fatalf("/install.sh = %d %.80s", resp.StatusCode, script)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, script, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", path)
	cmd.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin", "SHELL=/bin/bash"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh is for Linux and macOS")
	}
	for _, tool := range []string{"sh", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	svc := service.New(storetest.New(t), blob.NewMem(), nil)
	mux := http.NewServeMux()
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	vxServer := httptest.NewServer(mux)
	t.Cleanup(vxServer.Close)

	t.Run("latest into ~/.local/bin, not on PATH", func(t *testing.T) {
		releases, _ := fakeReleases(t, false)
		home := t.TempDir()
		out, err := runInstall(t, vxServer.URL, "HOME="+home, "VX_RELEASES_URL="+releases)
		if err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
		bin := filepath.Join(home, ".local", "bin", "vx")
		fi, err := os.Stat(bin)
		if err != nil || fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("vx not installed executable: %v %v\n%s", fi, err, out)
		}
		if got, _ := exec.Command(bin).Output(); string(got) != "vx 9.9.9 (fake)\n" {
			t.Fatalf("installed vx prints %q", got)
		}
		for _, want := range []string{
			"Installed vx v9.9.9 to " + bin,
			filepath.Join(home, ".local", "bin") + " is not on your PATH",
			`echo 'export PATH="$HOME/.local/bin:$PATH"' >> ` + filepath.Join(home, ".bashrc"),
			"vx login --server " + vxServer.URL + "\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		// Only vx was installed, nothing else in the directory.
		entries, _ := os.ReadDir(filepath.Join(home, ".local", "bin"))
		if len(entries) != 1 {
			t.Errorf("install dir has %d entries", len(entries))
		}
	})

	t.Run("VX_VERSION into VX_INSTALL_DIR on PATH", func(t *testing.T) {
		releases, _ := fakeReleases(t, false)
		home, dir := t.TempDir(), t.TempDir()
		out, err := runInstall(t, vxServer.URL, "HOME="+home, "VX_RELEASES_URL="+releases, "VX_VERSION=9.9.9", "VX_INSTALL_DIR="+dir,
			"PATH="+dir+":/usr/local/bin:/usr/bin:/bin")
		if err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "vx")); err != nil || !strings.Contains(out, dir+" is on your PATH.") {
			t.Fatalf("install = %v\n%s", err, out)
		}
	})

	t.Run("checksum mismatch installs nothing", func(t *testing.T) {
		releases, archive := fakeReleases(t, true)
		home := t.TempDir()
		out, err := runInstall(t, vxServer.URL, "HOME="+home, "VX_RELEASES_URL="+releases)
		if err == nil || !strings.Contains(out, "checksum mismatch for "+archive) || !strings.Contains(out, "nothing was installed") {
			t.Fatalf("install = %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(home, ".local", "bin", "vx")); !os.IsNotExist(err) {
			t.Fatalf("vx was installed despite the bad checksum: %v", err)
		}
	})

	t.Run("missing release", func(t *testing.T) {
		releases, _ := fakeReleases(t, false)
		out, err := runInstall(t, vxServer.URL, "HOME="+t.TempDir(), "VX_RELEASES_URL="+releases, "VX_VERSION=v1.2.3")
		if err == nil || !strings.Contains(out, "could not download") {
			t.Fatalf("install = %v\n%s", err, out)
		}
	})

	t.Run("PublicURL names the server", func(t *testing.T) {
		svc.GitHub.PublicURL = "https://vx.example.com"
		t.Cleanup(func() { svc.GitHub.PublicURL = "" })
		resp, err := http.Get(vxServer.URL + "/install.sh")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "server='https://vx.example.com'") {
			t.Fatalf("script does not name the public URL")
		}
	})
}
