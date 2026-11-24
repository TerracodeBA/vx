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

func TestReleaseAndOlderThanServer(t *testing.T) {
	for v, want := range map[string]string{"v0.4.0": "v0.4.0", "0.4.0": "v0.4.0", "v0.4.0-12-gabc1234-dirty": "v0.4.0", "v0.5.0-rc.1": "v0.5.0-rc.1", "dev": ""} {
		if got, _ := Release(v); got != want {
			t.Errorf("Release(%q) = %q, want %q", v, got, want)
		}
	}
	for _, tc := range []struct{ cli, server, want string }{
		{"v0.3.0", "v0.4.0", "vx v0.3.0 is older than the server (v0.4.0): vx self-update"},
		{"0.3.0", "v0.4.0-7-g1234abc", "vx v0.3.0 is older than the server (v0.4.0): vx self-update"},
		{"v0.4.0", "v0.4.0-7-g1234abc", ""}, // the server is a build after v0.4.0: there is no newer release
		{"v0.4.0", "v0.4.0", ""},
		{"v0.5.0", "v0.4.0", ""},
		{"dev", "v0.4.0", ""},
		{"v0.3.0", "dev", ""},
	} {
		if got := OlderThanServer(tc.cli, tc.server); got != tc.want {
			t.Errorf("OlderThanServer(%q, %q) = %q, want %q", tc.cli, tc.server, got, tc.want)
		}
	}
}
