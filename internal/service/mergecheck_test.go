package service_test

import (
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/service"
)

const muxGo = "package web\n\nfunc register(m *Mux) {\n\tm.Handle(\"a\")\n}\n\ntype Mux struct{}\n\nfunc (m *Mux) Handle(string) {}\n"

var (
	muxReturn = strings.Replace(muxGo, "\tm.Handle(\"a\")\n", "\tm.Handle(\"a\")\n\tm.Handle(\"b\")\n\treturn\n", 1)
	muxMore   = strings.Replace(muxGo, "\tm.Handle(\"a\")\n", "\tm.Handle(\"a\")\n\tm.Handle(\"c\")\n", 1)
)

// A land the server merges onto the effort warns about code the merge
// made unreachable, naming file and line, and still lands.
func TestLandWarnsUnreachableAfterMerge(t *testing.T) {
	e := newEnv(t)
	repo, eff := e.setup()
	e.land(repo, eff, "Add the mux", map[string]*string{"web/mux.go": s(muxGo)}, nil)
	eff = e.effort(eff)
	w1, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Register b, then stop")
	w2, _ := e.svc.OpenWorkspace(e.ctx, e.agent, repo.Name, eff.Slug, "Register c")
	if r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w1.Workspace.ID, Tree: e.snapshot(repo, w1.Workspace.Base, map[string]*string{"web/mux.go": s(muxReturn)})}); err != nil || !r.Landed || len(r.Warnings) != 0 {
		t.Fatalf("first land: %+v %v", r, err)
	}
	r, err := e.svc.Land(e.ctx, e.agent, service.LandInput{WorkspaceID: w2.Workspace.ID, Tree: e.snapshot(repo, w2.Workspace.Base, map[string]*string{"web/mux.go": s(muxMore)})})
	if err != nil || !r.Landed || !r.Rebased {
		t.Fatalf("second land: %+v %v", r, err)
	}
	var got []engine.Warning
	for _, w := range r.Warnings {
		if w.Kind == engine.WarningUnreachable {
			got = append(got, w)
		}
	}
	if len(got) != 1 || got[0].Detail != "web/mux.go:7: unreachable code in register after the merge (neither side had it; both changed this package)" {
		t.Fatalf("warnings = %+v", r.Warnings)
	}
	// Kept with the changeset.
	if ws := r.Changeset.Warnings; len(ws) == 0 || ws[len(ws)-1].Kind != string(engine.WarningUnreachable) {
		t.Fatalf("stored warnings = %+v", ws)
	}
}

// The overlap forecast of two efforts warns about the same before either
// ships.
func TestOverlapForecastUnreachable(t *testing.T) {
	e := newOverlapEnv(t, map[string]*string{"web/mux.go": s(muxGo)})
	e.landIn(e.a, "Register b, then stop", map[string]*string{"web/mux.go": s(muxReturn)})
	e.landIn(e.b, "Register c", map[string]*string{"web/mux.go": s(muxMore)})
	var found bool
	for _, o := range e.overlaps(e.a) {
		for _, w := range o.Warnings {
			if w.Kind == engine.WarningUnreachable || w.Kind == engine.WarningUndefined || w.Kind == engine.WarningUnusedImport {
				found = found || strings.Contains(w.Detail, "web/mux.go:") && strings.Contains(w.Detail, "unreachable code in register")
			}
		}
	}
	if !found {
		t.Fatalf("forecast = %+v", e.overlaps(e.a))
	}
}
