package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/provider"
	"github.com/TerracodeBA/vx/internal/store"
)

// SplitMode says how an effort ships as several pull requests.
type SplitMode string

const (
	// SplitNone ships the effort as one pull request.
	SplitNone SplitMode = ""
	// SplitIndependent ships parts that merge in any order: changes that
	// touch a common file, or where one depends on another, stay in one
	// part, and every part replays onto the target on its own.
	SplitIndependent SplitMode = "independent"
	// SplitStacked ships an ordered list of parts, each based on the
	// previous part's branch.
	SplitStacked SplitMode = "stacked"
)

// ParseSplitMode reads "independent", "stacked" or "" (one pull request).
func ParseSplitMode(s string) (SplitMode, error) {
	switch m := SplitMode(strings.TrimSpace(strings.ToLower(s))); m {
	case SplitNone, SplitIndependent, SplitStacked:
		return m, nil
	}
	return "", errf(CodeInvalid, "split %q: use independent (parts merge in any order) or stacked (each part builds on the previous one)", s)
}

// ShipGroup is one part of a split: one branch and one pull request.
type ShipGroup struct {
	Part   int    // 1-based
	Name   string // from the intents, e.g. "add-limiter-middleware"
	Title  string // pull request title
	Branch string // vx/<effort>/<n>-<name>
	Base   string // the target branch, or the previous part's branch (stacked)
	Steps  []ShipStep
	Files  []string // paths the part changes on its base
	// Why says why changes share this part: a common file, depends_on.
	Why          []string
	Tree         hash.Hash // tree after the part, on top of its base
	Conflicts    []engine.Conflict
	ConflictedAt *store.ChangesetRow
	// Problem says why the part cannot ship as grouped; empty: it can.
	Problem  string
	Warnings []engine.Warning
	Body     string
	// Previous is the shipment this part updates, when shipped before.
	Previous *store.Shipment
}

// Commits returns the steps of the part that produce a commit.
func (g ShipGroup) Commits() []ShipStep {
	var out []ShipStep
	for _, s := range g.Steps {
		if !s.Skipped {
			out = append(out, s)
		}
	}
	return out
}

// PRRef names the part's pull request for the other parts' bodies: "#41"
// once it has a number, else its branch.
func (g ShipGroup) PRRef() string {
	if g.Previous != nil && g.Previous.ProviderRef != "" {
		return "#" + g.Previous.ProviderRef
	}
	return "`" + g.Branch + "`"
}

// SplitInput controls PlanSplit.
type SplitInput struct {
	Repo, Effort, Target string
	Mode                 SplitMode
	// Groups is a person's grouping: change ids (or unique prefixes) per
	// part, in order. Empty: vx suggests the grouping, keeping the parts
	// shipped before.
	Groups [][]string
}

// splitChange is one change of the effort as the split sees it.
type splitChange struct {
	row   store.ChangesetRow
	index int // position in the effort, oldest first
	files []store.FileChange
	paths map[string]bool
}

