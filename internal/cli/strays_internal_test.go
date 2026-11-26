package cli

import (
	"encoding/binary"
	"testing"
)

func TestExecutableFormat(t *testing.T) {
	pe := make([]byte, 0x100)
	copy(pe, "MZ")
	binary.LittleEndian.PutUint32(pe[0x3c:], 0x80)
	copy(pe[0x80:], "PE\x00\x00")
	fat := []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 2}
	class := []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 61} // Java 17 class file
	for _, c := range []struct {
		name string
		b    []byte
		want string
	}{
		{"elf", []byte("\x7fELF\x02\x01\x01\x00"), "ELF"},
		{"macho64", []byte{0xcf, 0xfa, 0xed, 0xfe, 7, 0, 0, 1}, "Mach-O"},
		{"universal", fat, "Mach-O"},
		{"java class", class, ""},
		{"pe", pe, "PE"},
		{"MZ text", []byte("MZ is a text that starts like a DOS header but is not one, long enough to read 0x3c....."), ""},
		{"text", []byte("package main\n"), ""},
		{"short", []byte("\x7fE"), ""},
	} {
		if got := executableFormat(c.b); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIsAsset(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	if !isAsset("logo.bin", png) || !isAsset("font.woff2", []byte{0, 1, 2}) || !isAsset("Docs/Guide.PDF", nil) {
		t.Fatal("assets not recognized")
	}
	if isAsset("data.bin", []byte{0, 1, 2, 3}) || isAsset("vx-server", []byte("\x7fELF\x00")) {
		t.Fatal("binary data taken for an asset")
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 3 << 10: "3.0 KiB", 48<<20 + 1<<19: "48.5 MiB", 2 << 30: "2.0 GiB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
