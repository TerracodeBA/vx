package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

const shipCheckRules = `[[check]]
name   = "browser-e2e"
when   = "ship"
runner = "agent:browser"
paths  = ["web/**"]
`

func (e *splitEnv) reportHead(status string) {
	e.t.Helper()
	if _, err := e.svc.ReportCheck(e.ctx, e.agent, service.ReportInput{Repo: "acme/shop", Effort: e.effort.Slug, Name: "browser-e2e", Status: status, Summary: "checkout flow"}); err != nil {
		e.t.Fatal(err)
	}
}

// A check that blocks ship needs a passing result on the effort head's
// tree before vx pushes anything; a new head needs a new result.
func TestCheckGateOnShip(t *testing.T) {
	e := newSplitEnv(t, map[string]string{"README.md": "# shop\n", "vx.toml": shipCheckRules})
	e.land("Add checkout page", map[string]string{"web/checkout.js": "go()\n"})
	_, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug})
	if !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "not shipped: 1 check that blocks ship has no passing result on the effort head's tree ") ||
		!strings.Contains(err.Error(), "browser-e2e  missing  runner agent:browser: vx check report browser-e2e --status pass") {
		t.Fatalf("ship without the browser check: %v", err)
	}
	if e.gh.created != 0 {
		t.Fatal("a refused ship opened a pull request")
	}
	e.reportHead("fail")
	if _, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug}); err == nil || !strings.Contains(err.Error(), "browser-e2e  failed   agent:alice/backend at") {
		t.Fatalf("ship with a failed result: %v", err)
	}
	e.reportHead("pass")
	if _, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug}); err != nil {
		t.Fatalf("ship with a passing result: %v", err)
	}
	// A docs change needs no browser run, but moves the head: the check
	// applies to what ships (web/checkout.js), so it needs a new result.
	e.land("Document it", map[string]string{"README.md": "# shop\n\nCheckout.\n"})
	if _, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug}); err == nil || !strings.Contains(err.Error(), "browser-e2e") {
		t.Fatalf("a new head needs a new result: %v", err)
	}
}

// Split ship checks every part: only parts that touch the check's paths
// need it.
func TestCheckGateOnSplitShip(t *testing.T) {
	e := newSplitEnv(t, map[string]string{"README.md": "# shop\n", "vx.toml": shipCheckRules})
	e.land("Add checkout page", map[string]string{"web/checkout.js": "go()\n"})
	e.land("Add limiter", map[string]string{"limit.go": "package shop\n"})
	_, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitIndependent})
	if err == nil || !strings.Contains(err.Error(), "which part 1 of 2 ships:") {
		t.Fatalf("split ship without the browser check: %v", err)
	}
	e.reportHead("pass")
	if _, err := e.svc.OpenShipment(e.ctx, e.human, service.ShipInput{Repo: "acme/shop", Effort: e.effort.Slug, Split: service.SplitIndependent}); err != nil {
		t.Fatalf("split ship with a passing result: %v", err)
	}
}
