package rules

import (
	"fmt"
	"regexp"
	"strings"
)

// Glob matches slash-separated repository paths. A "*" matches any run of
// characters within one path segment, "?" one character within a segment,
// and a "**" segment any number of segments (zero or more):
//
//	internal/store/migrations/*.sql   files directly in that directory
//	gen/**                            every file under gen/
//	**/*_string.go                    a file of that name at any depth
//	api/**/types.go                   api/types.go, api/v1/types.go, ...
//
// Paths have no leading slash. Character classes and braces are not
// supported, so a glob means the same thing to everyone reading vx.toml.
type Glob struct {
	pattern string
	re      *regexp.Regexp
	dir     string // the literal directory every match lives under ("" = root)
}

// CompileGlob checks a glob and prepares it for matching. Errors say what
// is wrong and how to write it instead.
func CompileGlob(pattern string) (Glob, error) {
	switch {
	case pattern == "":
		return Glob{}, fmt.Errorf("glob is empty")
	case strings.HasPrefix(pattern, "/"):
		return Glob{}, fmt.Errorf("glob %q starts with /: paths are relative to the repository root, write %q", pattern, strings.TrimLeft(pattern, "/"))
	case strings.HasSuffix(pattern, "/"):
		return Glob{}, fmt.Errorf("glob %q ends with /: globs match files, write %q for everything under it", pattern, pattern+"**")
	case strings.Contains(pattern, `\`):
		return Glob{}, fmt.Errorf(`glob %q contains a backslash: separate directories with /`, pattern)
	case strings.ContainsAny(pattern, "[]{}"):
		return Glob{}, fmt.Errorf("glob %q uses [ ] or { }: only *, ? and ** are supported, write one rule per pattern", pattern)
	}
	segs := strings.Split(pattern, "/")
	var b strings.Builder
	b.WriteString("^")
	afterDouble := false
	literal := true
	var dir []string
	for i, seg := range segs {
		last := i == len(segs)-1
		switch {
		case seg == "":
			return Glob{}, fmt.Errorf("glob %q has an empty path segment (//)", pattern)
		case seg == "." || seg == "..":
			return Glob{}, fmt.Errorf("glob %q has a %q segment: write the path from the repository root", pattern, seg)
		case seg == "**":
			literal = false
			sep := "/"
			if i == 0 || afterDouble {
				sep = ""
			}
			if last {
				b.WriteString(sep + ".+")
			} else {
				b.WriteString(sep + "(?:.+/)?")
			}
			afterDouble = true
			continue
		case strings.Contains(seg, "**"):
			return Glob{}, fmt.Errorf("glob %q: ** must be a whole path segment, as in gen/** or **/*.go", pattern)
		}
		if i > 0 && !afterDouble {
			b.WriteString("/")
		}
		afterDouble = false
		if strings.ContainsAny(seg, "*?") {
			literal = false
		} else if literal && !last {
			dir = append(dir, seg)
		}
		for _, r := range seg {
			switch r {
			case '*':
				b.WriteString("[^/]*")
			case '?':
				b.WriteString("[^/]")
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return Glob{}, fmt.Errorf("glob %q: %v", pattern, err)
	}
	return Glob{pattern: pattern, re: re, dir: strings.Join(dir, "/")}, nil
}

// Match reports whether path matches the glob.
func (g Glob) Match(path string) bool { return g.re != nil && g.re.MatchString(path) }

// Dir is the literal directory every matching path lives under, "" for
// the repository root: the part of the tree a scan has to read.
func (g Glob) Dir() string { return g.dir }

// String returns the glob as written.
func (g Glob) String() string { return g.pattern }

// MatchGlob reports whether path matches pattern; an invalid pattern
// matches nothing.
func MatchGlob(pattern, path string) bool {
	g, err := CompileGlob(pattern)
	return err == nil && g.Match(path)
}
