package engine

import (
	"bytes"
	"regexp"
)

// generatedLine is Go's standard marker for generated files
// (https://go.dev/s/generatedcode), which tools such as buf, protoc and
// stringer write. Other languages' generators mostly follow it too.
var generatedLine = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// generatedLines is how many leading lines IsGenerated looks at.
const generatedLines = 10

// IsGenerated reports whether a file says it was generated: one of its
// first lines is "// Code generated ... DO NOT EDIT.". A conflict in such a
// file is fixed by regenerating it, not by editing conflict markers.
func IsGenerated(data []byte) bool {
	for i := 0; i < generatedLines && len(data) > 0; i++ {
		line := data
		if j := bytes.IndexByte(data, '\n'); j >= 0 {
			line, data = data[:j], data[j+1:]
		} else {
			data = nil
		}
		if generatedLine.Match(bytes.TrimRight(line, "\r")) {
			return true
		}
	}
	return false
}
