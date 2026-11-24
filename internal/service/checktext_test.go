package service

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
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

func TestRebasedChangeLabels(t *testing.T) {
	ch := object.Check{Name: "go test ./...", Result: "pass", RanByVX: true}
	cs := &object.Changeset{
		ChangeID: "kxqr-7m2p", Intent: "Add x", Author: object.Author{Principal: "agent:alice/claude"},
		Evidence: &object.Evidence{Checks: []object.Check{ch}},
		Rebase:   &object.Rebase{Branch: "main", OntoGitSHA: "0123456789abcdef0123", By: "user:alice", ResolvedBy: "agent:alice/claude"},
	}
	msg := commitMessage("feature", cs)
	for _, want := range []string{
		"Vx-Rebased-Onto: main at 0123456789ab\n",
		"Vx-Resolved-By: agent:alice/claude\n",
		"Vx-Evidence: go test ./... pass (run by vx), before rebase\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message lacks %q:\n%s", want, msg)
		}
	}
	body := prBody(ShipPlan{
		Target: "main", Effort: store.Effort{Slug: "feature"}, TargetHead: store.ChangesetRow{Changeset: &object.Changeset{GitSHA: "fedcba9876543210"}},
		Steps: []ShipStep{{Changeset: store.ChangesetRow{Changeset: cs}}},
	})
	for _, want := range []string{
		"- `kxqr-7m2p`: go test ./... pass (run by vx), before the effort was rebased onto main at 0123456789ab",
		"  - conflict resolved during the rebase by agent:alice/claude",
		"The effort was rebased onto `main` at 0123456789ab by user:alice; evidence of the rebased changes was gathered before the rebase.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, body)
		}
	}
	plain := commitMessage("feature", &object.Changeset{Intent: "Add y", Evidence: &object.Evidence{Checks: []object.Check{ch}}})
	if strings.Contains(plain, "rebase") {
		t.Fatalf("a change that was not rebased mentions a rebase:\n%s", plain)
	}
}
