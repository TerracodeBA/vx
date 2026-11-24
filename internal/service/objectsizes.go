package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/store"
)

// Object sizes (table object_sizes) are the bytes objects take in the blob
// store, for storage quotas (Store.RepoStoredBytes, Store.OrgStoredBytes).
// Uploads and merges queue their new objects; a background loop asks the
// blob store for their sizes (a stat on disk, a HEAD request on a bucket)
// and records them. A full pass at start and every SizeScanInterval catches
// everything else: git imports, backfilled repositories, queue overflows.

// SizeScanInterval is how often the server looks for objects of any
// repository that have no recorded size.
const SizeScanInterval = 10 * time.Minute

// sizeQueueMax bounds the queued hashes; more are left to the next scan.
const sizeQueueMax = 100_000

type objectSizer struct {
	mu      sync.Mutex
	pending []hash.Hash
	wake    chan struct{}
}

func (z *objectSizer) init() {
	if z.wake == nil {
		z.wake = make(chan struct{}, 1)
	}
}

// queue notes objects to size soon. It never blocks.
func (z *objectSizer) queue(hs []hash.Hash) {
	z.mu.Lock()
	z.init()
	if len(z.pending)+len(hs) <= sizeQueueMax {
		z.pending = append(z.pending, hs...)
	}
	z.mu.Unlock()
	select {
	case z.wake <- struct{}{}:
	default:
	}
}

func (z *objectSizer) take() []hash.Hash {
	z.mu.Lock()
	defer z.mu.Unlock()
	out := z.pending
	z.pending = nil
	return out
}

func (z *objectSizer) wakeup() chan struct{} {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.init()
	return z.wake
}

// sizeObjects records the stored size of hs, skipping objects with one.
// An object missing from the blob store is recorded as 0 bytes.
func (s *Service) sizeObjects(ctx context.Context, hs []hash.Hash) (int, error) {
	sz, ok := blob.AsSizer(s.Blobs)
	if !ok || len(hs) == 0 {
		return 0, nil
	}
	known, err := s.Store.ObjectSizesKnown(ctx, hs)
	if err != nil {
		return 0, err
	}
	var todo []hash.Hash
	seen := map[hash.Hash]bool{}
	for _, h := range hs {
		if !known[h] && !seen[h] && !h.IsZero() {
			seen[h] = true
			todo = append(todo, h)
		}
	}
	out := make([]store.ObjectSize, len(todo))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 8)
	for i, h := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			n, err := sz.Size(ctx, h)
			if errors.Is(err, blob.ErrNotFound) {
				n, err = 0, nil
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			out[i] = store.ObjectSize{Hash: h, Bytes: n}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}
	return len(out), s.Store.RecordObjectSizes(ctx, out)
}

// SizeAllObjects records sizes for every object of any repository that has
// none yet, in batches, and returns how many it recorded.
func (s *Service) SizeAllObjects(ctx context.Context) (int, error) {
	if _, ok := blob.AsSizer(s.Blobs); !ok {
		return 0, nil
	}
	total := 0
	for {
		hs, err := s.Store.ObjectsWithoutSize(ctx, 1000)
		if err != nil || len(hs) == 0 {
			return total, err
		}
		n, err := s.sizeObjects(ctx, hs)
		total += n
		if err != nil {
			return total, err
		}
	}
}

// ObjectSizesPending counts objects of repositories whose size is not
// recorded yet; they count 0 bytes toward quotas until it is.
func (s *Service) ObjectSizesPending(ctx context.Context) (int64, error) {
	return s.Store.CountObjectsWithoutSize(ctx)
}

// RunObjectSizer records object sizes until ctx ends: queued objects as
// they come, and a full pass at start and every SizeScanInterval.
func (s *Service) RunObjectSizer(ctx context.Context, log *slog.Logger) {
	if _, ok := blob.AsSizer(s.Blobs); !ok {
		return
	}
	scan := time.NewTimer(0)
	defer scan.Stop()
	wake := s.sizes.wakeup()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if _, err := s.sizeObjects(ctx, s.sizes.take()); err != nil && ctx.Err() == nil {
				log.Warn("recording object sizes; the next scan retries", "err", err)
			}
		case <-scan.C:
			if n, err := s.SizeAllObjects(ctx); err != nil && ctx.Err() == nil {
				log.Warn("object size scan stopped; it runs again later", "err", err, "recorded", n)
			} else if n > 0 {
				log.Info("recorded object sizes", "objects", n)
			}
			scan.Reset(SizeScanInterval)
		}
	}
}