// PlanSplit plans shipping an effort as several pull requests (admin). The
// plan's Groups hold the parts; Problems say why the split cannot ship as
// grouped (OpenShipment refuses it then). The whole-effort fields of the
// plan (Steps, Conflicts, Warnings) are those of PlanShipment.
func (s *Service) PlanSplit(ctx context.Context, actor Actor, in SplitInput) (ShipPlan, error) {
	if in.Mode == SplitNone {
		return ShipPlan{}, errf(CodeInvalid, "a split needs a mode: independent or stacked")
	}
	if _, err := ParseSplitMode(string(in.Mode)); err != nil {
		return ShipPlan{}, err
	}
	plan, err := s.PlanShipment(ctx, actor, in.Repo, in.Effort, in.Target)
	if err != nil {
		return ShipPlan{}, err
	}
	plan.Split = in.Mode
	repoID := plan.Repo.ID

	// The changes to split: the effort's vx changes the target lacks.
	var changes []*splitChange
	byID := map[string]*splitChange{}
	for _, st := range plan.Steps {
		if st.Skipped {
			plan.Notes = append(plan.Notes, fmt.Sprintf("change %s (%s) is already on %s and is left out", st.Changeset.ChangeID, st.Changeset.Title(), plan.Target))
			continue
		}
		c := &splitChange{row: st.Changeset, index: len(changes), paths: map[string]bool{}}
		parent, err := s.Store.GetChangeset(ctx, repoID, st.Changeset.FirstParent())
		if err != nil {
			return ShipPlan{}, err
		}
		diff, err := object.DiffTrees(ctx, s.Blobs, parent.Tree, st.Changeset.Tree)
		if err != nil {
			return ShipPlan{}, err
		}
		for _, d := range diff {
			c.files = append(c.files, store.FileChange{Change: d})
			c.paths[d.Path] = true
		}
		changes = append(changes, c)
		byID[c.row.ChangeID] = c
	}
	if plan.ConflictedAt != nil {
		// The replay stopped: the changes after the conflict were not
		// looked at. Split them too, each part shows its own conflicts.
		if err := s.addUnreplayed(ctx, &plan, &changes, byID); err != nil {
			return ShipPlan{}, err
		}
	}
	if len(changes) == 0 {
		return ShipPlan{}, errf(CodeFailedPrecondition, "nothing to ship: %s already has every change of %s", plan.Target, plan.Effort.Slug)
	}

	prev, err := s.Store.ListShipments(ctx, plan.Effort.ID)
	if err != nil {
		return ShipPlan{}, err
	}
	var live []store.Shipment // open parts of an earlier split, in part order
	for _, sh := range prev {
		if !shipmentOpen(sh.State) {
			continue
		}
		if sh.Split == "" && sh.Branch == "vx/"+plan.Effort.Slug {
			ref := sh.Branch
			if sh.ProviderRef != "" {
				ref = "#" + sh.ProviderRef + " from " + sh.Branch
			}
			plan.Problems = append(plan.Problems, fmt.Sprintf(
				"the effort is shipped as one pull request (%s), and git cannot hold %s next to %s/1-...: close that pull request and delete the branch on the host, then ship the split", ref, sh.Branch, sh.Branch))
			continue
		}
		if sh.Split != "" {
			live = append(live, sh)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].Part < live[j].Part })

	edges := s.splitEdges(ctx, changes)
	var groups [][]*splitChange
	var prevOf []*store.Shipment
	if len(in.Groups) > 0 {
		if groups, err = explicitGroups(in.Groups, changes); err != nil {
			return ShipPlan{}, err
		}
		prevOf = matchPrevious(groups, live, in.Mode)
	} else {
		plan.Suggested = true
		groups, prevOf = suggestGroups(in.Mode, changes, edges, live)
	}

	// Names, branches and bases.
	used := map[string]bool{}
	for i, g := range groups {
		sg := ShipGroup{Part: i + 1, Previous: prevOf[i]}
		if p := sg.Previous; p != nil {
			sg.Branch = p.Branch
			sg.Name = strings.TrimPrefix(p.Branch, "vx/"+plan.Effort.Slug+"/")
			if k := strings.IndexByte(sg.Name, '-'); k > 0 {
				if _, err := strconv.Atoi(sg.Name[:k]); err == nil {
					sg.Name = sg.Name[k+1:]
				}
			}
		} else {
			sg.Name = store.Slugify(g[0].row.Title())
			for n := 2; used[sg.Name]; n++ {
				sg.Name = fmt.Sprintf("%s-%d", store.Slugify(g[0].row.Title()), n)
			}
			sg.Branch = fmt.Sprintf("vx/%s/%d-%s", plan.Effort.Slug, sg.Part, sg.Name)
		}
		used[sg.Name] = true
		sg.Title = groupTitle(g)
		sg.Base = plan.Target
		if in.Mode == SplitStacked && i > 0 {
			sg.Base = plan.Groups[i-1].Branch
		}
		sg.Why = groupWhy(g, edges)
		plan.Groups = append(plan.Groups, sg)
	}
	// A new part pushed to the branch of an open earlier part (another
	// mode, another grouping) updates that part's pull request.
	for i := range plan.Groups {
		g := &plan.Groups[i]
		if g.Previous != nil {
			continue
		}
		for k := range live {
			taken := false
			for _, o := range plan.Groups {
				if o.Previous != nil && o.Previous.ID == live[k].ID {
					taken = true
				}
			}
			if !taken && live[k].Branch == g.Branch {
				sh := live[k]
				g.Previous = &sh
			}
		}
	}
	for _, sh := range live {
		kept := false
		for _, g := range plan.Groups {
			if g.Previous != nil && g.Previous.ID == sh.ID {
				kept = true
			}
		}
		if !kept {
			plan.Stale = append(plan.Stale, sh)
		}
	}

	// Replay every part: independent ones onto the target, stacked ones
	// onto the part before.
	partOf := map[string]int{}
	for i, g := range groups {
		for _, c := range g {
			partOf[c.row.ChangeID] = i
		}
	}
	onto := plan.TargetHead.Tree
	for i, g := range groups {
		sg := &plan.Groups[i]
		base := plan.TargetHead.Tree
		if in.Mode == SplitStacked {
			base = onto
		}
		rp, err := s.replayChain(ctx, repoID, chainOf(g), base, sg.Base, replayShip)
		if err != nil {
			return ShipPlan{}, err
		}
		sg.Steps, sg.Conflicts, sg.ConflictedAt, sg.Tree = rp.Steps, rp.Conflicts, rp.ConflictedAt, rp.Tree
		if len(sg.Conflicts) > 0 {
			sg.Problem = s.conflictReason(in.Mode, plan.Target, *sg, groups, partOf, byID)
		} else {
			diff, err := object.DiffTrees(ctx, s.Blobs, base, rp.Tree)
			if err != nil {
				return ShipPlan{}, err
			}
			for _, d := range diff {
				sg.Files = append(sg.Files, d.Path)
			}
			sg.Warnings = s.treeWarnings(ctx, rp.Tree, diff)
			sg.Warnings = append(sg.Warnings, engine.GeneratedWarnings(uniqueGenerated(rp.Generated))...)
			sg.Warnings = append(sg.Warnings, s.ruleWarnings(ctx, base, rp.Tree)...)
		}
		onto = rp.Tree
	}

	// depends_on across parts: independent parts may not depend on each
	// other; a stacked part may only depend on parts below it.
	for _, e := range edges {
		if e.kind != edgeDepends {
			continue
		}
		pa, pb := partOf[e.a.row.ChangeID], partOf[e.b.row.ChangeID]
		if pa == pb || (in.Mode == SplitStacked && pa < pb) {
			continue
		}
		sg := &plan.Groups[pb]
		if sg.Problem != "" {
			continue
		}
		if in.Mode == SplitStacked {
			sg.Problem = fmt.Sprintf("change %s (%s) depends on %s, which change %s (%s) in part %d above it changes: stack part %d first or put them in one part",
				e.b.row.ChangeID, e.b.row.Title(), e.what, e.a.row.ChangeID, e.a.row.Title(), pa+1, pa+1)
		} else {
			sg.Problem = fmt.Sprintf("change %s (%s) depends on %s, which change %s (%s) in part %d changes: independent parts must not depend on each other; put them in one part or ship --split stacked",
				e.b.row.ChangeID, e.b.row.Title(), e.what, e.a.row.ChangeID, e.a.row.Title(), pa+1)
		}
	}

	// Independent parts that share a file must merge cleanly in either
	// order: replay each onto the other.
	if in.Mode == SplitIndependent {
		for i := range plan.Groups {
			for j := i + 1; j < len(plan.Groups); j++ {
				a, b := &plan.Groups[i], &plan.Groups[j]
				if a.Problem != "" || b.Problem != "" {
					continue
				}
				shared := sharedPaths(a.Files, b.Files)
				if len(shared) == 0 {
					continue
				}
				for _, order := range [][2]int{{i, j}, {j, i}} {
					first, second := order[0], order[1]
					rp, err := s.replayChain(ctx, repoID, chainOf(groups[second]), plan.Groups[first].Tree, plan.Target, replayForecast)
					if err != nil {
						return ShipPlan{}, err
					}
					if len(rp.Conflicts) > 0 {
						b.Problem = fmt.Sprintf("parts %d and %d both change %s and conflict once one of them is merged: put them in one part, or ship --split stacked",
							i+1, j+1, strings.Join(shared, ", "))
						break
					}
				}
			}
		}
	}

	for i := range plan.Groups {
		if p := plan.Groups[i].Problem; p != "" {
			plan.Problems = append(plan.Problems, fmt.Sprintf("part %d (%s): %s", i+1, plan.Groups[i].Name, p))
		}
	}
	for i := range plan.Groups {
		plan.Groups[i].Body = partBody(plan, i, nil)
	}
	return plan, nil
}

