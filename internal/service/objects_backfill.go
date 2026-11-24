package service

// Backfill of repo_objects for repositories whose objects were stored
// before objects were scoped to repositories. Each such repository is
// queued (repo_objects_backfill); the backfill walks every changeset's tree,
// records its objects and removes the repository from the queue. It is
// idempotent and resumable: an interrupted walk leaves the repository queued
// and the next one records the same objects again, which is a no-op.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// objectIndex remembers which repositories need no backfill, and serializes
// backfills per repository within the process.
type objectIndex struct {
	mu    sync.Mutex
	done  map[uuid.UUID]bool
	locks map[uuid.UUID]*sync.Mutex
}

func (x *objectIndex) isDone(id uuid.UUID) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.done[id]
}

func (x *objectIndex) markDone(id uuid.UUID) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.done == nil {
		x.done = map[uuid.UUID]bool{}
	}
	x.done[id] = true
}

func (x *objectIndex) lock(id uuid.UUID) func() {
	x.mu.Lock()
	if x.locks == nil {
		x.locks = map[uuid.UUID]*sync.Mutex{}
	}
	l, ok := x.locks[id]
	if !ok {
		l = &sync.Mutex{}
		x.locks[id] = l
	}
	x.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// ensureObjectsIndexed backfills the repository's objects first if it is
// still queued, so object requests for it wait for its own backfill only.
func (s *Service) ensureObjectsIndexed(ctx context.Context, repoID uuid.UUID) error {
	if s.objects.isDone(repoID) {
		return nil
	}
	pending, err := s.Store.ObjectBackfillPending(ctx, repoID)
	if err != nil {
		return err
	}
	if !pending {
		s.objects.markDone(repoID)
		return nil
	}
	_, err = s.BackfillRepoObjects(ctx, repoID, false, slog.Default())
	return err
}

// BackfillStats is what one repository's backfill did.
type BackfillStats struct {
	Repo       string
	Changesets int
	Objects    int // objects recorded, including ones already recorded
	Skipped    int // trees that could not be read (missing from the store)
	Ran        bool
	Took       time.Duration
}

// BackfillRepoObjects records the objects of every changeset tree in the
// repository. Unless force is set it does nothing for a repository that is
// not queued. Concurrent calls for one repository wait for each other.
func (s *Service) BackfillRepoObjects(ctx context.Context, repoID uuid.UUID, force bool, log *slog.Logger) (BackfillStats, error) {
	if log == nil {
		log = slog.Default()
	}
	unlock := s.objects.lock(repoID)
	defer unlock()
	st := BackfillStats{}
	repo, err := s.Store.RepoByID(ctx, repoID)
	if err != nil {
		return st, classify(err)
	}
	st.Repo = repo.Name
	if !force {
		pending, err := s.Store.ObjectBackfillPending(ctx, repoID)
		if err != nil {
			return st, err
		}
		if !pending {
			s.objects.markDone(repoID)
			return st, nil
		}
	}
	start := time.Now()
	st.Ran = true
	log.Info("backfilling repository objects", "repo", repo.Name)

	trees := map[hash.Hash]hash.Hash{} // changeset -> tree, for parents
	walked := map[hash.Hash]bool{}
	var batch []hash.Hash
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		st.Objects += len(batch)
		err := s.Store.AddRepoObjects(ctx, repoID, batch)
		batch = batch[:0]
		return err
	}
	walk := func(from, to hash.Hash) error {
		ts, bs, err := object.NewObjects(ctx, s.Blobs, from, to)
		batch = append(append(batch, ts...), bs...)
		if errors.Is(err, blob.ErrNotFound) {
			// Keep what was read; a missing tree only means that part of
			// the repository cannot be served, as before.
			st.Skipped++
			log.Warn("backfill: tree missing from the object store", "repo", repo.Name, "tree", to.Short(), "err", err)
			err = nil
		}
		if err != nil {
			return err
		}
		if len(batch) >= 5000 {
			return flush()
		}
		return nil
	}
	batch = append(batch, emptyTree)
	// Walk until a listing shows no new changesets, so changesets added
	// while the backfill runs (an import, a land) are covered too.
	for {
		list, err := s.Store.ListChangesetTrees(ctx, repoID)
		if err != nil {
			return st, err
		}
		for _, c := range list {
			trees[c.Hash] = c.Tree
		}
		fresh := 0
		for _, c := range list {
			if walked[c.Hash] {
				continue
			}
			walked[c.Hash] = true
			fresh++
			// A changeset only adds what differs from its parent; the
			// parent's own walk covers the rest. Roots and shallow imports
			// (parent not in the repo) are walked in full.
			parent := trees[c.FirstParent]
			if err := walk(parent, c.Tree); err != nil {
				return st, err
			}
			if !c.SnapshotTree.IsZero() {
				if err := walk(c.Tree, c.SnapshotTree); err != nil {
					return st, err
				}
			}
			st.Changesets++
			if st.Changesets%500 == 0 {
				log.Info("backfilling repository objects", "repo", repo.Name, "changesets", st.Changesets, "of", len(list))
			}
		}
		if err := flush(); err != nil {
			return st, err
		}
		if fresh == 0 {
			break
		}
	}
	if err := s.Store.FinishObjectBackfill(ctx, repoID); err != nil {
		return st, err
	}
	s.objects.markDone(repoID)
	st.Took = time.Since(start)
	log.Info("backfilled repository objects", "repo", repo.Name, "changesets", st.Changesets, "objects", st.Objects, "skipped", st.Skipped, "ms", st.Took.Milliseconds())
	return st, nil
}

// BackfillObjects queues every repository that has changesets but no
// recorded objects and backfills each queued repository, one at a time.
// Requests for a repository that is still queued backfill it on demand, so
// a long backfill of one repository does not hold up the others.
func (s *Service) BackfillObjects(ctx context.Context, log *slog.Logger) ([]BackfillStats, error) {
	if log == nil {
		log = slog.Default()
	}
	if _, err := s.Store.QueueObjectBackfills(ctx); err != nil {
		return nil, err
	}
	ids, err := s.Store.ObjectBackfillQueue(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		log.Info("repositories need their objects indexed", "count", len(ids))
	}
	var out []BackfillStats
	for _, id := range ids {
		st, err := s.BackfillRepoObjects(ctx, id, false, log)
		if errors.Is(err, store.ErrNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return out, err
		}
		if st.Ran {
			out = append(out, st)
		}
	}
	return out, nil
}
