package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

// forYouKinds is how many items of each kind ForYou returns for actor.
func (e *feedbackEnv) forYouKinds(actor service.Actor) (service.ForYou, map[string]int) {
	e.t.Helper()
	f, err := e.svc.ForYou(e.ctx, actor)
	if err != nil {
		e.t.Fatal(err)
	}
	n := map[string]int{}
	for _, it := range f.Items {
		n[it.Kind]++
	}
	return f, n
}

// For you shows failed CI on the person's agents' shipment once (not
// passing CI); someone who cannot read the repository gets none of it.
func TestForYouCIAndRebase(t *testing.T) {
	e := newFeedbackEnv(t)
	_, res := e.shipped()
	pr := e.pr(res)
	e.deliver("check_run", e.gh.CheckRunEvent(e.inst, "acme/shop", pr, "completed", "github-actions", "test", "completed", "success"))
	e.deliver("check_suite", e.gh.CheckSuiteEvent(e.inst, "acme/shop", pr, "github-actions", "success"))
	if _, n := e.forYouKinds(e.alice); n[service.ForYouCI] != 0 {
		t.Fatalf("passing CI is on For you: %v", n)
	}
	e.deliver("check_run", e.gh.CheckRunEvent(e.inst, "acme/shop", pr, "rerequested", "github-actions", "test", "queued", ""))
	e.deliver("check_run", e.gh.CheckRunEvent(e.inst, "acme/shop", pr, "completed", "github-actions", "test", "completed", "failure"))
	f, n := e.forYouKinds(e.alice)
	if n[service.ForYouCI] != 1 {
		t.Fatalf("failed CI not on alice's For you: %v", n)
	}
	for _, it := range f.Items {
		if it.Kind == service.ForYouCI && !strings.Contains(it.Note.Body, "CI failed on PR #7") {
			t.Fatalf("CI item = %+v", it.Note)
		}
	}
	// Her agent sees its own CI note too.
	if _, n := e.forYouKinds(e.claude); n[service.ForYouCI] != 1 {
		t.Fatalf("agent's For you: %v", n)
	}

	// Bob has no role on acme/shop.
	bob := e.person("bob")
	if f, _ := e.forYouKinds(bob); len(f.Items) != 0 {
		t.Fatalf("bob sees %d items", len(f.Items))
	}
}

// For you shows the conflict note of an effort that needs a rebase, not
// the note of one vx rebased, newest first.
func TestForYouRebase(t *testing.T) {
	e := newFeedbackEnv(t)
	clean, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: e.repo.Name, Name: "Limiter"})
	e.must(err)
	conflicting, err := e.svc.CreateEffort(e.ctx, e.alice, service.CreateEffortInput{Repo: e.repo.Name, Name: "Docs"})
	e.must(err)
	e.landAs(e.claude, clean, "Add limiter", map[string]*string{"limit.go": s("package shop\n\nfunc Allow() bool { return true }\n")})
	e.landAs(e.codex, conflicting, "Retitle", map[string]*string{"README.md": s("# Shop API\n")})
	e.push(e.commitMain("Rename the shop", map[string]*string{"README.md": s("# Store\n")}))
	f, n := e.forYouKinds(e.alice)
	if n[service.ForYouRebase] != 1 {
		t.Fatalf("rebase items = %v", n)
	}
	for i, it := range f.Items {
		if it.Kind == service.ForYouRebase && (it.Note.EffortSlug != conflicting.Slug || !strings.Contains(it.Note.Body, "needs a rebase")) {
			t.Fatalf("rebase item = %+v", it.Note)
		}
		if i > 0 && it.At.After(f.Items[i-1].At) {
			t.Fatalf("not newest first at %d", i)
		}
	}
}