// addUnreplayed adds the effort's changes after the one where the
// whole-effort replay stopped, so every change is in some part.
func (s *Service) addUnreplayed(ctx context.Context, plan *ShipPlan, changes *[]*splitChange, byID map[string]*splitChange) error {
	chain, err := s.Store.Chain(ctx, plan.Repo.ID, plan.Effort.Head, plan.Effort.Base, 10000)
	if err != nil {
		return err
	}
	for i := len(chain) - 1; i >= 0; i-- {
		c := chain[i]
		if c.Origin != object.OriginVX || byID[c.ChangeID] != nil {
			continue
		}
		parent, err := s.Store.GetChangeset(ctx, plan.Repo.ID, c.FirstParent())
		if err != nil {
			return err
		}
		diff, err := object.DiffTrees(ctx, s.Blobs, parent.Tree, c.Tree)
		if err != nil {
			return err
		}
		sc := &splitChange{row: c, index: len(*changes), paths: map[string]bool{}}
		for _, d := range diff {
			sc.files = append(sc.files, store.FileChange{Change: d})
			sc.paths[d.Path] = true
		}
		*changes = append(*changes, sc)
		byID[c.ChangeID] = sc
	}
	return nil
}

func shipmentOpen(state string) bool {
	return state == "open" || state == "draft" || state == "pushed"
}

