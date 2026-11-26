// Package version reports the build version, set at release time with
// -ldflags "-X github.com/TerracodeBA/vx/internal/version.Version=v0.1.0 ...".
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set by the release build.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// String returns a one-line description of the build.
func String() string {
	v, c := Version, Commit
	if info, ok := debug.ReadBuildInfo(); ok && v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version // go install module@version
	}
	if c == "" {
		c = vcsCommit()
	}
	out := v
	if c != "" {
		out += " (" + c + ")"
	}
	if Date != "" {
		out += " built " + Date
	}
	return fmt.Sprintf("%s %s/%s", out, runtime.GOOS, runtime.GOARCH)
}

// vcsCommit is the git commit go build recorded (a build inside a git
// checkout), with "-modified" when the files differed from it; "" when
// none was recorded, as in a vx working copy built without make build.
func vcsCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && modified == "true" {
		rev += "-modified"
	}
	return rev
}
