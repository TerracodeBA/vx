package rules

import (
	"strings"
	"testing"
)

func TestFilesLarge(t *testing.T) {
	r, err := Parse(nil)
	if err != nil || r.LargeFileLimit() != DefaultLargeFile {
		t.Fatalf("default: %d %v", r.LargeFileLimit(), err)
	}
	for in, want := range map[string]int64{
		`large = "4 MiB"`: 4 << 20, `large = "500KB"`: 500_000, `large = 2048`: 2048, `large = "1.5 MiB"`: 3 << 19, `large = "2M"`: 2 << 20,
	} {
		r, err := Parse([]byte("[files]\n" + in + "\n"))
		if err != nil || r.LargeFileLimit() != want || r.IsEmpty() {
			t.Errorf("%s: %d %v, want %d", in, r.LargeFileLimit(), err, want)
		}
	}
	for in, msg := range map[string]string{
		`large = "big"`:    `vx.toml line 2: [files] large: "big" is not a size`,
		`large = "3 PB"`:   `has an unknown unit "PB"`,
		`large = 0`:        "0 is not a positive size",
		`large = "-1 MiB"`: "is not a size",
	} {
		_, err := Parse([]byte("[files]\n" + in + "\n"))
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: %v, want %q", in, err, msg)
		}
	}
	r, err = Parse([]byte("[files]\nlarge = \"1 MiB\"\nhuge = 3\n"))
	if err != nil || len(r.Ignored) != 1 || !strings.Contains(r.Ignored[0], `[files] uses key "huge"`) {
		t.Fatalf("unknown key: %+v %v", r.Ignored, err)
	}
}
