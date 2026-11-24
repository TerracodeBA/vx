// Package object defines the vx object model: trees, manifests and changesets.
//
// A tree is one directory: a sorted list of named entries, each pointing to a
// blob (file content) or another tree. Trees are stored in the blob store, so
// identical directories are stored once and two snapshots can be compared by
// skipping every subtree whose hash did not change.
package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
)

// Mode is the type of a tree entry.
type Mode byte

const (
	ModeFile    Mode = 'f' // regular file
	ModeExec    Mode = 'x' // executable file
	ModeSymlink Mode = 'l' // symbolic link, content is the target
	ModeDir     Mode = 'd' // subdirectory, hash is a tree
)

func (m Mode) valid() bool {
	switch m {
	case ModeFile, ModeExec, ModeSymlink, ModeDir:
		return true
	}
	return false
}

// String returns a readable name for the mode.
func (m Mode) String() string {
	switch m {
	case ModeFile:
		return "file"
	case ModeExec:
		return "exec"
	case ModeSymlink:
		return "symlink"
	case ModeDir:
		return "dir"
	}
	return fmt.Sprintf("mode(%d)", m)
}

// ParseMode is the inverse of Mode.String.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "file", "":
		return ModeFile, nil
	case "exec":
		return ModeExec, nil
	case "symlink":
		return ModeSymlink, nil
	case "dir":
		return ModeDir, nil
	}
	return 0, fmt.Errorf("unknown mode %q", s)
}

// Entry is one item in a tree.
type Entry struct {
	Name string
	Mode Mode
	Hash hash.Hash
}

// Tree is one directory.
type Tree struct {
	Entries []Entry // sorted by Name
}

var treeMagic = []byte("vxtree1\n")

// ErrBadPath is returned for paths or names vx cannot store.
var ErrBadPath = errors.New("invalid path")

// ValidName reports whether name can be a single tree entry name.
func ValidName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("%w: entry name %q", ErrBadPath, name)
	}
	if strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w: entry name %q contains / or NUL", ErrBadPath, name)
	}
	return nil
}

// Encode returns the canonical bytes of the tree. Entries must be sorted and
// unique; Encode checks this so a malformed tree is never stored.
func (t *Tree) Encode() ([]byte, error) {
	var b bytes.Buffer
	b.Write(treeMagic)
	for i, e := range t.Entries {
		if err := ValidName(e.Name); err != nil {
			return nil, err
		}
		if !e.Mode.valid() {
			return nil, fmt.Errorf("entry %q has invalid mode", e.Name)
		}
		if i > 0 && t.Entries[i-1].Name >= e.Name {
			return nil, fmt.Errorf("tree entries not sorted or duplicated at %q", e.Name)
		}
		b.WriteByte(byte(e.Mode))
		b.WriteString(e.Name)
		b.WriteByte(0)
		b.Write(e.Hash[:])
	}
	return b.Bytes(), nil
}

// DecodeTree parses canonical tree bytes.
func DecodeTree(data []byte) (*Tree, error) {
	if !bytes.HasPrefix(data, treeMagic) {
		return nil, errors.New("not a vx tree")
	}
	data = data[len(treeMagic):]
	t := &Tree{}
	for len(data) > 0 {
		mode := Mode(data[0])
		if !mode.valid() {
			return nil, fmt.Errorf("tree: invalid mode byte %q", data[0])
		}
		data = data[1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+1+hash.Size {
			return nil, errors.New("tree: truncated entry")
		}
		e := Entry{Name: string(data[:nul]), Mode: mode}
		copy(e.Hash[:], data[nul+1:nul+1+hash.Size])
		data = data[nul+1+hash.Size:]
		if err := ValidName(e.Name); err != nil {
			return nil, err
		}
		if n := len(t.Entries); n > 0 && t.Entries[n-1].Name >= e.Name {
			return nil, fmt.Errorf("tree: entries not sorted at %q", e.Name)
		}
		t.Entries = append(t.Entries, e)
	}
	return t, nil
}

// PutTree stores a tree and returns its hash.
func PutTree(ctx context.Context, s blob.Store, t *Tree) (hash.Hash, error) {
	data, err := t.Encode()
	if err != nil {
		return hash.Zero, err
	}
	return s.Put(ctx, hash.KindTree, data)
}

// GetTree loads a tree. The zero hash is the empty tree.
func GetTree(ctx context.Context, s blob.Store, h hash.Hash) (*Tree, error) {
	if h.IsZero() {
		return &Tree{}, nil
	}
	data, err := blob.GetKind(ctx, s, hash.KindTree, h)
	if err != nil {
		return nil, err
	}
	return DecodeTree(data)
}

// EmptyTree returns the hash of the empty tree, storing it if needed.
func EmptyTree(ctx context.Context, s blob.Store) (hash.Hash, error) {
	return PutTree(ctx, s, &Tree{})
}

// Find returns the entry with name, if present.
func (t *Tree) Find(name string) (Entry, bool) {
	i := sort.Search(len(t.Entries), func(i int) bool { return t.Entries[i].Name >= name })
	if i < len(t.Entries) && t.Entries[i].Name == name {
		return t.Entries[i], true
	}
	return Entry{}, false
}

// Lookup walks root to the entry at path ("a/b/c.go").
func Lookup(ctx context.Context, s blob.Store, root hash.Hash, path string) (Entry, bool, error) {
	parts, err := SplitPath(path)
	if err != nil {
		return Entry{}, false, err
	}
	cur := root
	for i, name := range parts {
		t, err := GetTree(ctx, s, cur)
		if err != nil {
			return Entry{}, false, err
		}
		e, ok := t.Find(name)
		if !ok {
			return Entry{}, false, nil
		}
		if i == len(parts)-1 {
			return e, true, nil
		}
		if e.Mode != ModeDir {
			return Entry{}, false, nil
		}
		cur = e.Hash
	}
	return Entry{}, false, nil
}

// SplitPath validates a slash-separated relative path and splits it.
func SplitPath(p string) ([]string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return nil, fmt.Errorf("%w: %q", ErrBadPath, p)
	}
	parts := strings.Split(p, "/")
	for _, name := range parts {
		if err := ValidName(name); err != nil {
			return nil, fmt.Errorf("%w in %q", err, p)
		}
	}
	return parts, nil
}
