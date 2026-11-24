package version

import (
	"regexp"
	"strconv"
	"strings"
)

// MinCLIVersion is the oldest vx CLI this server works with. A server
// refuses requests from older release builds with an "update vx" error.
// Raise it when the server stops understanding what older CLIs send.
const MinCLIVersion = "0.1.0"

// Header is the request header in which the CLI sends its version.
const Header = "X-Vx-Version"

// IsDev reports whether v is not a release version: "dev", empty, or
// anything that does not parse as one. Dev builds are never refused and
// never told to update.
func IsDev(v string) bool {
	_, ok := parse(v)
	return !ok
}

// Compare compares two release versions like 0.3.1 or v0.3.1-rc.1 and
// returns -1, 0 or 1. ok is false when either is not a release version.
func Compare(a, b string) (int, bool) {
	pa, okA := parse(a)
	pb, okB := parse(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range 3 {
		if pa.n[i] != pb.n[i] {
			if pa.n[i] < pb.n[i] {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case pa.pre == pb.pre:
		// Same release: a build N commits after its tag (git describe)
		// comes after it.
		switch {
		case pa.post < pb.post:
			return -1, true
		case pa.post > pb.post:
			return 1, true
		}
		return 0, true
	case pa.pre == "": // 1.0.0 > 1.0.0-rc.1
		return 1, true
	case pb.pre == "":
		return -1, true
	case pa.pre < pb.pre:
		return -1, true
	}
	return 1, true
}

type semver struct {
	n    [3]int
	pre  string
	post int // commits after the tag, from git describe (v0.1.0-73-gf389dfe)
}

// describeSuffix is what git describe appends to a tag: -N-gHASH and
// optionally -dirty. It means "N commits after this tag", not a
// pre-release, so v0.1.0-73-gf389dfe is newer than v0.1.0.
var describeSuffix = regexp.MustCompile(`-(\d+)-g[0-9a-f]{4,40}(-dirty)?$`)

func parse(v string) (semver, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	var s semver
	if m := describeSuffix.FindStringSubmatchIndex(v); m != nil {
		s.post, _ = strconv.Atoi(v[m[2]:m[3]])
		v = v[:m[0]]
	}
	core, pre, _ := strings.Cut(v, "-")
	s.pre = pre
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return s, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return s, false
		}
		s.n[i] = n
	}
	return s, true
}
