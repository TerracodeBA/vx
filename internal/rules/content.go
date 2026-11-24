package rules

// [[unique]] rules that look inside files: content (and optionally scope)
// instead of key. Two efforts that both add field 12 to message
// LandResponse each merge cleanly; the combined file has field 12 twice.
//
//	[[unique]]
//	name    = "proto-field"
//	glob    = "proto/**/*.proto"
//	scope   = '^\s*message\s+(\w+)'
//	content = '^\s*(?:(?:optional|repeated|required)\s+)?(?:map<[^>]+>|[A-Za-z_][\w.]*)\s+[A-Za-z_]\w*\s*=\s*(\d+)'
//
// content is matched against each line of each file the glob covers; its
// first group is the value that must be unique. scope, when set, starts a
// scope on each line it matches (its first group names it) and values only
// need to be unique within a scope. A scope opened on a line with "{" ends
// at the matching "}" (nested scopes are named outer.inner); in files
// without braces a scope lasts until the next line scope matches. Scopes
// of the same name in different files are one scope.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

// maxContentFile is the largest file a content rule reads; bigger ones are
// skipped.
const maxContentFile = 4 << 20

// IsContent reports whether the rule looks inside files rather than at
// their names.
func (u Unique) IsContent() bool { return u.Content != "" }

// occurrence is one use of a value: where it is.
type occurrence struct {
	path string
	line int
}

// ContentKeys returns the values content matches in data, each prefixed
// with its scope ("LandResponse 12"; without scope, or outside every
// scope, just "12"), with the 1-based lines they are on. scope may be nil.
func ContentKeys(content, scope *regexp.Regexp, data []byte) map[string][]int {
	out := map[string][]int{}
	type open struct {
		name   string
		depth  int  // brace depth on the line that opened it
		braced bool // a brace opened after it
	}
	var stack []open
	depth := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), maxContentFile)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		code := line
		if i := strings.Index(code, "//"); i >= 0 {
			code = code[:i]
		}
		if scope != nil {
			if m := scope.FindStringSubmatch(code); m != nil && len(m) > 1 && m[1] != "" {
				// A brace-less scope at this depth ends where the next begins.
				for len(stack) > 0 && !stack[len(stack)-1].braced && stack[len(stack)-1].depth >= depth {
					stack = stack[:len(stack)-1]
				}
				stack = append(stack, open{name: m[1], depth: depth})
			}
		}
		if m := content.FindStringSubmatch(code); m != nil && len(m) > 1 && m[1] != "" {
			key := m[1]
			if len(stack) > 0 {
				names := make([]string, len(stack))
				for i, o := range stack {
					names[i] = o.name
				}
				key = strings.Join(names, ".") + " " + key
			}
			out[key] = append(out[key], n)
		}
		for _, r := range code {
			switch r {
			case '{':
				depth++
				for i := range stack {
					if stack[i].depth < depth {
						stack[i].braced = true
					}
				}
			case '}':
				depth--
				for len(stack) > 0 && stack[len(stack)-1].braced && depth <= stack[len(stack)-1].depth {
					stack = stack[:len(stack)-1]
				}
			}
		}
	}
	return out
}

// contentUses reads every file of tree that g covers and returns where each
// value of the rule is used.
func contentUses(ctx context.Context, s blob.Store, tree hash.Hash, g Glob, content, scope *regexp.Regexp) (map[string][]occurrence, error) {
	out := map[string][]occurrence{}
	if tree.IsZero() {
		return out, nil
	}
	files, err := Files(ctx, s, tree, g)
	if err != nil {
		return nil, err
	}
	for _, p := range files {
		e, ok, err := object.Lookup(ctx, s, tree, p)
		if err != nil {
			return nil, err
		}
		if !ok || e.Mode == object.ModeDir || e.Mode == object.ModeSymlink {
			continue
		}
		data, err := blob.GetKind(ctx, s, hash.KindBlob, e.Hash)
		if err != nil {
			return nil, err
		}
		if len(data) > maxContentFile {
			continue
		}
		for k, lines := range ContentKeys(content, scope, data) {
			for _, l := range lines {
				out[k] = append(out[k], occurrence{path: p, line: l})
			}
		}
	}
	return out, nil
}

// checkContent reports the values of a content rule used more than once in
// tree, limited to those used more often than in base (a clash that
// predates the change is not repeated on every land).
func checkContent(ctx context.Context, s blob.Store, u Unique, g Glob, base, tree hash.Hash) ([]engine.Warning, error) {
	content, err := regexp.Compile(u.Content)
	if err != nil || content.NumSubexp() < 1 {
		return nil, nil
	}
	var scope *regexp.Regexp
	if u.Scope != "" {
		if scope, err = regexp.Compile(u.Scope); err != nil || scope.NumSubexp() < 1 {
			return nil, nil
		}
	}
	now, err := contentUses(ctx, s, tree, g, content, scope)
	if err != nil {
		return nil, err
	}
	before, err := contentUses(ctx, s, base, g, content, scope)
	if err != nil {
		return nil, err
	}
	var keys []string
	for k, occ := range now {
		if len(occ) > 1 && len(occ) > len(before[k]) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []engine.Warning
	for _, k := range keys {
		occ := now[k]
		sort.Slice(occ, func(i, j int) bool {
			if occ[i].path != occ[j].path {
				return occ[i].path < occ[j].path
			}
			return occ[i].line < occ[j].line
		})
		var files, where []string
		var lines []string
		for i, o := range occ {
			lines = append(lines, fmt.Sprint(o.line))
			if i == len(occ)-1 || occ[i+1].path != o.path {
				files = append(files, o.path)
				word := "line"
				if len(lines) > 1 {
					word = "lines"
				}
				where = append(where, fmt.Sprintf("%s %s %s", o.path, word, joinAnd(lines)))
				lines = nil
			}
		}
		what := k
		if sc, val, ok := strings.Cut(k, " "); ok {
			what = val + " in " + sc
		}
		times := fmt.Sprintf("%d times", len(occ))
		if len(occ) == 2 {
			times = "twice"
		}
		out = append(out, engine.Warning{
			Path: files[0], Kind: WarningRule, Name: u.Name, Decl: k, Files: files,
			Detail: fmt.Sprintf("%s %s is used %s: %s (rule %s in %s)", u.Name, what, times, strings.Join(where, "; "), u.Name, File),
		})
	}
	return out, nil
}
