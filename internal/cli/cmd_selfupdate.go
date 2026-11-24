package cli

// vx self-update: install the vx release the server runs, the way
// install.sh does (the release archive for this OS and CPU, checked
// against the release's checksums.txt), in place of the running vx.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/version"
)

// defaultReleases is where vx releases are published; VX_RELEASES_URL
// overrides it, as for install.sh.
const defaultReleases = "https://github.com/TerracodeBA/vx/releases"

// maxRelease bounds a downloaded archive.
const maxRelease = 200 << 20

func (a *App) selfUpdateCmd() *cobra.Command {
	var want string
	var check bool
	cmd := &cobra.Command{
		Use:   "self-update [--version vX.Y.Z] [--check]",
		Short: "Install the vx release the server runs, in place of this vx",
		Long: `Download the vx release the server runs (or --version), check it against
the release's checksums.txt and install it, the way the server's
install.sh does.

Where it goes: in place of the vx you ran, when that is a release build in
a directory you can write; otherwise into VX_INSTALL_DIR or ~/.local/bin,
and vx says which one comes first on your PATH. vx self-update never
writes anywhere else, never replaces a dev build (one built from source:
rebuild it instead), and does not downgrade unless --version asks for it.

VX_RELEASES_URL points it at another release server, as for install.sh.`,
		Example: `  vx self-update
  vx self-update --check
  vx self-update --version v0.4.0`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctxOf(cmd)
			cur := version.Version
			server := strings.TrimRight(a.settings().Server, "/")
			if version.IsDev(cur) {
				hint := "rebuild it from source (go build -o bin/vx ./cmd/vx), or install a release"
				if server != "" {
					hint += ": curl -fsSL " + server + "/install.sh | sh"
				}
				return fmt.Errorf("this vx is a dev build (%s); vx self-update only replaces release builds: %s", version.String(), hint)
			}
			target, explicit := strings.TrimSpace(want), strings.TrimSpace(want) != ""
			if !explicit {
				if server == "" {
					return errors.New("not logged in to a vx server, so there is no server version to match; log in (vx login --server URL) or give --version vX.Y.Z")
				}
				info, err := a.fetchServerInfo(ctx, server)
				if err != nil {
					return fmt.Errorf("could not ask %s for its version: %v; give --version vX.Y.Z instead", server, err)
				}
				rel, ok := version.Release(info.Version)
				if !ok {
					return fmt.Errorf("the server %s runs a dev build (%s), not a release; give --version vX.Y.Z", server, info.Version)
				}
				target = rel
			}
			rel, ok := version.Release(target)
			if !ok {
				return fmt.Errorf("--version %q is not a release version such as v0.4.0", target)
			}
			c, _ := version.Compare(cur, rel)
			if c == 0 || (c > 0 && !explicit) {
				what := "matches"
				if c > 0 {
					what = "is newer than"
				}
				a.printf("vx %s %s the server's release (%s): nothing to do.\n", cur, what, rel)
				a.emit(map[string]any{"version": cur, "target": rel, "updated": false})
				return nil
			}
			if check {
				a.printf("vx %s can be updated to %s: run vx self-update.\n", cur, rel)
				a.emit(map[string]any{"version": cur, "target": rel, "updated": false})
				return nil
			}
			if runtime.GOOS == "windows" {
				return fmt.Errorf("vx self-update does not replace vx on Windows; download the zip of %s from %s", rel, a.releasesURL())
			}
			bin, err := a.downloadRelease(ctx, rel)
			if err != nil {
				return err
			}
			path, note, err := a.installBinary(bin)
			if err != nil {
				return err
			}
			a.printf("Updated vx %s to %s at %s.\n", cur, rel, path)
			if note != "" {
				a.printf("%s\n", note)
			}
			a.emit(map[string]any{"version": cur, "target": rel, "updated": true, "path": path})
			return nil
		},
	}
	cmd.Flags().StringVar(&want, "version", "", "install this release instead of the server's (may downgrade)")
	cmd.Flags().BoolVar(&check, "check", false, "only say whether there is an update")
	return cmd
}

