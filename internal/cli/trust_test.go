package cli

import (
	"bytes"
	"strings"
	"testing"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// Another agent of the same person reads as "[same person]", never as
// "[yours]" (which agents read as "written by me"), also from servers
// before v0.3 that still send "yours".
func TestTrustLabelSamePerson(t *testing.T) {
	for in, want := range map[string]string{
		"self": "self", "same_person": "same person", "yours": "same person", "teammate": "teammate", "": "teammate", "boss": "teammate",
	} {
		if got := trustLabel(in); got != want {
			t.Errorf("trustLabel(%q) = %q, want %q", in, got, want)
		}
	}
	var out bytes.Buffer
	a := &App{Out: &out}
	a.printNote(&vxv1.Note{Id: "1a2b3c4d5e", Kind: "plan", Trust: "yours", Body: "x", Author: &vxv1.Principal{Ref: "agent:alice/b"}}, noteOpts{})
	a.printClaim(&vxv1.Claim{Id: "2b3c4d5e6f", Trust: "same_person", Owner: &vxv1.Principal{Ref: "agent:alice/b"}}, "")
	if s := out.String(); strings.Contains(s, "yours") || strings.Count(s, "[same person]") != 2 {
		t.Errorf("printed:\n%s", s)
	}
}
