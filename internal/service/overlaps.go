package service

// The overlap forecast (#26): what happens when two open efforts of one
// repository both ship. After every land, and whenever a base branch
// moves, vx replays both efforts onto the branch head the way shipping
// would, records which files they share, which would not merge and what the
// combined tree breaks (duplicate Go declarations, vx.toml rules), and
// tells both boards when that changes.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

const (
	// DefaultForecastTimeout bounds one background forecast run (all pairs
	// of one effort, or of a repository after its branch moved).
	DefaultForecastTimeout = 2 * time.Minute
	// onDemandForecast bounds the recompute ListOverlaps does when the
	// stored forecast is stale; after it, the stored forecast is returned.
	onDemandForecast = 20 * time.Second
	// overlapNoteFiles is how many files an overlap note names.
	overlapNoteFiles = 10
)

// forecaster runs forecasts in the background, one run per repository at a
// time, coalescing requests that arrive while a run is busy.
type forecaster struct {
	mu      sync.Mutex
	queued  map[uuid.UUID]*forecastQueue // by repository
	wg      sync.WaitGroup
	vx      store.Principal // the service principal, once loaded
	hook    func()          // tests: runs before each background forecast
	running map[uuid.UUID]bool
}

type forecastQueue struct {
	repo    store.Repo
	efforts map[uuid.UUID]bool
	all     map[string]bool // base branches whose efforts all need a forecast
}

// scheduleForecast queues a forecast of effortID against the other open
// efforts of repo and returns at once.
func (s *Service) scheduleForecast(repo store.Repo, effortID uuid.UUID) {
	s.enqueueForecast(repo, func(q *forecastQueue) { q.efforts[effortID] = true })
}

// scheduleBranchForecast queues a forecast of every open effort of repo
// based on branch, after the branch head moved.
func (s *Service) scheduleBranchForecast(repo store.Repo, branch string) {
	s.enqueueForecast(repo, func(q *forecastQueue) { q.all[branch] = true })
}

func (s *Service) enqueueForecast(repo store.Repo, add func(*forecastQueue)) {
	f := &s.forecasts
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queued == nil {
		f.queued = map[uuid.UUID]*forecastQueue{}
		f.running = map[uuid.UUID]bool{}
	}
	q := f.queued[repo.ID]
	if q == nil {
		q = &forecastQueue{repo: repo, efforts: map[uuid.UUID]bool{}, all: map[string]bool{}}
		f.queued[repo.ID] = q
	}
	add(q)
	if f.running[repo.ID] {
		return // the running loop picks it up
	}
	f.running[repo.ID] = true
	f.wg.Add(1)
	go s.forecastLoop(repo.ID)
}