func (a *App) releasesURL() string {
	if u := strings.TrimSpace(a.Getenv("VX_RELEASES_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return defaultReleases
}

// downloadRelease fetches the vx binary of release rel for this OS and CPU,
// checked against the release's checksums.txt, exactly as install.sh does.
func (a *App) downloadRelease(ctx context.Context, rel string) ([]byte, error) {
	base := a.releasesURL()
	archive := fmt.Sprintf("vx_%s_%s_%s.tar.gz", strings.TrimPrefix(rel, "v"), runtime.GOOS, runtime.GOARCH)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	get := func(name string) ([]byte, error) {
		u := base + "/download/" + rel + "/" + name
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("could not download %s: %v; nothing was installed", u, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("could not download %s: %s; nothing was installed", u, resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxRelease+1))
		if err != nil {
			return nil, fmt.Errorf("could not download %s: %v; nothing was installed", u, err)
		}
		if len(data) > maxRelease {
			return nil, fmt.Errorf("%s is larger than %d MB; nothing was installed", u, maxRelease>>20)
		}
		return data, nil
	}
	a.printf("Downloading vx %s for %s/%s...\n", rel, runtime.GOOS, runtime.GOARCH)
	data, err := get(archive)
	if err != nil {
		return nil, err
	}
	sums, err := get("checksums.txt")
	if err != nil {
		return nil, fmt.Errorf("the checksums of vx %s cannot be read, so the download cannot be verified: %w", rel, err)
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && (f[1] == archive || f[1] == "*"+archive) {
			want = strings.ToLower(f[0])
			break
		}
	}
	if want == "" {
		return nil, fmt.Errorf("checksums.txt of vx %s does not list %s; nothing was installed", rel, archive)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("checksum mismatch for %s (expected %s, got %s); nothing was installed", archive, want, got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("could not unpack %s: %v; nothing was installed", archive, err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no vx binary; nothing was installed", archive)
		}
		if err != nil {
			return nil, fmt.Errorf("could not unpack %s: %v; nothing was installed", archive, err)
		}
		if h.Typeflag == tar.TypeReg && filepath.Clean(h.Name) == "vx" {
			bin, err := io.ReadAll(io.LimitReader(tr, maxRelease))
			if err != nil {
				return nil, fmt.Errorf("could not unpack %s: %v; nothing was installed", archive, err)
			}
			return bin, nil
		}
	}
}

// installBinary writes bin in place of the running vx when its directory
// is writable, else into VX_INSTALL_DIR or ~/.local/bin, through a rename
// so a running vx is never half replaced. note says what to do about PATH.
func (a *App) installBinary(bin []byte) (path, note string, err error) {
	exe := a.Executable
	if exe == nil {
		exe = os.Executable
	}
	self, err := exe()
	if err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
	}
	if err == nil && writableDir(filepath.Dir(self)) {
		return self, "", replaceFile(self, bin)
	}
	dir := strings.TrimSpace(a.Getenv("VX_INSTALL_DIR"))
	if dir == "" {
		home := a.Getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		dir = filepath.Join(home, ".local", "bin")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("could not create %s: %v; set VX_INSTALL_DIR to a directory you can write", dir, err)
	}
	path = filepath.Join(dir, "vx")
	if err := replaceFile(path, bin); err != nil {
		return "", "", err
	}
	note = fmt.Sprintf("%s (the vx you ran) is in a directory you cannot write, so it is unchanged; put %s ahead of it on your PATH.", self, dir)
	if self == "" {
		note = fmt.Sprintf("Put %s on your PATH.", dir)
	}
	return path, note, nil
}

func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".vx-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// replaceFile writes data next to path, then renames it over path.
func replaceFile(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.new.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("could not write %s: %v; set VX_INSTALL_DIR to a directory you can write", tmp, err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not replace %s: %v", path, err)
	}
	return nil
}
