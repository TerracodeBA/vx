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
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 12 {
					c = s.Value[:12]
				}
			}
		}
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
