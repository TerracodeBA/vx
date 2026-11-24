package version

import "testing"

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"0.3.0", "v0.3.0", 0, true},
		{"0.2.9", "0.3.0", -1, true},
		{"0.10.0", "0.9.1", 1, true},
		{"1.0.0-rc.1", "1.0.0", -1, true},
		{"1.0.0", "1.0.0-rc.1", 1, true},
		{"v1.2.3+meta", "1.2.3", 0, true},
		{"dev", "0.1.0", 0, false},
		{"", "0.1.0", 0, false},
		{"0.1", "0.1.0", 0, false},
		// Builds from git: git describe means "N commits after the tag".
		{"v0.1.0-73-gf389dfe", "0.1.0", 1, true},
		{"0.1.0", "v0.1.0-73-gf389dfe", -1, true},
		{"v0.1.0-73-gf389dfe-dirty", "0.1.0", 1, true},
		{"v0.1.0-9-gabcdef1", "v0.1.0-73-gf389dfe", -1, true},
		{"v0.1.0-73-gf389dfe", "0.2.0", -1, true},
		{"v1.0.0-rc.1-4-gabcdef1", "1.0.0-rc.1", 1, true},
		{"v1.0.0-rc.1-4-gabcdef1", "1.0.0", -1, true},
		// Go pseudo-versions are pre-releases of the next version.
		{"v0.1.1-0.20261124161910-f389dfe05be9", "0.1.0", 1, true},
	} {
		got, ok := Compare(tc.a, tc.b)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
	if !IsDev("dev") || IsDev("v0.3.0") || !IsDev("") {
		t.Fatal("IsDev")
	}
	if IsDev(MinCLIVersion) {
		t.Fatal("MinCLIVersion must be a release version")
	}
}
