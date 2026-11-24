package cli

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// goTestCache counts the package result lines of `go test` output: how
// many packages reported a result, and how many of those came from the
// test cache instead of running. Packages without test files are not
// counted.
//
//	ok  	example.com/a	(cached)
//	ok  	example.com/b	1.204s
//	?   	example.com/c	[no test files]
//	FAIL	example.com/d	0.311s
//	FAIL	example.com/e [build failed]
func goTestCache(output string) (cached, total int) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 2 {
			continue
		}
		status := strings.TrimSpace(fields[0])
		if status != "ok" && status != "FAIL" {
			continue
		}
		total++
		if len(fields) < 3 { // "FAIL\tpkg [build failed]"
			continue
		}
		// With -cover the cache marker is followed by a coverage field:
		// "ok  \tpkg\t(cached)\tcoverage: 80.0% of statements".
		if strings.TrimSpace(fields[2]) == "(cached)" {
			cached++
		}
	}
	return cached, total
}

// cacheNote describes test cache use for Check.notes, or "" when no
// package came from the cache.
func cacheNote(output string) string {
	cached, total := goTestCache(output)
	if cached == 0 {
		return ""
	}
	pkgs := "packages"
	if total == 1 {
		pkgs = "package"
	}
	return fmt.Sprintf("%d of %d %s from the Go test cache", cached, total, pkgs)
}

// splitNewTests accepts repeated and comma-separated --new-test values and
// returns the names in order, without blanks or repeats.
func splitNewTests(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range values {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// testFuncRe matches Go test, benchmark, fuzz and example functions,
// including methods (test suites).
var testFuncRe = regexp.MustCompile(`(?m)^func\s+(?:\([^)]*\)\s*)?((?:Test|Benchmark|Fuzz|Example)\w*)\s*[\[(]`)

// verifyNewTests checks that every name is declared as a function in a
// _test.go file the changes add or modify. It returns an error naming the
// missing tests and the test functions that the changed files do declare.
func verifyNewTests(ctx context.Context, wc *workcopy.WorkingCopy, changes []object.Change, names []string) error {
	if len(names) == 0 {
		return nil
	}
	var files []string
	var found []string
	foundSet := map[string]bool{}
	var sources [][]byte
	for _, ch := range changes {
		if ch.Op == object.OpDelete || !strings.HasSuffix(ch.Path, "_test.go") {
			continue
		}
		data, err := wc.ReadBlob(ctx, ch.New.Hash)
		if err != nil {
			return err
		}
		files = append(files, ch.Path)
		sources = append(sources, data)
		for _, m := range testFuncRe.FindAllSubmatch(data, -1) {
			if name := string(m[1]); !foundSet[name] {
				foundSet[name] = true
				found = append(found, name)
			}
		}
	}
	var missing []string
	for _, name := range names {
		re := regexp.MustCompile(`(?m)^func\s+(?:\([^)]*\)\s*)?` + regexp.QuoteMeta(name) + `\s*[\[(]`)
		ok := false
		for _, src := range sources {
			if re.Match(src) {
				ok = true
				break
			}
		}
		if !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "nothing landed: --new-test %s: ", strings.Join(missing, ", "))
	if len(files) == 0 {
		b.WriteString("this land adds or changes no _test.go files.")
	} else {
		sort.Strings(found)
		fmt.Fprintf(&b, "no such test func in the _test.go files this land adds or changes (%s).", strings.Join(files, ", "))
		if len(found) > 0 {
			fmt.Fprintf(&b, "\nTest functions in those files: %s.", strings.Join(found, ", "))
		} else {
			b.WriteString("\nThose files declare no test functions.")
		}
	}
	b.WriteString("\nFix the name, add the test, or pass --allow-unverified-tests to land anyway.")
	return fmt.Errorf("%s", b.String())
}

// notesDropped reports whether the server stored fewer check notes than
// the CLI sent, which happens when the server runs an older vx that does
// not know the field.
func notesDropped(sent, stored *vxv1.Evidence) bool {
	count := func(e *vxv1.Evidence) int {
		n := 0
		for _, ch := range e.GetChecks() {
			if ch.Notes != "" {
				n++
			}
		}
		return n
	}
	return count(stored) < count(sent)
}
