package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/service"
)

// TestReporterTrustRules checks every reporter rule against every kind of
// reporter: the author agent, a sibling agent of the same person, the
// person, another person and their agent, a CI token and another service.
func TestReporterTrustRules(t *testing.T) {
	author := service.ChangeAuthor{Principal: "agent:alice/backend", Person: "user:alice"}
	byHand := service.ChangeAuthor{Principal: "user:alice", Person: "user:alice"}
	who := map[string]service.ReporterIdentity{
		"author":  {Kind: service.ReporterIsAgent, Principal: "agent:alice/backend", Person: "user:alice"},
		"sibling": {Kind: service.ReporterIsAgent, Principal: "agent:alice/browser", Person: "user:alice"},
		"person":  {Kind: service.ReporterIsPerson, Principal: "user:alice", Person: "user:alice"},
		"bob":     {Kind: service.ReporterIsPerson, Principal: "user:bob", Person: "user:bob"},
		"bobs":    {Kind: service.ReporterIsAgent, Principal: "agent:bob/browser", Person: "user:bob"},
		"ci":      {Kind: service.ReporterIsCI, Principal: "service:ci-1a2b3c4d"},
		"service": {Kind: service.ReporterIsService, Principal: "service:deploy"},
	}
	cases := []struct {
		rule     string
		same     bool
		author   service.ChangeAuthor
		allowed  []string
		refusedM string // a word every refusal says
	}{
		{"self", false, author, []string{"author", "sibling", "person", "bob", "bobs", "ci", "service"}, ""},
		{"", false, author, []string{"author", "sibling", "person", "bob", "bobs", "ci", "service"}, ""},
		{"independent", false, author, []string{"bob", "bobs", "ci", "service"}, "independent"},
		{"independent", true, author, []string{"sibling", "person", "bob", "bobs", "ci", "service"}, "made this change"},
		{"ci", false, author, []string{"ci"}, "CI"},
		{"human", false, author, []string{"person", "bob"}, "person"},
		// A change a person made by hand: they cannot sign it off themselves.
		{"human", false, byHand, []string{"bob"}, ""},
		{"independent", false, byHand, []string{"bob", "bobs", "ci", "service"}, ""},
	}
	for _, c := range cases {
		for name, by := range who {
			ok, why := service.ReporterAllowed(c.rule, c.same, c.author, by)
			want := false
			for _, a := range c.allowed {
				want = want || a == name
			}
			if ok != want {
				t.Errorf("rule %q same=%v author %s, reporter %s: allowed=%v want %v (%s)", c.rule, c.same, c.author.Principal, name, ok, want, why)
			}
			if ok && why != "" {
				t.Errorf("rule %q reporter %s: allowed with a reason %q", c.rule, name, why)
			}
			if !ok && (why == "" || !strings.Contains(why, c.refusedM)) {
				t.Errorf("rule %q reporter %s: reason %q should say %q", c.rule, name, why, c.refusedM)
			}
		}
	}
	// Agents can never report as a person, whatever their person is.
	for _, by := range []service.ReporterIdentity{who["author"], who["sibling"], who["bobs"]} {
		if ok, _ := service.ReporterAllowed("human", true, author, by); ok {
			t.Errorf("agent %s reported a human check", by.Principal)
		}
	}
	if ok, why := service.ReporterAllowed("anyone", false, author, who["bob"]); ok || !strings.Contains(why, "unknown reporter rule") {
		t.Errorf("unknown rule: %v %q", ok, why)
	}
}