// chainOf returns a part's changes as replayChain takes them: newest first.
func chainOf(g []*splitChange) []store.ChangesetRow {
	out := make([]store.ChangesetRow, len(g))
	for i, c := range g {
		out[len(g)-1-i] = c.row
	}
	return out
}

type edgeKind int

const (
	edgeFile edgeKind = iota
	edgeDepends
)

// splitEdge says why change b must ship with (or after) change a.
type splitEdge struct {
	a, b *splitChange // a is the older change
	kind edgeKind
	what string // the shared path, or the depends_on target
}

// splitEdges finds what ties the effort's changes together: a common file,
// or a depends_on of the newer change that names the older one or a
// symbol or path the older one changes.
func (s *Service) splitEdges(ctx context.Context, changes []*splitChange) []splitEdge {
	var out []splitEdge
	for j, b := range changes {
		for i := 0; i < j; i++ {
			a := changes[i]
			var shared []string
			for p := range b.paths {
				if a.paths[p] {
					shared = append(shared, p)
				}
			}
			sort.Strings(shared)
			if len(shared) > 0 {
				out = append(out, splitEdge{a: a, b: b, kind: edgeFile, what: strings.Join(shared, ", ")})
			}
		}
		for _, dep := range b.row.DependsOn {
			for i := 0; i < j; i++ {
				a := changes[i]
				hit := dep == a.row.ChangeID || (len(dep) >= 4 && strings.HasPrefix(a.row.ChangeID, dep))
				if !hit {
					touched, err := newLandTouches(ctx, s.Blobs, a.files).touches([]string{dep})
					hit = err == nil && touched
				}
				if hit {
					out = append(out, splitEdge{a: a, b: b, kind: edgeDepends, what: dep})
				}
			}
		}
	}
	return out
}

// suggestGroups is vx's grouping. Independent: changes tied by an edge
// share a part (union-find), parts ordered by their oldest change.
// Stacked: one part per change, in the effort's order. Parts shipped
// before are kept, so shipping again updates the same branches and pull
// requests; new changes join the part they are tied to, or a new part.
func suggestGroups(mode SplitMode, changes []*splitChange, edges []splitEdge, live []store.Shipment) ([][]*splitChange, []*store.Shipment) {
	parent := make([]int, len(changes))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra > rb {
			ra, rb = rb, ra
		}
		parent[rb] = ra
	}
	// Seed: changes shipped together before stay together.
	byID := map[string]int{}
	for i, c := range changes {
		byID[c.row.ChangeID] = i
	}
	seedOf := map[int]int{} // change index -> index into live
	for k, sh := range live {
		if SplitMode(sh.Split) != mode {
			continue
		}
		first := -1
		for _, ch := range sh.Changes {
			i, ok := byID[ch.ChangeID]
			if !ok {
				continue
			}
			seedOf[i] = k
			if first < 0 {
				first = i
			} else {
				union(first, i)
			}
		}
	}
	if mode == SplitIndependent {
		for _, e := range edges {
			// Two changes shipped before stay where they were (a person
			// may have split them on purpose); a new change joins what it
			// is tied to.
			_, sa := seedOf[e.a.index]
			_, sb := seedOf[e.b.index]
			if !sa || !sb {
				union(e.a.index, e.b.index)
			}
		}
	}
	roots := map[int]int{} // root -> group index
	var groups [][]*splitChange
	for i, c := range changes {
		r := find(i)
		if mode == SplitStacked {
			if _, seeded := seedOf[i]; !seeded {
				r = -1 - i // a change of its own
			}
		}
		gi, ok := roots[r]
		if !ok {
			gi = len(groups)
			roots[r] = gi
			groups = append(groups, nil)
		}
		groups[gi] = append(groups[gi], c)
	}
	prevOf := make([]*store.Shipment, len(groups))
	taken := map[int]bool{}
	for gi, g := range groups {
		for _, c := range g {
			if k, ok := seedOf[c.index]; ok && !taken[k] {
				taken[k] = true
				sh := live[k]
				prevOf[gi] = &sh
				break
			}
		}
	}
	if mode == SplitStacked {
		// Parts shipped before stay below new ones, in their old order.
		idx := make([]int, len(groups))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			pa, pb := prevOf[idx[a]], prevOf[idx[b]]
			switch {
			case pa != nil && pb != nil:
				return pa.Part < pb.Part
			case pa != nil:
				return true
			}
			return false
		})
		g2 := make([][]*splitChange, len(groups))
		p2 := make([]*store.Shipment, len(groups))
		for i, k := range idx {
			g2[i], p2[i] = groups[k], prevOf[k]
		}
		groups, prevOf = g2, p2
	}
	return groups, prevOf
}