// forecastLoop drains a repository's queue. Errors are logged: a forecast
// never fails the land or sync that asked for it.
func (s *Service) forecastLoop(repoID uuid.UUID) {
	f := &s.forecasts
	defer f.wg.Done()
	for {
		f.mu.Lock()
		q := f.queued[repoID]
		delete(f.queued, repoID)
		if q == nil {
			f.running[repoID] = false
			f.mu.Unlock()
			return
		}
		hook := f.hook
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		timeout := s.ForecastTimeout
		if timeout <= 0 {
			timeout = DefaultForecastTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		run := s.newForecastRun()
		start, failed := time.Now(), false
		for branch := range q.all {
			if err := run.branch(ctx, q.repo, branch); err != nil {
				failed = true
				slog.Warn("overlap forecast", "repo", q.repo.Name, "branch", branch, "err", err)
			}
		}
		for id := range q.efforts {
			if err := run.effort(ctx, q.repo, id); err != nil {
				failed = true
				slog.Warn("overlap forecast", "repo", q.repo.Name, "effort", id, "err", err)
			}
		}
		cancel()
		recordForecast(time.Since(start), failed)
	}
}

// WaitForecasts blocks until the background forecasts queued so far have
// finished. Servers call it on shutdown; tests call it before looking.
func (s *Service) WaitForecasts() { s.forecasts.wg.Wait() }

// ForecastOverlaps recomputes the forecast between effort and every other
// open effort of its repository with the same base branch, and posts or
// updates overlap notes on both boards when something changed. Called
// after every land and when the base branch moves; it never fails a land
// (errors are logged). It is internal: no actor, no access check.
func (s *Service) ForecastOverlaps(ctx context.Context, repo store.Repo, effortID uuid.UUID) error {
	return s.newForecastRun().effort(ctx, repo, effortID)
}

// forecastRun caches what one run learns about each effort.
type forecastRun struct {
	s      *Service
	sides  map[uuid.UUID]*forecastSide
	owners map[uuid.UUID]store.Principal
}

func (s *Service) newForecastRun() *forecastRun {
	return &forecastRun{s: s, sides: map[uuid.UUID]*forecastSide{}, owners: map[uuid.UUID]store.Principal{}}
}

// forecastSide is one effort as the forecast sees it.
type forecastSide struct {
	eff       store.Effort
	chain     []store.ChangesetRow // newest first, back to the base; loaded when simulated
	changed   map[string]bool      // paths changed since the base tree
	dirs      map[string]bool      // their directories
	firstLand time.Time            // when its oldest vx change landed; with chain
}

func (r *forecastRun) side(ctx context.Context, repo store.Repo, eff store.Effort) (*forecastSide, error) {
	if sd, ok := r.sides[eff.ID]; ok && sd.eff.Head == eff.Head {
		return sd, nil
	}
	s := r.s
	sd := &forecastSide{eff: eff, changed: map[string]bool{}, dirs: map[string]bool{}}
	if eff.Head != eff.Base {
		changes, err := s.effortChanges(ctx, repo.ID, eff)
		if err != nil {
			return nil, err
		}
		for _, c := range changes {
			sd.changed[c.Path] = true
			sd.dirs[path.Dir(c.Path)] = true
		}
	}
	r.sides[eff.ID] = sd
	return sd, nil
}

// loadChain reads the side's changes back to its base, once, for a pair
// that needs a simulation.
func (r *forecastRun) loadChain(ctx context.Context, repo store.Repo, sd *forecastSide) error {
	if sd.chain != nil || sd.eff.Head == sd.eff.Base {
		return nil
	}
	chain, err := r.s.Store.Chain(ctx, repo.ID, sd.eff.Head, sd.eff.Base, 100000)
	if err != nil {
		return err
	}
	for _, c := range chain {
		if c.Origin == object.OriginVX {
			sd.firstLand = c.CreatedAt
		}
	}
	sd.chain = chain
	return nil
}

// branch forecasts every open effort of repo based on branch.
func (r *forecastRun) branch(ctx context.Context, repo store.Repo, branch string) error {
	effs, err := r.s.Store.ListEfforts(ctx, repo.ID, store.EffortOpen)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range effs {
		if e.BaseBranch == branch {
			errs = append(errs, r.effort(ctx, repo, e.ID))
		}
	}
	return errors.Join(errs...)
}

// effort forecasts one effort against every other open effort with the same
// base branch.
func (r *forecastRun) effort(ctx context.Context, repo store.Repo, effortID uuid.UUID) error {
	s := r.s
	eff, err := s.Store.EffortByID(ctx, effortID)
	if err != nil {
		return err
	}
	if err := s.Store.DeleteClosedOverlaps(ctx, eff.ID); err != nil {
		return err
	}
	if eff.State != store.EffortOpen {
		return nil
	}
	effs, err := s.Store.ListEfforts(ctx, repo.ID, store.EffortOpen)
	if err != nil {
		return err
	}
	var errs []error
	for _, other := range effs {
		if other.ID == eff.ID || other.BaseBranch != eff.BaseBranch {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.pair(ctx, repo, eff, other); err != nil {
			errs = append(errs, fmt.Errorf("%s and %s: %w", eff.Slug, other.Slug, err))
		}
	}
	return errors.Join(errs...)
}

// forecast is the computed outcome for one pair.
type forecast struct {
	status      string
	shared      []string
	conflicts   []string
	warnings    []engine.Warning
	targetHead  hash.Hash
	involvedOfA []string // files of each side named by warnings (no shared files case)
	involvedOfB []string
}

// pair computes and stores the forecast of two efforts and posts notes when
// it changed.
func (r *forecastRun) pair(ctx context.Context, repo store.Repo, x, y store.Effort) error {
	s := r.s
	a, b := x, y
	if _, _, swapped := store.OrderPair(x.ID, y.ID); swapped {
		a, b = y, x
	}
	branch, err := s.Store.GetBranch(ctx, repo.ID, a.BaseBranch)
	if err != nil {
		return err
	}
	stored, err := s.Store.GetOverlap(ctx, a.ID, b.ID)
	have := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if have && stored.AHead == a.Head.String() && stored.BHead == b.Head.String() && stored.TargetHead == branch.Head.String() {
		return nil // nothing moved since the stored forecast
	}
	sa, err := r.side(ctx, repo, a)
	if err != nil {
		return err
	}
	sb, err := r.side(ctx, repo, b)
	if err != nil {
		return err
	}
	fc, err := r.compute(ctx, repo, branch, sa, sb)
	if err != nil {
		return err
	}
	return r.save(ctx, repo, a, b, fc)
}

// compute simulates shipping both efforts: the one that landed first is
// replayed onto the branch head exactly as PlanShipment does, and the
// other's net change is merged onto that result.
func (r *forecastRun) compute(ctx context.Context, repo store.Repo, branch store.Branch, a, b *forecastSide) (forecast, error) {
	s := r.s
	fc := forecast{status: OverlapClean, targetHead: branch.Head}
	for p := range a.changed {
		if b.changed[p] {
			fc.shared = append(fc.shared, p)
		}
	}
	sort.Strings(fc.shared)
	near := false
	for d := range a.dirs {
		if b.dirs[d] {
			near = true
			break
		}
	}
	if len(fc.shared) == 0 && !near {
		return fc, nil // nothing in common: no simulation needed
	}
	target, err := s.Store.GetChangeset(ctx, repo.ID, branch.Head)
	if err != nil {
		return fc, err
	}
	if err := r.loadChain(ctx, repo, a); err != nil {
		return fc, err
	}
	if err := r.loadChain(ctx, repo, b); err != nil {
		return fc, err
	}
	first, second := a, b
	if b.firstLand.Before(a.firstLand) {
		first, second = b, a
	}
	r1, err := s.replayChain(ctx, repo.ID, first.chain, target.Tree, branch.Name, replayForecast)
	if err != nil {
		return fc, err
	}
	onto := r1.Tree
	if len(r1.Conflicts) > 0 || onto.IsZero() {
		// The first effort needs a rebase onto the branch anyway: forecast
		// the pair on its own head instead of the branch head.
		head, err := s.Store.GetChangeset(ctx, repo.ID, first.eff.Head)
		if err != nil {
			return fc, err
		}
		onto = head.Tree
	}
	// The second effort goes on top as one net change, base tree to head
	// tree: a line one of its changes edited and a later one put back is no
	// collision with the first effort, though a change-by-change replay
	// would stop there.
	secondBase, err := s.Store.GetChangeset(ctx, repo.ID, second.eff.Base)
	if err != nil {
		return fc, err
	}
	secondHead, err := s.Store.GetChangeset(ctx, repo.ID, second.eff.Head)
	if err != nil {
		return fc, err
	}
	r2, err := engine.MergeTreesKeepConflicts(ctx, s.Blobs, secondBase.Tree, secondHead.Tree, onto,
		merge.Labels{Base: "base", Ours: second.eff.Slug, Theirs: first.eff.Slug}, s.mergeOptions(ctx, secondHead.Tree, onto)...)
	if err != nil {
		return fc, err
	}
	shared := map[string]bool{}
	for _, p := range fc.shared {
		shared[p] = true
	}
	seen := map[string]bool{}
	for _, c := range r2.Conflicts {
		if shared[c.Path] && !seen[c.Path] {
			seen[c.Path] = true
			fc.conflicts = append(fc.conflicts, c.Path)
		}
	}
	sort.Strings(fc.conflicts)
	// Generated files both change are never text-merged: whoever ships
	// second regenerates them.
	for _, g := range r2.Generated {
		if shared[g.Path] && !seen[g.Path] {
			fc.warnings = append(fc.warnings, regenerateWarning(g))
			fc.involvedOfA, fc.involvedOfB = append(fc.involvedOfA, g.Path), append(fc.involvedOfB, g.Path)
		}
	}

	if combined := r2.Tree; !combined.IsZero() {
		union := make([]string, 0, len(a.changed)+len(b.changed))
		for p := range a.changed {
			union = append(union, p)
		}
		for p := range b.changed {
			if !a.changed[p] {
				union = append(union, p)
			}
		}
		sort.Strings(union)
		ws, err := engine.CheckGo(ctx, s.Blobs, combined, union)
		if err != nil {
			slog.Warn("overlap forecast: Go check", "repo", repo.Name, "err", err)
		}
		for _, w := range ws {
			if w.Kind != engine.WarningDuplicate {
				continue // parse errors belong to one side or to a conflict
			}
			files := make([]string, 0, len(w.Files))
			for _, f := range w.Files {
				files = append(files, path.Join(w.Path, f))
			}
			if inA, inB := involved(files, a.changed), involved(files, b.changed); len(inA) > 0 && len(inB) > 0 {
				fc.warnings = append(fc.warnings, w)
				fc.involvedOfA, fc.involvedOfB = append(fc.involvedOfA, inA...), append(fc.involvedOfB, inB...)
			}
		}
		// Signatures and names one effort changed that the other's new
		// code uses the old way.
		sws, err := engine.CheckGoSides(ctx, s.Blobs, engine.GoSides{
			Base: secondBase.Tree, Ours: secondHead.Tree, Theirs: onto, Merged: combined,
			OursName: "effort " + second.eff.Slug, TheirsName: "effort " + first.eff.Slug,
		})
		if err != nil {
			slog.Warn("overlap forecast: Go signatures", "repo", repo.Name, "err", err)
		}
		for _, w := range sws {
			if inA, inB := involved(w.Files, a.changed), involved(w.Files, b.changed); len(inA) > 0 && len(inB) > 0 {
				fc.warnings = append(fc.warnings, w)
				fc.involvedOfA, fc.involvedOfB = append(fc.involvedOfA, inA...), append(fc.involvedOfB, inB...)
			}
		}
		// Shared files the merge could only do declaration by declaration.
		for _, w := range engine.MergedByDeclWarnings(r2.MergedByDecl) {
			if shared[w.Path] {
				fc.warnings = append(fc.warnings, w)
				fc.involvedOfA, fc.involvedOfB = append(fc.involvedOfA, w.Path), append(fc.involvedOfB, w.Path)
			}
		}
		rules, err := s.RulesAt(ctx, combined)
		if err != nil {
			slog.Warn("overlap forecast: vx.toml", "repo", repo.Name, "err", err)
		} else {
			rws, err := s.CheckRules(ctx, rules, target.Tree, combined)
			if err != nil {
				slog.Warn("overlap forecast: rules", "repo", repo.Name, "err", err)
			}
			for _, w := range rws {
				files := w.Files
				if len(files) == 0 && w.Path != "" {
					files = []string{w.Path}
				}
				inA, inB := involved(files, a.changed), involved(files, b.changed)
				if len(inA) > 0 && len(inB) > 0 {
					fc.warnings = append(fc.warnings, w)
					fc.involvedOfA, fc.involvedOfB = append(fc.involvedOfA, inA...), append(fc.involvedOfB, inB...)
				}
			}
		}
	}
	switch {
	case len(fc.conflicts) > 0:
		fc.status = OverlapConflict
	case len(fc.shared) > 0 || len(fc.warnings) > 0:
		fc.status = OverlapShared
	}
	fc.involvedOfA, fc.involvedOfB = uniqueSorted(fc.involvedOfA), uniqueSorted(fc.involvedOfB)
	return fc, nil
}

func involved(files []string, changed map[string]bool) []string {
	var out []string
	for _, f := range files {
		if changed[f] {
			out = append(out, f)
		}
	}
	return out
}

func uniqueSorted(xs []string) []string {
	sort.Strings(xs)
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// save stores the forecast of a and b (ordered) and, when its status,
// conflicts or warnings changed, posts a note on both boards that
// supersedes the pair's previous one.
func (r *forecastRun) save(ctx context.Context, repo store.Repo, a, b store.Effort, fc forecast) error {
	s := r.s
	vx, err := s.systemPrincipal(ctx)
	if err != nil {
		return err
	}
	ownerA, err := r.owner(ctx, a.OwnerID)
	if err != nil {
		return err
	}
	ownerB, err := r.owner(ctx, b.OwnerID)
	if err != nil {
		return err
	}
	row := store.EffortOverlap{
		RepoID: repo.ID, EffortA: a.ID, EffortB: b.ID, Status: fc.status,
		SharedFiles: fc.shared, Conflicts: fc.conflicts, Warnings: storeWarnings(fc.warnings),
		AHead: a.Head.String(), BHead: b.Head.String(), TargetHead: fc.targetHead.String(), CheckedAt: s.Now(),
	}
	return s.Store.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.LockOverlapPair(ctx, a.ID, b.ID); err != nil {
			return err
		}
		prev, err := tx.GetOverlap(ctx, a.ID, b.ID)
		have := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if have {
			if prev.AHead == row.AHead && prev.BHead == row.BHead && prev.TargetHead == row.TargetHead {
				return nil // another run stored this forecast meanwhile
			}
			row.ANote, row.BNote = prev.ANote, prev.BNote
		} else {
			prev.Status = OverlapClean
		}
		if sameOutcome(prev, row) {
			return tx.SaveOverlap(ctx, row)
		}
		if row.Status == OverlapClean && prev.Status == OverlapClean {
			return tx.SaveOverlap(ctx, row)
		}
		post := func(on store.Effort, other store.Effort, otherOwner store.Principal, mine, theirs []string, prevNote *uuid.UUID) (*uuid.UUID, error) {
			body, targets := overlapNoteText(other, otherOwner, fc, mine, theirs)
			n, err := tx.InsertNote(ctx, store.Note{
				EffortID: on.ID, Kind: string(NoteOverlap), Targets: targets, AuthorID: vx.ID, Body: body,
				State: NoteOpen, CreatedAt: s.Now(), Supersedes: prevNote, System: true,
			})
			if err != nil {
				return nil, err
			}
			if err := tx.LogOp(ctx, &repo.ID, &vx.ID, "note.overlap", n.ID.String(), nil,
				map[string]any{"effort": on.Slug, "other": other.Slug, "status": fc.status, "conflicts": fc.conflicts}); err != nil {
				return nil, err
			}
			return &n.ID, nil
		}
		if row.ANote, err = post(a, b, ownerB, fc.involvedOfA, fc.involvedOfB, prev.ANote); err != nil {
			return err
		}
		if row.BNote, err = post(b, a, ownerA, fc.involvedOfB, fc.involvedOfA, prev.BNote); err != nil {
			return err
		}
		return tx.SaveOverlap(ctx, row)
	})
}

// sameOutcome reports whether two forecasts would tell people the same:
// status, conflicts and warnings (shared files alone do not make a note).
func sameOutcome(a, b store.EffortOverlap) bool {
	if a.Status != b.Status || strings.Join(a.Conflicts, "\x00") != strings.Join(b.Conflicts, "\x00") || len(a.Warnings) != len(b.Warnings) {
		return false
	}
	for i := range a.Warnings {
		wa, wb := a.Warnings[i], b.Warnings[i]
		if wa.Kind != wb.Kind || wa.Path != wb.Path || wa.Name != wb.Name || wa.Decl != wb.Decl || strings.Join(wa.Files, "\x00") != strings.Join(wb.Files, "\x00") {
			return false
		}
	}
	return true
}

// overlapNoteText is the note posted on one effort's board about other.
// mine and theirs are the files of each side that warnings name.
func overlapNoteText(other store.Effort, owner store.Principal, fc forecast, mine, theirs []string) (string, []string) {
	who := fmt.Sprintf("Effort %q (%s, %s)", other.Name, other.Slug, owner.Ref())
	var b strings.Builder
	targets := fc.shared
	switch {
	case fc.status == OverlapClean:
		fmt.Fprintf(&b, "Resolved: %s no longer changes the same files as this effort.", who)
		return finishNote(b.String()), nil
	case len(fc.shared) > 0:
		fmt.Fprintf(&b, "%s also changes %s.", who, fileList(fc.shared))
	default:
		fmt.Fprintf(&b, "%s changes %s, next to this effort's %s.", who, fileList(theirs), fileList(mine))
		targets = uniqueSorted(append(append([]string{}, mine...), theirs...))
	}
	regen, others := splitRegenerate(fc.warnings)
	switch {
	case len(fc.conflicts) > 0:
		fmt.Fprintf(&b, " Shipping both conflicts in %s.", fileList(fc.conflicts))
	case len(regen) > 0:
		// Not "merges cleanly": the generated files need regenerating.
		fmt.Fprintf(&b, " Shipping both: %s", regenerateText(regen))
		if len(regen) < len(fc.shared) {
			b.WriteString("; the other shared files merge")
		}
		if len(others) == 0 {
			b.WriteString(".")
		}
	default:
		b.WriteString(" Shipping both merges cleanly")
		if len(others) == 0 {
			b.WriteString(".")
		}
	}
	if len(fc.conflicts) > 0 && len(regen) > 0 {
		others = append([]engine.Warning{{Kind: engine.WarningGenerated, Detail: regenerateText(regen)}}, others...)
	}
	if len(others) > 0 {
		if len(fc.conflicts) > 0 {
			b.WriteString(" Also:")
		} else {
			b.WriteString(", but:")
		}
		for i, w := range others {
			if i > 0 {
				b.WriteString(";")
			}
			b.WriteString(" " + overlapWarningText(w))
		}
		b.WriteString(".")
	}
	if len(targets) > MaxNoteTargets {
		targets = targets[:MaxNoteTargets]
	}
	return finishNote(b.String()), targets
}

// regenerateWarning is a generated file both efforts change: never
// text-merged, so it has to be regenerated when the second one ships.
func regenerateWarning(g engine.GeneratedFile) engine.Warning {
	d := fmt.Sprintf("regenerate when shipping (%s)", g.Path)
	if g.Command != "" {
		d += fmt.Sprintf(" with `%s`", g.Command)
	}
	return engine.Warning{Path: g.Path, Kind: engine.WarningGenerated, Name: g.Command, Files: []string{g.Path}, Detail: d}
}

// splitRegenerate separates the generated-file warnings of a forecast from
// the others.
func splitRegenerate(ws []engine.Warning) (regen, others []engine.Warning) {
	for _, w := range ws {
		if w.Kind == engine.WarningGenerated && w.Path != "" {
			regen = append(regen, w)
		} else {
			others = append(others, w)
		}
	}
	return regen, others
}

// regenerateText says which generated files to regenerate, grouped by
// command: "regenerate when shipping (gen/a.go, gen/b.go) with `buf generate`".
func regenerateText(regen []engine.Warning) string {
	var order []string
	files := map[string][]string{}
	for _, w := range regen {
		if _, ok := files[w.Name]; !ok {
			order = append(order, w.Name)
		}
		files[w.Name] = append(files[w.Name], w.Path)
	}
	parts := make([]string, 0, len(order))
	for _, cmd := range order {
		p := fmt.Sprintf("regenerate when shipping (%s)", fileList(files[cmd]))
		if cmd != "" {
			p += fmt.Sprintf(" with `%s`", cmd)
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// overlapWarningText says what a warning of the combined tree means.
func overlapWarningText(w engine.Warning) string {
	if w.Kind == engine.WarningDuplicate {
		where := "package " + w.Path
		if w.Path == "." {
			where = "the root package"
		}
		return fmt.Sprintf("both declare %s %s in %s (%s)", w.Decl, w.Name, where, strings.Join(w.Files, ", "))
	}
	return strings.TrimSuffix(w.Detail, ".")
}

func fileList(files []string) string {
	if len(files) <= overlapNoteFiles {
		return strings.Join(files, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(files[:overlapNoteFiles], ", "), len(files)-overlapNoteFiles)
}

// finishNote keeps a generated note within the board's limits.
func finishNote(s string) string {
	s = stripControls(s)
	if utf8.RuneCountInString(s) <= MaxNoteBody {
		return s
	}
	rs := []rune(s)
	return string(rs[:MaxNoteBody-1]) + "…"
}

func (r *forecastRun) owner(ctx context.Context, id uuid.UUID) (store.Principal, error) {
	if p, ok := r.owners[id]; ok {
		return p, nil
	}
	p, err := r.s.Store.PrincipalByID(ctx, id)
	if err != nil {
		return p, err
	}
	r.owners[id] = p
	return p, nil
}

// systemPrincipal is the vx service principal that posts system notes.
func (s *Service) systemPrincipal(ctx context.Context) (store.Principal, error) {
	f := &s.forecasts
	f.mu.Lock()
	p := f.vx
	f.mu.Unlock()
	if p.ID != uuid.Nil {
		return p, nil
	}
	p, err := s.Store.PrincipalByHandle(ctx, SystemPrincipalHandle)
	if err != nil {
		return p, fmt.Errorf("the %s service principal: %w", SystemPrincipalHandle, err)
	}
	f.mu.Lock()
	f.vx = p
	f.mu.Unlock()
	return p, nil
}

// ListOverlaps returns the stored forecasts of an effort (read), or of every
// pair in the repository when effort is empty. A forecast older than the
// efforts' heads or the branch head is recomputed first, within a short
// time limit; past it the stored one is returned.
func (s *Service) ListOverlaps(ctx context.Context, actor Actor, repoName, effort string) ([]Overlap, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, RoleRead)
	if err != nil {
		return nil, err
	}
	fctx, cancel := context.WithTimeout(ctx, onDemandForecast)
	defer cancel()
	run := s.newForecastRun()
	if effort == "" {
		effs, err := s.Store.ListEfforts(ctx, repo.ID, store.EffortOpen)
		if err != nil {
			return nil, err
		}
		for _, e := range effs {
			if err := run.effort(fctx, repo, e.ID); err != nil {
				slog.Warn("overlap forecast on demand", "repo", repo.Name, "effort", e.Slug, "err", err)
			}
		}
		rows, err := s.Store.ListRepoOverlaps(ctx, repo.ID)
		if err != nil {
			return nil, err
		}
		return s.overlapViews(ctx, repo, uuid.Nil, rows)
	}
	eff, err := s.Store.GetEffort(ctx, repo.ID, effort)
	if err != nil {
		return nil, classify(err)
	}
	if err := run.effort(fctx, repo, eff.ID); err != nil {
		slog.Warn("overlap forecast on demand", "repo", repo.Name, "effort", eff.Slug, "err", err)
	}
	return s.storedOverlaps(ctx, repo, eff.ID)
}

// storedOverlaps returns an effort's stored forecasts without recomputing
// them. The caller has checked read access to repo.
func (s *Service) storedOverlaps(ctx context.Context, repo store.Repo, effortID uuid.UUID) ([]Overlap, error) {
	rows, err := s.Store.ListEffortOverlaps(ctx, effortID)
	if err != nil {
		return nil, err
	}
	return s.overlapViews(ctx, repo, effortID, rows)
}

// contextOverlaps returns an effort's stored forecasts that are not clean
// and, with targets, that share a file or warning with one of them. The
// caller has checked read access to repo.
func (s *Service) contextOverlaps(ctx context.Context, repo store.Repo, effortID uuid.UUID, targets []string) ([]Overlap, error) {
	all, err := s.storedOverlaps(ctx, repo, effortID)
	if err != nil {
		return nil, err
	}
	var out []Overlap
	for _, o := range all {
		if o.Status == OverlapClean {
			continue
		}
		if len(targets) > 0 {
			files := append([]string{}, o.SharedFiles...)
			for _, w := range o.Warnings {
				files = append(files, w.Files...)
				files = append(files, w.Path)
			}
			if !matchAny(files, targets) {
				continue
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// overlapViews turns stored pairs into Overlaps seen from effortID's side
// (uuid.Nil: from the side of effort_a).
func (s *Service) overlapViews(ctx context.Context, repo store.Repo, effortID uuid.UUID, rows []store.EffortOverlap) ([]Overlap, error) {
	efforts := map[uuid.UUID]store.Effort{}
	principals := map[uuid.UUID]store.Principal{}
	effortOf := func(id uuid.UUID) (store.Effort, error) {
		if e, ok := efforts[id]; ok {
			return e, nil
		}
		e, err := s.Store.EffortByID(ctx, id)
		efforts[id] = e
		return e, err
	}
	out := make([]Overlap, 0, len(rows))
	for _, row := range rows {
		mine, other := row.EffortA, row.EffortB
		if effortID != uuid.Nil && row.EffortB == effortID {
			mine, other = row.EffortB, row.EffortA
		}
		me, err := effortOf(mine)
		if err != nil {
			return nil, err
		}
		oe, err := effortOf(other)
		if err != nil {
			return nil, err
		}
		owner, ok := principals[oe.OwnerID]
		if !ok {
			if owner, err = s.Store.PrincipalByID(ctx, oe.OwnerID); err != nil {
				return nil, err
			}
			principals[oe.OwnerID] = owner
		}
		out = append(out, Overlap{
			RepoName: repo.Name, Effort: me, Other: oe, OtherOwner: owner, Status: row.Status,
			SharedFiles: row.SharedFiles, Conflicts: row.Conflicts, Warnings: engineWarnings(row.Warnings), CheckedAt: row.CheckedAt,
		})
	}
	return out, nil
}

// engineWarnings converts stored warnings back.
func engineWarnings(ws []store.Warning) []engine.Warning {
	out := make([]engine.Warning, 0, len(ws))
	for _, w := range ws {
		out = append(out, engine.Warning{Path: w.Path, Kind: engine.WarningKind(w.Kind), Name: w.Name, Decl: w.Decl, Files: w.Files, Detail: w.Detail})
	}
	return out
}

// EffortOverlaps returns an effort's stored forecasts that are not clean,
// without recomputing them (read). The web UI shows them.
func (s *Service) EffortOverlaps(ctx context.Context, actor Actor, repoName, slug string) ([]Overlap, error) {
	v, err := s.effortFor(ctx, actor, repoName, slug, RoleRead)
	if err != nil {
		return nil, err
	}
	return s.contextOverlaps(ctx, v.Repo, v.Effort.ID, nil)
}

// OverlapCounts returns, for each open effort of a repository the actor
// can read, how many other efforts it overlaps with and how many of those
// conflict (read). The web UI shows them as badges.
func (s *Service) OverlapCounts(ctx context.Context, actor Actor, repoName string) (map[uuid.UUID][2]int, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, RoleRead)
	if err != nil {
		return nil, err
	}
	return s.Store.OverlapCounts(ctx, repo.ID)
}
