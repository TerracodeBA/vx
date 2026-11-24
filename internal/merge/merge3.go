package merge

import "strings"

// Result is the outcome of a three-way merge.
type Result struct {
	Lines     []string // merged text; contains conflict markers if Conflicts > 0
	Conflicts int      // number of conflicting regions
}

// Bytes returns the merged text.
func (r Result) Bytes() []byte { return JoinLines(r.Lines) }

// Labels name the sides in conflict markers.
type Labels struct {
	Base, Ours, Theirs string
}

// Merge3 merges ours and theirs, which both started from base. Regions
// changed on one side only take that side. Regions changed identically on
// both sides take the change once. Regions changed differently on both sides
// are conflicts and are written with diff3-style markers.
func Merge3(base, ours, theirs []string, labels Labels) Result {
	mo := matches(base, ours)
	mt := matches(base, theirs)

	var res Result
	i, j, k := 0, 0, 0
	for {
		// Stable run: base, ours and theirs advance together.
		n := 0
		for i+n < len(base) && mo[i+n] == j+n && mt[i+n] == k+n {
			n++
		}
		if n > 0 {
			res.Lines = append(res.Lines, base[i:i+n]...)
			i, j, k = i+n, j+n, k+n
			continue
		}
		if i >= len(base) && j >= len(ours) && k >= len(theirs) {
			break
		}
		// Find the next base line matched on both sides, after the cursors.
		ni := -1
		for x := i; x < len(base); x++ {
			if mo[x] >= j && mt[x] >= k {
				ni = x
				break
			}
		}
		var b, o, t []string
		if ni < 0 {
			b, o, t = base[i:], ours[j:], theirs[k:]
			i, j, k = len(base), len(ours), len(theirs)
		} else {
			b, o, t = base[i:ni], ours[j:mo[ni]], theirs[k:mt[ni]]
			i, j, k = ni, mo[ni], mt[ni]
		}
		switch {
		case equalLines(o, b):
			res.Lines = append(res.Lines, t...)
		case equalLines(t, b):
			res.Lines = append(res.Lines, o...)
		case equalLines(o, t):
			res.Lines = append(res.Lines, o...)
		default:
			res.Conflicts++
			res.Lines = appendConflict(res.Lines, b, o, t, labels)
		}
	}
	return res
}

// Merge3Bytes is Merge3 on raw file contents. It reports ok=false without
// merging when any side is binary and the sides differ.
func Merge3Bytes(base, ours, theirs []byte, labels Labels) (Result, bool) {
	if IsBinary(base) || IsBinary(ours) || IsBinary(theirs) {
		switch {
		case string(ours) == string(theirs), string(theirs) == string(base):
			return Result{Lines: SplitLines(ours)}, true
		case string(ours) == string(base):
			return Result{Lines: SplitLines(theirs)}, true
		}
		return Result{Conflicts: 1}, false
	}
	return Merge3(SplitLines(base), SplitLines(ours), SplitLines(theirs), labels), true
}

// matches maps each base line index to the index of the matching line in
// other, or -1 when the line was deleted or changed.
func matches(base, other []string) []int {
	m := make([]int, len(base))
	for i := range m {
		m[i] = -1
	}
	for _, e := range Diff(base, other) {
		if e.Kind != Equal {
			continue
		}
		for x := 0; x < e.A1-e.A0; x++ {
			m[e.A0+x] = e.B0 + x
		}
	}
	return m
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func appendConflict(out, base, ours, theirs []string, l Labels) []string {
	marker := func(prefix, name string) string {
		if name == "" {
			return prefix + "\n"
		}
		return prefix + " " + name + "\n"
	}
	out = append(out, marker("<<<<<<<", l.Ours))
	out = appendWithNewline(out, ours)
	out = append(out, marker("|||||||", l.Base))
	out = appendWithNewline(out, base)
	out = append(out, "=======\n")
	out = appendWithNewline(out, theirs)
	out = append(out, marker(">>>>>>>", l.Theirs))
	return out
}

// appendWithNewline appends lines, making sure the last ends in "\n" so the
// next marker starts on its own line.
func appendWithNewline(out, lines []string) []string {
	for i, l := range lines {
		if i == len(lines)-1 && !strings.HasSuffix(l, "\n") {
			l += "\n"
		}
		out = append(out, l)
	}
	return out
}
