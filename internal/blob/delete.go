package blob

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Deleter is a Store that can delete an object. Objects are shared between
// repositories (content-addressed), so only the organization purge deletes,
// and only objects no repository records any more (see
// store.RecordRepoObjects for how a recording racing a deletion is caught).
// Deleting an object that is not there is not an error.
type Deleter interface {
	Delete(ctx context.Context, h hash.Hash) error
}

// AsDeleter returns s, or the first store it wraps, as a Deleter. A cache
// in front of the store is a Deleter itself, so the object also leaves the
// cache.
func AsDeleter(s Store) (Deleter, bool) {
	for s != nil {
		if d, ok := s.(Deleter); ok {
			return d, true
		}
		u, ok := s.(Unwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

// Fresh returns the store under s's wrappers and caches, whose Has and Put
// ask the backend itself: a cache on one server may still hold an object
// another server deleted.
func Fresh(s Store) Store {
	for {
		switch w := s.(type) {
		case *Cached:
			s = w.inner
		case Unwrapper:
			s = w.Unwrap()
		default:
			return s
		}
	}
}

// Delete implements Deleter.
func (d *Disk) Delete(ctx context.Context, h hash.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := os.Remove(d.path(h))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("delete object %s: %w", h.Short(), err)
}

// Delete implements Deleter. S3 deletes of missing keys succeed.
func (s *S3) Delete(ctx context.Context, h hash.Hash) error {
	if err := s.c.Delete(ctx, objectKey(h)); err != nil && !IsNotFound(err) {
		return fmt.Errorf("delete object %s: %w", h.Short(), err)
	}
	return nil
}

// Delete implements Deleter.
func (m *Mem) Delete(_ context.Context, h hash.Hash) error {
	m.mu.Lock()
	delete(m.objs, h)
	m.mu.Unlock()
	return nil
}

// Delete implements Deleter: the object leaves the cache and the store
// behind it.
func (c *Cached) Delete(ctx context.Context, h hash.Hash) error {
	c.mu.Lock()
	if el, ok := c.entries[h]; ok {
		e := el.Value.(cacheEntry)
		c.order.Remove(el)
		delete(c.entries, h)
		c.size -= int64(len(e.data)) + 64
	}
	c.mu.Unlock()
	d, ok := AsDeleter(c.inner)
	if !ok {
		return fmt.Errorf("%s cannot delete objects", describe(c.inner))
	}
	return d.Delete(ctx, h)
}

func describe(s Store) string {
	if d, ok := s.(fmt.Stringer); ok {
		return d.String()
	}
	return fmt.Sprintf("%T", s)
}