// explicitGroups resolves a person's grouping. Every change of the effort
// must be in exactly one part; within a part, changes keep the effort's
// order.
func explicitGroups(in [][]string, changes []*splitChange) ([][]*splitChange, error) {
	seen := map[string]int{}
	var out [][]*splitChange
	for gi, ids := range in {
		var g []*splitChange
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			var match []*splitChange
			for _, c := range changes {
				if c.row.ChangeID == id {
					match = []*splitChange{c}
					break
				}
				if len(id) >= 4 && strings.HasPrefix(c.row.ChangeID, id) {
					match = append(match, c)
				}
			}
			switch len(match) {
			case 0:
				return nil, errf(CodeInvalid, "part %d names change %s, which is not a change of this effort left to ship (see vx ship plan --split)", gi+1, id)
			case 1:
			default:
				return nil, errf(CodeInvalid, "change id prefix %s is ambiguous: give more characters", id)
			}
			c := match[0]
			if p, dup := seen[c.row.ChangeID]; dup {
				return nil, errf(CodeInvalid, "change %s is in part %d and part %d: put it in one", c.row.ChangeID, p+1, len(out)+1)
			}
			seen[c.row.ChangeID] = len(out)
			g = append(g, c)
		}
		if len(g) == 0 {
			continue
		}
		sort.SliceStable(g, func(a, b int) bool { return g[a].index < g[b].index })
		out = append(out, g)
	}
	for _, c := range changes {
		if _, ok := seen[c.row.ChangeID]; !ok {
			return nil, errf(CodeInvalid, "change %s (%s) is in no part: add it to one", c.row.ChangeID, c.row.Title())
		}
	}
	return out, nil
}

// matchPrevious pairs a person's parts with the parts shipped before, so
// their branches and pull requests are reused: part n takes the earlier
// part n of the same mode when they share a change, or else the first
// earlier part it shares a change with.
func matchPrevious(groups [][]*splitChange, live []store.Shipment, mode SplitMode) []*store.Shipment {
	out := make([]*store.Shipment, len(groups))
	taken := map[int]bool{}
	shares := func(g []*splitChange, sh store.Shipment) bool {
		for _, c := range g {
			for _, ch := range sh.Changes {
				if ch.ChangeID == c.row.ChangeID {
					return true
				}
			}
		}
		return false
	}
	for pass := 0; pass < 2; pass++ {
		for gi, g := range groups {
			if out[gi] != nil {
				continue
			}
			for k, sh := range live {
				if taken[k] || SplitMode(sh.Split) != mode || !shares(g, sh) {
					continue
				}
				if pass == 0 && sh.Part != gi+1 {
					continue
				}
				taken[k] = true
				s := sh
				out[gi] = &s
				break
			}
		}
	}
	return out
}

func groupTitle(g []*splitChange) string {
	t := g[0].row.Title()
	if len(g) > 1 {
		t += fmt.Sprintf(" (+%d more)", len(g)-1)
	}
	return t
}

// groupWhy explains why a part's changes are together.
func groupWhy(g []*splitChange, edges []splitEdge) []string {
	in := map[*splitChange]bool{}
	for _, c := range g {
		in[c] = true
	}
	var out []string
	for _, e := range edges {
		if !in[e.a] || !in[e.b] {
			continue
		}
		switch e.kind {
		case edgeFile:
			out = append(out, fmt.Sprintf("%s and %s both change %s", e.a.row.ChangeID, e.b.row.ChangeID, e.what))
		case edgeDepends:
			out = append(out, fmt.Sprintf("%s depends on %s, which %s changes", e.b.row.ChangeID, e.what, e.a.row.ChangeID))
		}
	}
	return out
}

