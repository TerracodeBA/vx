package merge

import (
	"fmt"
	"strings"
)

// Unified renders a unified diff between a and b with the given number of
// context lines. Paths are written as "a/<path>" and "b/<path>"; an empty
// name means /dev/null (file added or deleted). It returns "" when equal.
func Unified(oldName, newName string, a, b []byte, context int) string {
	if IsBinary(a) || IsBinary(b) {
		if string(a) == string(b) {
			return ""
		}
		return fmt.Sprintf("Binary files %s and %s differ\n", label("a", oldName), label("b", newName))
	}
	la, lb := SplitLines(a), SplitLines(b)
	edits := Diff(la, lb)
	if len(edits) == 0 || (len(edits) == 1 && edits[0].Kind == Equal) {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", label("a", oldName), label("b", newName))
	for _, h := range hunks(edits, context) {
		writeHunk(&sb, h, la, lb)
	}
	return sb.String()
}

func label(side, name string) string {
	if name == "" {
		return "/dev/null"
	}
	return side + "/" + name
}

// hunks groups edits into hunks with up to context lines of unchanged text
// around each change. Two changes separated by more than 2*context unchanged
// lines go into separate hunks.
func hunks(edits []Edit, context int) [][]Edit {
	var out [][]Edit
	var cur []Edit
	last := len(edits) - 1
	lead := func(e Edit) []Edit {
		c := min(e.A1-e.A0, context)
		return []Edit{{Equal, e.A1 - c, e.A1, e.B1 - c, e.B1}}
	}
	for i, e := range edits {
		if e.Kind != Equal {
			cur = append(cur, e)
			continue
		}
		if !hasChange(cur) {
			cur = nil
			if i != last {
				cur = lead(e)
			}
			continue
		}
		if i != last && e.A1-e.A0 <= 2*context {
			cur = append(cur, e)
			continue
		}
		c := min(e.A1-e.A0, context)
		cur = append(cur, Edit{Equal, e.A0, e.A0 + c, e.B0, e.B0 + c})
		out = append(out, trimEmpty(cur))
		cur = nil
		if i != last {
			cur = lead(e)
		}
	}
	if hasChange(cur) {
		out = append(out, trimEmpty(cur))
	}
	return out
}

func hasChange(h []Edit) bool {
	for _, e := range h {
		if e.Kind != Equal {
			return true
		}
	}
	return false
}

func trimEmpty(h []Edit) []Edit {
	out := h[:0]
	for _, e := range h {
		if e.A0 != e.A1 || e.B0 != e.B1 {
			out = append(out, e)
		}
	}
	return out
}

func writeHunk(sb *strings.Builder, h []Edit, a, b []string) {
	a0, b0 := h[0].A0, h[0].B0
	a1, b1 := h[len(h)-1].A1, h[len(h)-1].B1
	fmt.Fprintf(sb, "@@ -%s +%s @@\n", rangeStr(a0, a1-a0), rangeStr(b0, b1-b0))
	for _, e := range h {
		switch e.Kind {
		case Equal:
			for _, l := range a[e.A0:e.A1] {
				writeLine(sb, ' ', l)
			}
		case Delete:
			for _, l := range a[e.A0:e.A1] {
				writeLine(sb, '-', l)
			}
		case Insert:
			for _, l := range b[e.B0:e.B1] {
				writeLine(sb, '+', l)
			}
		}
	}
}

func rangeStr(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func writeLine(sb *strings.Builder, prefix byte, line string) {
	sb.WriteByte(prefix)
	sb.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		sb.WriteString("\n\\ No newline at end of file\n")
	}
}
