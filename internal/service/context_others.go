package service

// Other efforts in Context: what other open efforts of the same repository
// do on the targets (#26). Efforts used to be walls; this is how an agent
// learns that another effort changes, claims or decided something about
// the code it is about to touch.

import (
	"context"
	"sort"
	"sync"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// Limits on the work Context does for other efforts, so a repository with
// many efforts or large ones stays cheap:
//   - at most otherEffortsScan other open efforts are looked at, the most
//     recently created first;
//   - an effort's changed files are the diff of its base tree to its head
//     tree (cost proportional to the change, cached by tree pair, see
//     effortChanges); at most otherChangedMax of them are matched;
//   - the newest otherNotesScan interface and decision notes of an effort
//     are read, and at most OtherEffortNotes of them returned;
//   - at most otherPathsShown changed paths are returned per effort.
const (
	OtherEffortNotes = 10
	otherEffortsScan = 25
	otherChangedMax  = 5000
	otherNotesScan   = 500
	otherPathsShown  = 100
)

// otherEfforts fills NoteContext.OtherEfforts for the effort in v: every
// other open effort of the repository that changed, claimed or posted an
// interface or decision on the targets. Without targets, the targets are
// the files the effort itself changed since its base. Efforts with nothing
// matching are left out.
//
// Access: all efforts of a repository share its permissions, and the
// caller already has read on it (Context checked through boardEffort), so
// every effort found here is one the caller can read.
func (s *Service) otherEfforts(ctx context.Context, v EffortView, targets []string, c *noteCache) ([]OtherEffortActivity, error) {
	effs, err := s.Store.ListEfforts(ctx, v.Repo.ID, store.EffortOpen)
	if err != nil {
		return nil, err
	}
	var others []store.Effort
	for _, e := range effs {
		if e.ID != v.Effort.ID && e.State == store.EffortOpen {
			others = append(others, e)
		}
	}
	if len(others) == 0 {
		return nil, nil
	}
	if len(others) > otherEffortsScan {
		others = others[:otherEffortsScan]
	}
	byFiles := len(targets) == 0
	if byFiles {
		mine, err := s.effortChangedPaths(ctx, v.Repo.ID, v.Effort)
		if err != nil {
			return nil, err
		}
		if len(mine) == 0 {
			return nil, nil
		}
		targets = mine
	}
	var out []OtherEffortActivity
	for _, e := range others {
		act, ok, err := s.otherEffort(ctx, v.Repo.ID, e, targets, byFiles, c)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, act)
		}
	}
	return out, nil
}

// otherEffort is one other effort's activity on want. With byFiles, want
// are the caller effort's changed files and the effort is included only
// when it changed one of them.
func (s *Service) otherEffort(ctx context.Context, repoID uuid.UUID, e store.Effort, want []string, byFiles bool, c *noteCache) (OtherEffortActivity, bool, error) {
	act := OtherEffortActivity{Effort: e}
	changed, err := s.effortChangedPaths(ctx, repoID, e)
	if err != nil {
		return act, false, err
	}
	for _, p := range changed {
		if matchAny([]string{p}, want) {
			act.ChangedPaths = append(act.ChangedPaths, p)
			if len(act.ChangedPaths) == otherPathsShown {
				break
			}
		}
	}
	if byFiles && len(act.ChangedPaths) == 0 {
		return act, false, nil
	}
	claims, err := s.activeClaims(ctx, s.Store.Q, e.ID, want, uuid.Nil)
	if err != nil {
		return act, false, err
	}
	if act.Claims, err = c.claims(ctx, claims); err != nil {
		return act, false, err
	}
	touches := len(act.ChangedPaths) > 0 || len(act.Claims) > 0
	rows, err := s.Store.ListNotes(ctx, store.NoteFilter{
		EffortID: e.ID, Kinds: []string{string(NoteInterface), string(NoteDecision)}, Current: true, Limit: otherNotesScan,
	})
	if err != nil {
		return act, false, err
	}
	notes, err := c.notes(ctx, rows, s.Clock())
	if err != nil {
		return act, false, err
	}
	for _, n := range notes {
		if n.SupersededBy != nil {
			continue
		}
		if byFiles || matchAny(n.Targets, want) || (touches && hasSymbol(n.Targets)) {
			act.Notes = append(act.Notes, n)
			if len(act.Notes) == OtherEffortNotes {
				break
			}
		}
	}
	if !touches && len(act.Notes) == 0 {
		return act, false, nil
	}
	if act.Owner, err = c.principal(ctx, e.OwnerID); err != nil {
		return act, false, err
	}
	ws, err := s.Store.ListWorkspaces(ctx, e.ID, store.WorkspaceOpen)
	if err != nil {
		return act, false, err
	}
	for _, w := range ws {
		owner, err := c.principal(ctx, w.OwnerID)
		if err != nil {
			return act, false, err
		}
		act.Working = append(act.Working, ContextWorkspace{Workspace: w, Owner: owner})
	}
	return act, true, nil
}