// conflictReason explains a part whose replay conflicted: a change that
// builds on lines another part wrote, or a change that conflicts with the
// target itself.
func (s *Service) conflictReason(mode SplitMode, target string, g ShipGroup, groups [][]*splitChange, partOf map[string]int, byID map[string]*splitChange) string {
	at := g.ConflictedAt
	paths := map[string]bool{}
	var names []string
	for _, cf := range g.Conflicts {
		if !paths[cf.Path] {
			names = append(names, cf.Path)
		}
		paths[cf.Path] = true
	}
	me := byID[at.ChangeID]
	if me != nil {
		// The older changes of other parts that touch the same files and
		// are not below this part, oldest first.
		var cands []*splitChange
		for _, c := range byID {
			cands = append(cands, c)
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].index < cands[j].index })
		for _, c := range cands {
			if c.index >= me.index || partOf[c.row.ChangeID] == g.Part-1 {
				continue
			}
			if mode == SplitStacked && partOf[c.row.ChangeID] < g.Part-1 {
				continue
			}
			var hit []string
			for _, p := range names {
				if c.paths[p] {
					hit = append(hit, p)
				}
			}
			if len(hit) == 0 {
				continue
			}
			other := partOf[c.row.ChangeID] + 1
			if mode == SplitStacked {
				return fmt.Sprintf("change %s (%s) builds on lines that change %s (%s) wrote in %s, which is in part %d above it: stack part %d first or put them in one part",
					at.ChangeID, at.Title(), c.row.ChangeID, c.row.Title(), strings.Join(hit, ", "), other, other)
			}
			return fmt.Sprintf("change %s (%s) builds on lines that change %s (%s) wrote in %s, which is in part %d: put them in one part",
				at.ChangeID, at.Title(), c.row.ChangeID, c.row.Title(), strings.Join(hit, ", "), other)
		}
	}
	return fmt.Sprintf("change %s (%s) conflicts with %s in %s: sync or rebase the effort onto %s first (vx effort rebase)",
		at.ChangeID, at.Title(), target, strings.Join(names, ", "), target)
}

func sharedPaths(a, b []string) []string {
	in := map[string]bool{}
	for _, p := range a {
		in[p] = true
	}
	var out []string
	for _, p := range b {
		if in[p] {
			out = append(out, p)
		}
	}
	return out
}

