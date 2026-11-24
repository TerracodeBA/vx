// Package hash defines the content hash used for every object in vx.
//
// vx identifies blobs, trees and changesets by the BLAKE3-256 hash of their
// canonical encoding. The text form is "b3:" followed by 64 lowercase hex
// characters. Objects of different kinds are hashed with a distinct domain
// prefix so that a blob can never collide with a tree that has the same bytes.
package hash

import (
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/zeebo/blake3"
)

// Size is the length of a hash in bytes.
const Size = 32

// Prefix is the text prefix that marks a vx hash.
const Prefix = "b3:"

// Hash is a BLAKE3-256 digest. The zero value means "no object".
type Hash [Size]byte

// Zero is the empty hash.
var Zero Hash

// Kind separates the hash domains of different object types.
type Kind string

const (
	KindBlob      Kind = "blob"
	KindTree      Kind = "tree"
	KindChangeset Kind = "changeset"
)

// Of hashes data in the domain of kind.
func Of(kind Kind, data []byte) Hash {
	h := blake3.New()
	_, _ = h.Write([]byte("vx:"))
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(data)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// IsZero reports whether h is the empty hash.
func (h Hash) IsZero() bool { return h == Zero }

// String returns the full text form, for example "b3:9f1c...".
func (h Hash) String() string {
	if h.IsZero() {
		return ""
	}
	return Prefix + hex.EncodeToString(h[:])
}

// Short returns a 12 character hex prefix for display.
func (h Hash) Short() string {
	if h.IsZero() {
		return "000000000000"
	}
	return hex.EncodeToString(h[:6])
}

// Hex returns the 64 character hex form without prefix.
func (h Hash) Hex() string { return hex.EncodeToString(h[:]) }

// ErrInvalid is returned when parsing malformed hash text.
var ErrInvalid = errors.New("invalid hash")

// Parse reads the text form. The "b3:" prefix is optional.
func Parse(s string) (Hash, error) {
	var h Hash
	if s == "" {
		return h, nil
	}
	s = strings.TrimPrefix(s, Prefix)
	if len(s) != Size*2 {
		return h, fmt.Errorf("%w: %q has %d hex characters, want %d", ErrInvalid, s, len(s), Size*2)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return h, nil
}

// MustParse is Parse for tests and constants.
func MustParse(s string) Hash {
	h, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return h
}

// FromBytes converts a 32 byte slice. A nil or empty slice gives Zero.
func FromBytes(b []byte) (Hash, error) {
	var h Hash
	if len(b) == 0 {
		return h, nil
	}
	if len(b) != Size {
		return h, fmt.Errorf("%w: %d bytes, want %d", ErrInvalid, len(b), Size)
	}
	copy(h[:], b)
	return h, nil
}

// Bytes returns the hash as a slice, or nil for Zero.
func (h Hash) Bytes() []byte {
	if h.IsZero() {
		return nil
	}
	b := make([]byte, Size)
	copy(b, h[:])
	return b
}

// MarshalText implements encoding.TextMarshaler.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*h = v
	return nil
}

// Value implements driver.Valuer, storing the hash as bytea (NULL for Zero).
func (h Hash) Value() (driver.Value, error) {
	if h.IsZero() {
		return nil, nil
	}
	return h[:], nil
}

// Scan implements sql.Scanner.
func (h *Hash) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*h = Zero
		return nil
	case []byte:
		x, err := FromBytes(v)
		if err != nil {
			return err
		}
		*h = x
		return nil
	default:
		return fmt.Errorf("hash: cannot scan %T", src)
	}
}
