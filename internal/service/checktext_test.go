package service

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/object"
)

func TestCheckTextShowsNotes(t *testing.T) {
	ch := object.Check{Name: "go test ./...", Result: "pass", RanByVX: true, Notes: "12 of 14 packages from the Go test cache"}
	if got, want := checkText(ch), "go test ./... pass (run by vx, 12 of 14 packages from the Go test cache)"; got != want {
		t.Fatalf("checkText = %q, want %q", got, want)
	}
	msg := commitMessage("feature", &object.Changeset{Intent: "Add x", Evidence: &object.Evidence{Checks: []object.Check{ch}}})
	if !strings.Contains(msg, "Vx-Evidence: go test ./... pass (run by vx, 12 of 14 packages from the Go test cache)") {
		t.Fatalf("commit message = %s", msg)
	}
}