// partBody is the pull request body of part i: which part of how many,
// what it depends on, the other parts, then the usual body for its
// changes. refs overrides the parts' references ("#41") where known.
func partBody(p ShipPlan, i int, refs map[int]string) string {
	g := p.Groups[i]
	ref := func(k int) string {
		if r, ok := refs[k]; ok && r != "" {
			return r
		}
		return p.Groups[k].PRRef()
	}
	var b strings.Builder
	n := len(p.Groups)
	fmt.Fprintf(&b, "**Part %d of %d** of vx effort `%s`", i+1, n, p.Effort.Slug)
	switch {
	case p.Split == SplitStacked && i > 0:
		fmt.Fprintf(&b, ", stacked: depends on %s. Merge the parts in order; this one shows only its own commits on top of `%s`.\n\n", ref(i-1), g.Base)
	case p.Split == SplitStacked:
		b.WriteString(", stacked: merge it first, then the parts above it in order.\n\n")
	default:
		b.WriteString(". The parts are independent: they merge in any order.\n\n")
	}
	for k, o := range p.Groups {
		line := fmt.Sprintf("%d. %s %s", k+1, ref(k), o.Title)
		if k == i {
			line = fmt.Sprintf("%d. %s (this pull request)", k+1, o.Title)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	gp := p
	gp.Steps, gp.Warnings, gp.Branch = g.Steps, g.Warnings, g.Branch
	b.WriteString(prBody(gp))
	return b.String()
}

// ShipPart is one pushed part of a split.
type ShipPart struct {
	Group    ShipGroup
	Shipment store.Shipment
	PR       provider.OpenedPR
}

// openSplit pushes one branch per part and opens or updates one pull
// request per part. Stacked parts target the previous part's branch.
// OpenShipment checked admin and the git bridge.
func (s *Service) openSplit(ctx context.Context, actor Actor, in ShipInput) (ShipResult, error) {
	if in.Title != "" || in.Body != "" {
		return ShipResult{}, errf(CodeInvalid, "a title or body is for one pull request; with --split every part gets its own from its changes")
	}
	plan, err := s.PlanSplit(ctx, actor, SplitInput{Repo: in.Repo, Effort: in.Effort, Target: in.Target, Mode: in.Split, Groups: in.Groups})
	if err != nil {
		return ShipResult{}, err
	}
	res := ShipResult{Plan: plan}
	if len(plan.Problems) > 0 {
		return res, errf(CodeFailedPrecondition, "this split cannot ship: %s", strings.Join(plan.Problems, "; "))
	}
	// Push every branch first: a stacked pull request needs its base.
	heads := make([]string, len(plan.Groups))
	bases := make([]string, len(plan.Groups))
	shas := make([][]string, len(plan.Groups))
	for i, g := range plan.Groups {
		base := plan.TargetHead.GitSHA
		if plan.Split == SplitStacked && i > 0 {
			base = heads[i-1]
		}
		commits := make([]provider.Commit, 0, len(g.Steps))
		for _, st := range g.Commits() {
			c := st.Changeset
			commits = append(commits, provider.Commit{
				Tree: st.Tree, Message: commitMessage(plan.Effort.Slug, c.Changeset),
				AuthorName: c.Author.Name, AuthorEmail: c.Author.Email, When: c.CreatedAt,
			})
		}
		out, err := s.Shipper.ExportBranch(ctx, plan.Repo, base, g.Branch, commits)
		if err != nil {
			return res, errf(CodeUnavailable, "push %s (part %d of %d): %v", g.Branch, i+1, len(plan.Groups), err)
		}
		bases[i], shas[i], heads[i] = base, out, out[len(out)-1]
	}
	// Open the pull requests in order, then fill in the numbers the
	// earlier bodies could not know yet.
	refs := map[int]string{}
	sent := make([]string, len(plan.Groups))
	prs := make([]provider.OpenedPR, len(plan.Groups))
	hasPRs := true
	for i, g := range plan.Groups {
		body := partBody(plan, i, refs)
		pr, err := s.Shipper.OpenPullRequest(ctx, plan.Repo, provider.PullRequest{
			Head: g.Branch, Base: g.Base, Title: g.Title, Body: body, Draft: in.Draft,
		})
		switch {
		case errors.Is(err, provider.ErrNoPullRequests):
			hasPRs = false
		case err != nil:
			return res, errf(CodeUnavailable, "branches were pushed, but opening the pull request of part %d (%s) failed: %v", i+1, g.Branch, err)
		}
		prs[i], sent[i] = pr, body
		if pr.Number > 0 {
			refs[i] = "#" + strconv.Itoa(pr.Number)
		}
	}
	if hasPRs {
		for i, g := range plan.Groups {
			body := partBody(plan, i, refs)
			if body == sent[i] {
				continue
			}
			if _, err := s.Shipper.OpenPullRequest(ctx, plan.Repo, provider.PullRequest{
				Head: g.Branch, Base: g.Base, Title: g.Title, Body: body, Draft: in.Draft,
			}); err != nil {
				return res, errf(CodeUnavailable, "pull requests were opened, but listing the other parts in #%d failed: %v", prs[i].Number, err)
			}
			sent[i] = body
		}
	}
	for i := range plan.Groups {
		plan.Groups[i].Body = sent[i]
	}
	res.Plan = plan
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		for i, g := range plan.Groups {
			state := "pushed"
			if hasPRs {
				state = "open"
				if in.Draft {
					state = "draft"
				}
			}
			sh := store.Shipment{
				EffortID: plan.Effort.ID, Title: g.Title, Branch: g.Branch, Target: g.Base,
				BaseGitSHA: bases[i], HeadGitSHA: heads[i], URL: prs[i].URL, State: state,
				Split: string(plan.Split), Part: g.Part, CreatedBy: actor.ID,
			}
			if prs[i].Number > 0 {
				sh.ProviderRef = strconv.Itoa(prs[i].Number)
			}
			// The trees of the pushed commits map review comments on lines
			// to changes (feedback_blame.go).
			sh.BaseTree = plan.TargetHead.Tree
			if plan.Split == SplitStacked && i > 0 {
				sh.BaseTree = plan.Groups[i-1].Tree
			}
			for k, st := range g.Commits() {
				sh.Changes = append(sh.Changes, store.ShipmentChange{Position: k, Changeset: st.Changeset.Hash, ChangeID: st.Changeset.ChangeID, GitSHA: shas[i][k], Tree: st.Tree})
			}
			saved, err := tx.SaveShipment(ctx, sh)
			if err != nil {
				return err
			}
			res.Parts = append(res.Parts, ShipPart{Group: g, Shipment: saved, PR: prs[i]})
		}
		// Parts of an earlier split this one no longer has: vx stops
		// updating them; their pull requests are for a person to close.
		for _, sh := range plan.Stale {
			if err := tx.SetShipmentState(ctx, sh.ID, "closed"); err != nil {
				return err
			}
		}
		var parts []string
		for _, p := range res.Parts {
			parts = append(parts, p.Shipment.Branch)
		}
		return tx.LogOp(ctx, &plan.Repo.ID, &actor.ID, "ship", plan.Effort.Slug, nil,
			map[string]any{"split": string(plan.Split), "parts": parts, "target": plan.Target})
	})
	if err != nil {
		return res, err
	}
	res.Shipment, res.PR = res.Parts[0].Shipment, res.Parts[0].PR
	for _, p := range res.Parts {
		if p.PR.Number > 0 {
			s.postEvidenceCheck(ctx, ShipPlan{Repo: plan.Repo, Effort: plan.Effort, Steps: p.Group.Steps, Warnings: p.Group.Warnings}, p.Shipment.HeadGitSHA, p.PR.URL)
		}
	}
	return res, nil
}

