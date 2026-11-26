package cli

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// strayFile is a new file that is probably not meant to land: a compiled
// program, other binary data, or a file above the [files] large limit.
type strayFile struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Executable string `json:"executable,omitempty"` // ELF, Mach-O or PE
	Binary     bool   `json:"binary,omitempty"`     // NUL bytes, not a known asset format
	Large      bool   `json:"large,omitempty"`      // above the limit
}

// what says what the file is, for the listing: "ELF executable",
// "binary", "large", "binary, large".
func (f strayFile) what() string {
	var parts []string
	switch {
	case f.Executable != "":
		parts = append(parts, f.Executable+" executable")
	case f.Binary:
		parts = append(parts, "binary")
	}
	if f.Large {
		parts = append(parts, "large")
	}
	return strings.Join(parts, ", ")
}

// sniffBytes is how much of a file vx reads to tell what it is; git looks
// at the first 8000 bytes for NUL as well.
const sniffBytes = 8000

// strayFiles returns the new files among changes that are executables,
// other binaries, or larger than limit, in the order of changes. Images,
// fonts, audio, video and PDFs are assets: listed only above the limit.
func strayFiles(wc *workcopy.WorkingCopy, changes []object.Change, limit int64) []strayFile {
	var out []strayFile
	for _, c := range changes {
		if c.Op != object.OpAdd || c.New.Mode == object.ModeSymlink || c.New.Mode == object.ModeDir {
			continue
		}
		full := filepath.Join(wc.Root, filepath.FromSlash(c.Path))
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		head := make([]byte, sniffBytes)
		n, _ := io.ReadFull(f, head)
		f.Close()
		if err != nil {
			continue
		}
		head = head[:n]
		s := strayFile{Path: c.Path, Size: info.Size(), Large: info.Size() > limit}
		if s.Executable = executableFormat(head); s.Executable == "" && !isAsset(c.Path, head) {
			s.Binary = bytes.IndexByte(head, 0) >= 0
		}
		if s.Executable != "" || s.Binary || s.Large {
			out = append(out, s)
		}
	}
	return out
}

// executableFormat names the executable format of a file's first bytes:
// ELF, Mach-O (including universal binaries) or PE; "" for anything else.
func executableFormat(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	switch {
	case bytes.HasPrefix(b, []byte("\x7fELF")):
		return "ELF"
	case bytes.HasPrefix(b, []byte{0xfe, 0xed, 0xfa, 0xce}), bytes.HasPrefix(b, []byte{0xce, 0xfa, 0xed, 0xfe}),
		bytes.HasPrefix(b, []byte{0xfe, 0xed, 0xfa, 0xcf}), bytes.HasPrefix(b, []byte{0xcf, 0xfa, 0xed, 0xfe}):
		return "Mach-O"
	case bytes.HasPrefix(b, []byte{0xca, 0xfe, 0xba, 0xbe}) && len(b) >= 8:
		// A universal binary counts its architectures here; a Java class
		// file has its version (45 or more) in the same place.
		if n := binary.BigEndian.Uint32(b[4:8]); n > 0 && n < 20 {
			return "Mach-O"
		}
	case bytes.HasPrefix(b, []byte("MZ")) && len(b) >= 0x40:
		off := int(binary.LittleEndian.Uint32(b[0x3c:0x40]))
		if off > 0 && off+4 <= len(b) && bytes.Equal(b[off:off+4], []byte("PE\x00\x00")) {
			return "PE"
		}
	}
	return ""
}

// assetExts are file types that are binary by nature and belong in a
// repository: images, fonts, audio, video, documents.
var assetExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".avif": true, ".ico": true, ".bmp": true,
	".svg": true, ".tif": true, ".tiff": true, ".psd": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp3": true, ".wav": true, ".ogg": true, ".flac": true, ".m4a": true,
	".mp4": true, ".webm": true, ".mov": true,
	".pdf": true,
}

// isAsset reports whether a file is an image, font, audio, video or PDF,
// by its name or its content.
func isAsset(p string, head []byte) bool {
	if assetExts[strings.ToLower(path.Ext(p))] {
		return true
	}
	ct := http.DetectContentType(head)
	for _, pre := range []string{"image/", "font/", "audio/", "video/", "application/pdf"} {
		if strings.HasPrefix(ct, pre) {
			return true
		}
	}
	return false
}

// largeFileLimit is the working copy's vx.toml [files] large, or the
// default when there is none or it does not parse (vx land reports a
// broken vx.toml elsewhere).
func largeFileLimit(wc *workcopy.WorkingCopy) int64 {
	data, err := os.ReadFile(filepath.Join(wc.Root, rules.File))
	if err != nil {
		return rules.DefaultLargeFile
	}
	r, err := rules.Parse(data)
	if err != nil {
		return rules.DefaultLargeFile
	}
	return r.LargeFileLimit()
}

// printStrays lists stray files with their sizes and what to do.
func (a *App) printStrays(strays []strayFile, limit int64, landing bool) {
	if len(strays) == 0 {
		return
	}
	a.printf("\nWarning: new files that look like they should not land (binary, or above %s):\n", byteSize(limit))
	exe := false
	for _, s := range strays {
		a.printf("  %-40s %9s  %s\n", s.Path, byteSize(s.Size), s.what())
		exe = exe || s.Executable != ""
	}
	a.printf("  If they are build output or data, add them to .vxignore (or .gitignore).")
	if exe && !landing {
		a.printf(" vx land refuses executables unless --allow-binary.")
	}
	a.printf("\n")
}

// refuseExecutables is vx land's error for new executables, nil when there
// are none.
func refuseExecutables(strays []strayFile) error {
	var exe []string
	for _, s := range strays {
		if s.Executable != "" {
			exe = append(exe, fmt.Sprintf("%s (%s executable, %s)", s.Path, s.Executable, byteSize(s.Size)))
		}
	}
	if len(exe) == 0 {
		return nil
	}
	return fmt.Errorf("not landing compiled programs: %s. Add them to .vxignore (or .gitignore) and delete them from the working copy, or pass --allow-binary to land them anyway", strings.Join(exe, ", "))
}

// byteSize formats a size: "512 B", "3.1 KiB", "48.2 MiB".
func byteSize(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}