// hasSymbol reports whether a note is about a symbol (auth.Login) rather
// than a path. vx cannot tell which file a symbol lives in, so an effort
// that changes or claims the targets shows its symbol notes too.
func hasSymbol(targets []string) bool {
	for _, t := range targets {
		if !looksLikePath(t) {
			return true
		}
	}
	return false
}

// effortChangedPaths lists the files an effort changed since its base
// (base tree to head tree), sorted, at most otherChangedMax.
func (s *Service) effortChangedPaths(ctx context.Context, repoID uuid.UUID, e store.Effort) ([]string, error) {
	changes, err := s.effortChanges(ctx, repoID, e)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, min(len(changes), otherChangedMax))
	for _, ch := range changes {
		paths = append(paths, ch.Path)
		if len(paths) == otherChangedMax {
			break
		}
	}
	return paths, nil
}

// effortChanges lists every file change of an effort since its base (base
// tree to head tree), sorted by path. The result is shared: do not modify
// it.
func (s *Service) effortChanges(ctx context.Context, repoID uuid.UUID, e store.Effort) ([]object.Change, error) {
	if e.Head == e.Base {
		return nil, nil
	}
	base, err := s.Store.GetChangeset(ctx, repoID, e.Base)
	if err != nil {
		return nil, classify(err)
	}
	head, err := s.Store.GetChangeset(ctx, repoID, e.Head)
	if err != nil {
		return nil, classify(err)
	}
	return s.treeChanges(ctx, base.Tree, head.Tree)
}

// treeChanges lists the changes between two trees, sorted by path. Trees
// are content addressed, so the result for a pair never changes and is
// cached. The result is shared: do not modify it.
func (s *Service) treeChanges(ctx context.Context, from, to hash.Hash) ([]object.Change, error) {
	if from == to {
		return nil, nil
	}
	key := [2]hash.Hash{from, to}
	if c, ok := changesCache.get(key); ok {
		return c, nil
	}
	changes, err := object.DiffTrees(ctx, s.Blobs, from, to)
	if err != nil {
		return nil, err
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	changesCache.put(key, changes)
	return changes, nil
}

// changeCache remembers tree diffs per tree pair. It forgets everything
// when it holds too many changes: they are cheap to recompute.
type changeCache struct {
	mu    sync.Mutex
	m     map[[2]hash.Hash][]object.Change
	total int
}

const changeCacheMax = 200_000 // changes held, over all tree pairs

var changesCache = &changeCache{m: map[[2]hash.Hash][]object.Change{}}

func (c *changeCache) get(k [2]hash.Hash) ([]object.Change, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *changeCache) put(k [2]hash.Hash, v []object.Change) {
	if len(v) > changeCacheMax {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total+len(v) > changeCacheMax || len(c.m) >= 4096 {
		c.m, c.total = map[[2]hash.Hash][]object.Change{}, 0
	}
	if _, ok := c.m[k]; !ok {
		c.m[k] = v
		c.total += len(v)
	}
}
