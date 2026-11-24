package service_test

import (
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
)

// A claim tied to an open workspace lasts as long as the workspace: it is
// still active (and renewed) after its duration, and ends when the
// workspace lands. An untied claim still expires.
func TestClaimLastsAsLongAsItsWorkspace(t *testing.T) {
	b := newBoardEnv(t)
	ws, err := b.svc.OpenWorkspace(b.ctx, b.bobBE, "shop-api", b.effort.Slug, "Limiter")
	if err != nil {
		t.Fatal(err)
	}
	tied, _ := b.claim(b.bobBE, ws.Workspace.ID.String(), "", "internal/http/")
	b.claim(b.bobBE, "", "", "docs/")
	b.now = b.now.Add(3 * time.Hour)
	cs := b.listClaims(b.agent)
	if len(cs) != 1 || cs[0].ID != tied.ID || !cs[0].ExpiresAt.After(b.now) {
		t.Fatalf("claims after 3h = %+v", cs)
	}
	// Landing over it still warns, and the owner can release it.
	other, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", b.effort.Slug, "Touch http")
	if err != nil {
		t.Fatal(err)
	}
	tree := b.snapshot(b.repo, other.Workspace.Base, map[string]*string{"internal/http/x.go": s("package http\n")})
	res, err := b.svc.Land(b.ctx, b.agent, service.LandInput{WorkspaceID: other.Workspace.ID, Tree: tree})
	if err != nil || !res.Landed || len(res.OverlappingClaims) != 1 {
		t.Fatalf("land over the workspace claim: %+v %v", res.OverlappingClaims, err)
	}
	// The workspace is abandoned: its claim ends.
	if _, err := b.svc.Abandon(b.ctx, b.bobBE, ws.Workspace.ID); err != nil {
		t.Fatal(err)
	}
	if cs := b.listClaims(b.agent); len(cs) != 0 {
		t.Fatalf("claims after abandoning = %+v", cs)
	}
}

const limitBase = `package limit

// Allow says whether key may go on.
func Allow(key string) bool {
	return true
}

// Reset forgets key.
func Reset(key string) {
}

type Limiter struct{ n int }

func (l *Limiter) Take() bool { return l.n > 0 }
`

// A symbol claim only counts against a land that changes that symbol's
// lines: two agents changing different functions of one file do not
// collide, and other languages count the whole file.
func TestSymbolClaimsCountAgainstTheirLines(t *testing.T) {
	b := newBoardEnv(t)
	b.land(b.alice, "Add limit", map[string]*string{"internal/limit/limit.go": s(limitBase), "web/limit.ts": s("export const a = 1\n")})
	b.claim(b.bobBE, "", "", "limit.Allow")
	b.claim(b.bobBE, "", "", "limit.Limiter")
	b.claim(b.tests, "", "", "limit.Missing")
	land := func(intent string, files map[string]*string) string {
		t.Helper()
		ws, err := b.svc.OpenWorkspace(b.ctx, b.agent, "shop-api", b.effort.Slug, intent)
		if err != nil {
			t.Fatal(err)
		}
		tree := b.snapshot(b.repo, ws.Workspace.Base, files)
		res, err := b.svc.Land(b.ctx, b.agent, service.LandInput{WorkspaceID: ws.Workspace.ID, Tree: tree})
		if err != nil || !res.Landed {
			t.Fatalf("land %s: %+v %v", intent, res, err)
		}
		var got string
		for _, c := range res.OverlappingClaims {
			got += c.Targets[0] + " "
		}
		return got
	}
	cur := limitBase
	replace := func(old, new string) *string { // each land builds on the last
		cur = stringsReplace(cur, old, new)
		return s(cur)
	}
	if got := land("Reset clears", map[string]*string{"internal/limit/limit.go": replace("func Reset(key string) {\n}", "func Reset(key string) {\n\tclear(key)\n}")}); got != "" {
		t.Fatalf("changing Reset counted against %q", got)
	}
	if got := land("Allow says no", map[string]*string{"internal/limit/limit.go": replace("return true", "return false")}); got != "limit.Allow " {
		t.Fatalf("changing Allow = %q", got)
	}
	if got := land("Allow doc", map[string]*string{"internal/limit/limit.go": replace("// Allow says whether key may go on.", "// Allow reports whether key may go on.")}); got != "limit.Allow " {
		t.Fatalf("changing Allow's doc = %q", got)
	}
	if got := land("Take checks", map[string]*string{"internal/limit/limit.go": replace("return l.n > 0", "return l.n >= 1")}); got != "limit.Limiter " {
		t.Fatalf("changing a method of Limiter = %q", got)
	}
	// Other languages: the whole file named like the package part.
	if got := land("ts", map[string]*string{"web/limit.ts": s("export const a = 2\n")}); got != "limit.Allow limit.Limiter limit.Missing " {
		t.Fatalf("changing limit.ts = %q", got)
	}
}

func stringsReplace(s, old, new string) string {
	i := len(s)
	for j := 0; j+len(old) <= len(s); j++ {
		if s[j:j+len(old)] == old {
			i = j
			break
		}
	}
	if i == len(s) {
		panic("no " + old)
	}
	return s[:i] + new + s[i+len(old):]
}
