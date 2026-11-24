// Package merge implements line diffs, unified diff output and three-way
// merges of text files.
package merge

import (
	"bytes"
	"strings"
)

// SplitLines splits text into lines that keep their "\n", so joining the
// lines gives back the exact input. A final line without "\n" is kept.
func SplitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	n := bytes.Count(data, []byte{'\n'})
	lines := make([]string, 0, n+1)
	s := string(data)
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines
}

// JoinLines is the inverse of SplitLines.
func JoinLines(lines []string) []byte {
	n := 0
	for _, l := range lines {
		n += len(l)
	}
	b := make([]byte, 0, n)
	for _, l := range lines {
		b = append(b, l...)
	}
	return b
}

// IsBinary reports whether data looks like binary content (a NUL byte in the
// first 8000 bytes, the same rule git uses).
func IsBinary(data []byte) bool {
	if len(data) > 8000 {
		data = data[:8000]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// EditKind is the kind of an edit.
type EditKind int

const (
	Equal EditKind = iota
	Delete
	Insert
)

// Edit is a run of lines: Equal covers a[A0:A1] == b[B0:B1], Delete removes
// a[A0:A1], Insert adds b[B0:B1].
type Edit struct {
	Kind   EditKind
	A0, A1 int
	B0, B1 int
}

// Diff returns the shortest edit script from a to b (Myers' algorithm, with
// the common prefix and suffix trimmed first).
func Diff(a, b []string) []Edit {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	var edits []Edit
	if prefix > 0 {
		edits = append(edits, Edit{Equal, 0, prefix, 0, prefix})
	}
	mid := myers(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])
	for _, e := range mid {
		e.A0 += prefix
		e.A1 += prefix
		e.B0 += prefix
		e.B1 += prefix
		edits = append(edits, e)
	}
	if suffix > 0 {
		edits = append(edits, Edit{Equal, len(a) - suffix, len(a), len(b) - suffix, len(b)})
	}
	return coalesce(edits)
}

// myers computes an edit script with the greedy O(ND) algorithm, keeping one
// V array per step to backtrack.
func myers(a, b []string) []Edit {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 {
		return []Edit{{Insert, 0, 0, 0, m}}
	}
	if m == 0 {
		return []Edit{{Delete, 0, n, 0, 0}}
	}
	max := n + m
	offset := max
	v := make([]int, 2*max+2)
	var trace [][]int
	found := false
	for d := 0; d <= max && !found; d++ {
		// Keep only the diagonals step d can read: k in [-d, d]. This makes
		// the trace O(D^2) instead of O(D*(N+M)).
		snapshot := make([]int, 2*d+1)
		copy(snapshot, v[offset-d:offset+d+1])
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1] // down: insertion
			} else {
				x = v[offset+k-1] + 1 // right: deletion
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	// Backtrack from (n, m) to (0, 0), collecting single-line steps.
	type step struct {
		kind EditKind
		x, y int
	}
	var steps []step
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		if d == 0 {
			// The first step is a pure snake from (0, 0).
			for x > 0 && y > 0 {
				x--
				y--
				steps = append(steps, step{Equal, x, y})
			}
			break
		}
		vd := trace[d]
		at := func(k int) int { return vd[k+d] }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			steps = append(steps, step{Equal, x, y})
		}
		if x == prevX {
			y--
			steps = append(steps, step{Insert, x, y})
		} else {
			x--
			steps = append(steps, step{Delete, x, y})
		}
	}
	edits := make([]Edit, 0, len(steps))
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		switch s.kind {
		case Equal:
			edits = append(edits, Edit{Equal, s.x, s.x + 1, s.y, s.y + 1})
		case Delete:
			edits = append(edits, Edit{Delete, s.x, s.x + 1, s.y, s.y})
		case Insert:
			edits = append(edits, Edit{Insert, s.x, s.x, s.y, s.y + 1})
		}
	}
	return edits
}

// coalesce merges adjacent edits of the same kind.
func coalesce(edits []Edit) []Edit {
	var out []Edit
	for _, e := range edits {
		if e.A0 == e.A1 && e.B0 == e.B1 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Kind == e.Kind && out[n-1].A1 == e.A0 && out[n-1].B1 == e.B0 {
			out[n-1].A1 = e.A1
			out[n-1].B1 = e.B1
			continue
		}
		out = append(out, e)
	}
	return out
}

// Stats counts added and deleted lines.
func Stats(edits []Edit) (added, deleted int) {
	for _, e := range edits {
		switch e.Kind {
		case Insert:
			added += e.B1 - e.B0
		case Delete:
			deleted += e.A1 - e.A0
		}
	}
	return
}

// LineStats returns the added and deleted line counts between two texts.
// Binary content counts as zero lines.
func LineStats(a, b []byte) (added, deleted int) {
	if IsBinary(a) || IsBinary(b) {
		return 0, 0
	}
	return Stats(Diff(SplitLines(a), SplitLines(b)))
}
