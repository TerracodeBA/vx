package blob

import (
	"context"
	"fmt"
	"sync"

	"github.com/TerracodeBA/vx/internal/hash"
)

type memObject struct {
	kind hash.Kind
	data []byte
}

// Mem is an in-memory Store for tests and short-lived work.
type Mem struct {
	mu   sync.RWMutex
	objs map[hash.Hash]memObject
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem { return &Mem{objs: make(map[hash.Hash]memObject)} }

// Put implements Store.
func (m *Mem) Put(_ context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	if _, err := kindCode(kind); err != nil {
		return hash.Zero, err
	}
	h := hash.Of(kind, data)
	m.mu.Lock()
	if _, ok := m.objs[h]; !ok {
		m.objs[h] = memObject{kind: kind, data: append([]byte(nil), data...)}
	}
	m.mu.Unlock()
	return h, nil
}

// Get implements Store.
func (m *Mem) Get(_ context.Context, h hash.Hash) (hash.Kind, []byte, error) {
	m.mu.RLock()
	o, ok := m.objs[h]
	m.mu.RUnlock()
	if !ok {
		return "", nil, fmt.Errorf("%w: %s", ErrNotFound, h.Short())
	}
	return o.kind, append([]byte(nil), o.data...), nil
}

// Has implements Store.
func (m *Mem) Has(_ context.Context, h hash.Hash) (bool, error) {
	m.mu.RLock()
	_, ok := m.objs[h]
	m.mu.RUnlock()
	return ok, nil
}

// Len returns the number of stored objects.
func (m *Mem) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objs)
}
