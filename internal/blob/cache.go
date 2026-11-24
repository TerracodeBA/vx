package blob

import (
	"container/list"
	"context"
	"sync"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Cached keeps recently used objects of a remote store in memory, bounded
// by total size, least recently used first out. Objects are immutable, so a
// cached object never goes stale; only presence is cached, never absence
// (another server may store the object a moment later).
type Cached struct {
	inner Store
	max   int64

	mu      sync.Mutex
	size    int64
	order   *list.List // front: most recently used
	entries map[hash.Hash]*list.Element
	stats   CacheStats
}

type cacheEntry struct {
	h    hash.Hash
	kind hash.Kind
	data []byte
}

// CacheStats counts cache use.
type CacheStats struct {
	Hits, Misses int64
	Objects      int
	Bytes        int64
}

// NewCached wraps inner with a cache of at most maxBytes of object data.
// Objects larger than an eighth of that are not cached, so one large file
// cannot push out everything else.
func NewCached(inner Store, maxBytes int64) *Cached {
	return &Cached{inner: inner, max: maxBytes, order: list.New(), entries: map[hash.Hash]*list.Element{}}
}

// Inner is the wrapped store.
func (c *Cached) Inner() Store { return c.inner }

// String names the store for logs.
func (c *Cached) String() string {
	if s, ok := c.inner.(interface{ String() string }); ok {
		return s.String()
	}
	return "cached store"
}

// Stats returns the cache counters.
func (c *Cached) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Objects, s.Bytes = len(c.entries), c.size
	return s
}

func (c *Cached) lookup(h hash.Hash) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[h]
	if !ok {
		c.stats.Misses++
		return cacheEntry{}, false
	}
	c.stats.Hits++
	c.order.MoveToFront(el)
	return el.Value.(cacheEntry), true
}

func (c *Cached) add(h hash.Hash, kind hash.Kind, data []byte) {
	n := int64(len(data)) + 64
	if n > c.max/8 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[h]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.entries[h] = c.order.PushFront(cacheEntry{h: h, kind: kind, data: data})
	c.size += n
	for c.size > c.max {
		el := c.order.Back()
		e := el.Value.(cacheEntry)
		c.order.Remove(el)
		delete(c.entries, e.h)
		c.size -= int64(len(e.data)) + 64
	}
}

// Put implements Store.
func (c *Cached) Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	h := hash.Of(kind, data)
	if _, ok := c.lookup(h); ok {
		return h, nil
	}
	h, err := c.inner.Put(ctx, kind, data)
	if err != nil {
		return hash.Zero, err
	}
	c.add(h, kind, append([]byte(nil), data...))
	return h, nil
}

// Get implements Store. Callers may change the returned slice: they get a
// copy, as from the other stores.
func (c *Cached) Get(ctx context.Context, h hash.Hash) (hash.Kind, []byte, error) {
	if e, ok := c.lookup(h); ok {
		return e.kind, append([]byte(nil), e.data...), nil
	}
	kind, data, err := c.inner.Get(ctx, h)
	if err != nil {
		return "", nil, err
	}
	c.add(h, kind, append([]byte(nil), data...))
	return kind, data, nil
}

// Has implements Store.
func (c *Cached) Has(ctx context.Context, h hash.Hash) (bool, error) {
	c.mu.Lock()
	_, ok := c.entries[h]
	c.mu.Unlock()
	if ok {
		return true, nil
	}
	return c.inner.Has(ctx, h)
}

// List implements Lister when the inner store does.
func (c *Cached) List(ctx context.Context, fn func(hash.Hash) error) error {
	l, ok := AsLister(c.inner)
	if !ok {
		return errNoList(c.inner)
	}
	return l.List(ctx, fn)
}

// Size implements Sizer when the inner store does.
func (c *Cached) Size(ctx context.Context, h hash.Hash) (int64, error) {
	s, ok := AsSizer(c.inner)
	if !ok {
		return 0, errNoSize(c.inner)
	}
	return s.Size(ctx, h)
}
