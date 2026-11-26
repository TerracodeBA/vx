package service

// Objects the server makes (merges on land, approving a review, ship
// plans and split plans, rebases, the overlap forecast) are recorded as
// part of their repository the moment they are written, also when the
// operation then stops on a conflict or only simulates. Every object in
// the blob store is then recorded by some repository (or is the empty
// tree), so the purge of a deleted organization finds all of its content,
// and the shared-object rule (delete only what no remaining repository
// records) covers server-made objects too. They are made from the
// repository's own objects, so recording them gives nobody anything new.

import (
	"context"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
)

// repoWriter is the blob store merges write through: each object it puts
// is recorded for its repository at once (store.RecordRepoObjects, so a
// purge running at the same time cannot leave it unrecorded or deleted).
type repoWriter struct {
	blob.Store
	s      *Service
	repoID uuid.UUID
}

// repoWrites returns the store for objects the server makes in a
// repository.
func (s *Service) repoWrites(repoID uuid.UUID) blob.Store {
	return &repoWriter{Store: s.Blobs, s: s, repoID: repoID}
}

// Unwrap returns the store behind, for blob.AsSizer and friends.
func (w *repoWriter) Unwrap() blob.Store { return w.Store }

// Put implements blob.Store.
func (w *repoWriter) Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	h, err := w.Store.Put(ctx, kind, data)
	if err != nil || h == emptyTree {
		return h, err
	}
	missing, err := w.s.Store.RecordRepoObjects(ctx, w.repoID, []hash.Hash{h}, w.s.blobPresent)
	for try := 0; err == nil && len(missing) > 0 && try < 3; try++ {
		// A purge deleted it between the put and the recording: write it
		// again (Put asks the store, not a cache) and record it again.
		if _, err = w.Store.Put(ctx, kind, data); err == nil {
			missing, err = w.s.Store.RecordRepoObjects(ctx, w.repoID, missing, w.s.blobPresent)
		}
	}
	if err != nil {
		return hash.Zero, err
	}
	if len(missing) > 0 {
		return hash.Zero, errObjectsPurged(len(missing))
	}
	w.s.sizes.queue([]hash.Hash{h})
	return h, nil
}
