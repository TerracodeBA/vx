package merge

import "strings"

// Result is the outcome of a three-way merge.
type Result struct {
	Lines     []string // merged text; contains conflict markers if Conflicts > 0
	Conflicts int      // number of conflicting regions
	// KeptBoth counts regions where both sides inserted different lines at
	// the same place and the merge kept both insertions.
	KeptBoth int
	// BothDeleted counts regions where each side deleted different,
	// neighbouring lines and the merge applied both deletions.
	BothDeleted int
}

// Bytes returns the merged text.
func (r Result) Bytes() []byte { return JoinLines(r.Lines) }

// Labels name the sides in conflict markers.
type Labels struct {
	Base, Ours, Theirs string
}

// Merge3 merges ours and theirs, which both started from base. Regions
// changed on one side only take that side. Regions changed identically on
// both sides take the change once.
//
// Regions where both sides only inserted lines at the same place (the base
// region is empty) keep both insertions: theirs first, then ours. Callers
// pass the side that landed earlier (the effort) as theirs. Nothing either
// side had is lost; only the order may need a look. This does not apply when
// base is empty (both sides created the file) or when ours or theirs already
// contains conflict markers.
//
// Regions where both sides only deleted lines, and no line was deleted by
// both (neighbouring but separate blocks), apply both deletions. Overlapping
// deletions, or a deletion next to an edit, stay conflicts.
//
// Other regions changed differently on both sides are conflicts and are
// written with diff3-style markers.
func Merge3(base, ours, theirs []string, labels Labels) Result {
	mo := matches(base, ours)
	mt := matches(base, theirs)
	keepBoth := len(base) > 0 && !HasConflictMarkers(ours) && !HasConflictMarkers(theirs)

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
		case len(b) == 0 && keepBoth:
			res.KeptBoth++
			res.Lines = appendWithNewline(res.Lines, t)
			res.Lines = append(res.Lines, o...)
		case keepBoth && separateDeletions(b, o, t, mo[i-len(b):i], mt[i-len(b):i]):
			// Each base line of the region was deleted by exactly one side:
			// nothing of the region is left.
			res.BothDeleted++
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

// separateDeletions reports whether, in a region of base lines b (with
// their matches mo and mt into ours and theirs), ours region o and theirs
// region t each only deleted lines and no line was deleted by both. Every
// base line of such a region is deleted by exactly one side, because a line
// both kept would have ended the region.
func separateDeletions(b, o, t []string, mo, mt []int) bool {
	if len(b) == 0 || len(o) >= len(b) || len(t) >= len(b) {
		return false
	}
	keptO, keptT := 0, 0
	for x := range b {
		switch {
		case mo[x] < 0 && mt[x] < 0:
			return false // both deleted this line: overlapping deletions
		case mo[x] >= 0 && mt[x] >= 0:
			return false // kept by both inside the region: not pure deletions
		case mo[x] >= 0:
			keptO++
		default:
			keptT++
		}
	}
	// Pure deletions: every line left on a side is one of its kept base lines.
	return keptO == len(o) && keptT == len(t)
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

// HasConflictMarkers reports whether lines contain unresolved conflict
// markers: a "<<<<<<< ", a "=======" and a ">>>>>>> " line, in that order
// (the rule vx land uses to refuse files with markers).
func HasConflictMarkers(lines []string) bool {
	state := 0
	for _, l := range lines {
		switch {
		case state == 0 && strings.HasPrefix(l, "<<<<<<< "):
			state = 1
		case state == 1 && l == "=======\n":
			state = 2
		case state == 2 && strings.HasPrefix(l, ">>>>>>> "):
			return true
		}
	}
	return false
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
