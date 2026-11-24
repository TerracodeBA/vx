package blob

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/TerracodeBA/vx/internal/hash"
)

// CopyOptions configures Copy.
type CopyOptions struct {
	// Jobs is how many objects are copied at once (default 8).
	Jobs int
	// VerifyExisting reads back objects the destination already has
	// instead of trusting them; without it they are skipped, which makes a
	// second run after an interruption fast.
	VerifyExisting bool
	// Progress, when set, is called after every 1000 objects and at the end.
	Progress func(CopyStats)
}

// CopyStats counts what Copy did.
type CopyStats struct {
	Seen     int64 // objects listed in the source
	Copied   int64 // written to the destination and read back
	Skipped  int64 // already in the destination
	Verified int64 // already in the destination, read back and correct
	Bytes    int64 // uncompressed bytes written
}

// Copy copies every object of src into dst. Each object is read from src
// (verified against its hash on read), written to dst and read back from
// dst, so a finished copy is known to be complete and correct. Writes are
// atomic per object and objects are immutable, so an interrupted copy is
// resumed by running it again: objects already in dst are skipped.
func Copy(ctx context.Context, src, dst Store, o CopyOptions) (CopyStats, error) {
	l, ok := AsLister(src)
	if !ok {
		return CopyStats{}, errNoList(src)
	}
	jobs := o.Jobs
	if jobs <= 0 {
		jobs = 8
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var st struct{ seen, copied, skipped, verified, bytes atomic.Int64 }
	snapshot := func() CopyStats {
		return CopyStats{Seen: st.seen.Load(), Copied: st.copied.Load(), Skipped: st.skipped.Load(),
			Verified: st.verified.Load(), Bytes: st.bytes.Load()}
	}
	var progressMu sync.Mutex
	done := func() {
		if n := st.copied.Load() + st.skipped.Load() + st.verified.Load(); o.Progress != nil && n%1000 == 0 {
			progressMu.Lock()
			o.Progress(snapshot())
			progressMu.Unlock()
		}
	}
	work := make(chan hash.Hash, jobs*4)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range work {
				if err := copyOne(ctx, src, dst, h, o.VerifyExisting, &st.copied, &st.skipped, &st.verified, &st.bytes); err != nil {
					cancel(err)
					return
				}
				done()
			}
		}()
	}
	listErr := l.List(ctx, func(h hash.Hash) error {
		st.seen.Add(1)
		select {
		case work <- h:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	})
	close(work)
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return snapshot(), err
	}
	if listErr != nil {
		return snapshot(), listErr
	}
	if err := ctx.Err(); err != nil {
		return snapshot(), err
	}
	if o.Progress != nil {
		o.Progress(snapshot())
	}
	return snapshot(), nil
}

func copyOne(ctx context.Context, src, dst Store, h hash.Hash, verifyExisting bool, copied, skipped, verified, written *atomic.Int64) error {
	has, err := dst.Has(ctx, h)
	if err != nil {
		return fmt.Errorf("check %s in the destination: %w", h.Short(), err)
	}
	if has && !verifyExisting {
		skipped.Add(1)
		return nil
	}
	if has {
		if _, _, err := dst.Get(ctx, h); err == nil {
			verified.Add(1)
			return nil
		} else if !errors.Is(err, ErrCorrupt) {
			return fmt.Errorf("verify %s in the destination: %w", h.Short(), err)
		}
		// A corrupt copy is written again below.
	}
	kind, data, err := src.Get(ctx, h)
	if err != nil {
		return fmt.Errorf("read %s from the source: %w", h.Short(), err)
	}
	if err := putFresh(ctx, dst, kind, data, has); err != nil {
		return fmt.Errorf("write %s to the destination: %w", h.Short(), err)
	}
	if _, _, err := dst.Get(ctx, h); err != nil {
		return fmt.Errorf("read back %s from the destination: %w", h.Short(), err)
	}
	copied.Add(1)
	written.Add(int64(len(data)))
	return nil
}

// overwriter is a store that can replace an object it holds (a corrupt one).
type overwriter interface {
	overwrite(ctx context.Context, kind hash.Kind, data []byte) error
}

func putFresh(ctx context.Context, dst Store, kind hash.Kind, data []byte, replace bool) error {
	if replace {
		if o, ok := dst.(overwriter); ok {
			return o.overwrite(ctx, kind, data)
		}
	}
	_, err := dst.Put(ctx, kind, data)
	return err
}
