// Package blob stores content-addressed objects: file contents and trees.
//
// Objects are immutable. Put returns the hash of the object, and storing the
// same bytes twice is a no-op. Every read is verified against its hash, so a
// corrupted store fails loudly instead of returning wrong content.
package blob

import (
	"context"
	"errors"
	"fmt"

	"github.com/TerracodeBA/vx/internal/hash"
)

// ErrNotFound is returned when an object is not in the store.
var ErrNotFound = errors.New("object not found")

// ErrCorrupt is returned when stored bytes do not match their hash.
var ErrCorrupt = errors.New("object corrupt")

// Store is a content-addressed object store.
type Store interface {
	// Put stores data under the hash of kind and data.
	Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error)
	// Get returns the object and its kind.
	Get(ctx context.Context, h hash.Hash) (hash.Kind, []byte, error)
	// Has reports whether the object exists.
	Has(ctx context.Context, h hash.Hash) (bool, error)
}

// Lister is a Store that can list every object it holds, in no particular
// order. Copies between stores (vx-server blobs copy) need it.
type Lister interface {
	List(ctx context.Context, fn func(hash.Hash) error) error
}

// Sizer is a Store that tells how many bytes an object takes in it
// (compressed, as stored), without reading the object.
type Sizer interface {
	Size(ctx context.Context, h hash.Hash) (int64, error)
}

// Unwrapper is a Store that wraps another (metrics, caches); AsLister and
// AsSizer look through it for the optional interfaces.
type Unwrapper interface {
	Unwrap() Store
}

// AsSizer returns s, or the first store it wraps, as a Sizer.
func AsSizer(s Store) (Sizer, bool) {
	for s != nil {
		if z, ok := s.(Sizer); ok {
			return z, true
		}
		u, ok := s.(Unwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

// AsLister returns s, or the first store it wraps, as a Lister.
func AsLister(s Store) (Lister, bool) {
	for s != nil {
		if l, ok := s.(Lister); ok {
			return l, true
		}
		u, ok := s.(Unwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

// GetKind is Get that also checks the object has the expected kind.
func GetKind(ctx context.Context, s Store, want hash.Kind, h hash.Hash) ([]byte, error) {
	kind, data, err := s.Get(ctx, h)
	if err != nil {
		return nil, err
	}
	if kind != want {
		return nil, fmt.Errorf("object %s is a %s, want %s", h.Short(), kind, want)
	}
	return data, nil
}

func kindCode(k hash.Kind) (byte, error) {
	switch k {
	case hash.KindBlob:
		return 'b', nil
	case hash.KindTree:
		return 't', nil
	case hash.KindChangeset:
		return 'c', nil
	}
	return 0, fmt.Errorf("unknown object kind %q", k)
}

func codeKind(c byte) (hash.Kind, error) {
	switch c {
	case 'b':
		return hash.KindBlob, nil
	case 't':
		return hash.KindTree, nil
	case 'c':
		return hash.KindChangeset, nil
	}
	return "", fmt.Errorf("unknown object kind code %q", c)
}