// markMergedShipments marks a shipment merged once branch holds every
// change it shipped, so each part of a split knows its own state before
// the whole effort is merged.
func (s *Service) markMergedShipments(ctx context.Context, repo store.Repo, branch string) {
	b, err := s.Store.GetBranch(ctx, repo.ID, branch)
	if err != nil {
		return
	}
	effs, err := s.Store.ListEfforts(ctx, repo.ID, "")
	if err != nil {
		return
	}
	for _, e := range effs {
		if e.BaseBranch != branch || (e.State != store.EffortOpen && e.State != store.EffortShipping) {
			continue
		}
		list, err := s.Store.ListShipments(ctx, e.ID)
		if err != nil {
			continue
		}
		var onBranch map[string]bool // change ids on the branch since the effort's base
		for _, sh := range list {
			if sh.Split == "" || !shipmentOpen(sh.State) || len(sh.Changes) == 0 {
				continue
			}
			if onBranch == nil {
				onBranch = map[string]bool{}
				upstream, err := s.Store.Chain(ctx, repo.ID, b.Head, e.Base, 5000)
				if err != nil {
					break
				}
				for _, c := range upstream {
					for _, m := range changeIDTrailer.FindAllStringSubmatch(c.Intent, -1) {
						onBranch[m[1]] = true
					}
				}
			}
			all := true
			for _, ch := range sh.Changes {
				if !onBranch[ch.ChangeID] {
					all = false
					break
				}
			}
			if all {
				_ = s.Store.SetShipmentState(ctx, sh.ID, "merged")
			}
		}
	}
}

// markShipmentMergedByHead handles a merged pull request from head (a vx/
// branch), for merges that lose the change-id trailers (squash): the
// shipment pushed to head is merged, and its effort too once no part of it
// is still open. It reports whether the effort was closed.
func (s *Service) markShipmentMergedByHead(ctx context.Context, repo store.Repo, head, how string) (bool, error) {
	sh, err := s.Store.ShipmentByBranch(ctx, repo.ID, head)
	if errors.Is(err, store.ErrNotFound) {
		// Not shipped by this server (or before shipments were kept):
		// vx/<slug> names the effort.
		e, err := s.Store.GetEffort(ctx, repo.ID, strings.TrimPrefix(head, "vx/"))
		if err != nil || (e.State != store.EffortOpen && e.State != store.EffortShipping) {
			return false, nil
		}
		return true, s.closeEffort(ctx, nil, repo, e, store.EffortMerged, how)
	}
	if err != nil {
		return false, err
	}
	if err := s.Store.SetShipmentState(ctx, sh.ID, "merged"); err != nil {
		return false, err
	}
	effs, err := s.Store.ListEfforts(ctx, repo.ID, "")
	if err != nil {
		return false, err
	}
	var e store.Effort
	for _, x := range effs {
		if x.ID == sh.EffortID {
			e = x
		}
	}
	if e.State != store.EffortOpen && e.State != store.EffortShipping {
		return false, nil
	}
	list, err := s.Store.ListShipments(ctx, e.ID)
	if err != nil {
		return false, err
	}
	for _, o := range list {
		if o.ID != sh.ID && shipmentOpen(o.State) {
			return false, nil
		}
	}
	return true, s.closeEffort(ctx, nil, repo, e, store.EffortMerged, how)
}

// refuseOverSplit stops shipping an effort as one pull request while parts
// of a split are open: git cannot hold vx/<slug> next to vx/<slug>/<n>-...,
// and the parts are what reviewers look at.
func (s *Service) refuseOverSplit(ctx context.Context, plan ShipPlan) error {
	list, err := s.Store.ListShipments(ctx, plan.Effort.ID)
	if err != nil {
		return err
	}
	var open []string
	mode := ""
	for _, sh := range list {
		if sh.Split != "" && shipmentOpen(sh.State) {
			open = append(open, sh.Branch)
			mode = sh.Split
		}
	}
	if len(open) == 0 {
		return nil
	}
	return errf(CodeFailedPrecondition, "effort %s is shipped as %d pull requests (%s): update them with vx ship open --split %s, or close them on the host first",
		plan.Effort.Slug, len(open), strings.Join(open, ", "), mode)
}
