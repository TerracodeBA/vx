package object

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
)

// File is a file's content hash and mode.
type File struct {
	Hash hash.Hash
	Mode Mode // ModeFile, ModeExec or ModeSymlink
}

// Manifest is a flat view of a snapshot: every file path and its content.
type Manifest map[string]File

// Paths returns the manifest's paths in sorted order.
func (m Manifest) Paths() []string {
	ps := make([]string, 0, len(m))
	for p := range m {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// Clone returns a copy of the manifest.
func (m Manifest) Clone() Manifest {
	c := make(Manifest, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// ReadManifest flattens the tree at root into a manifest.
func ReadManifest(ctx context.Context, s blob.Store, root hash.Hash) (Manifest, error) {
	m := Manifest{}
	var walk func(h hash.Hash, prefix string) error
	walk = func(h hash.Hash, prefix string) error {
		t, err := GetTree(ctx, s, h)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			if e.Mode == ModeDir {
				if err := walk(e.Hash, p); err != nil {
					return err
				}
				continue
			}
			m[p] = File{Hash: e.Hash, Mode: e.Mode}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return m, nil
}

// WriteManifest stores the trees for m and returns the root tree hash.
// File contents must already be in the store; WriteManifest only writes trees.
func WriteManifest(ctx context.Context, s blob.Store, m Manifest) (hash.Hash, error) {
	type dir struct {
		files map[string]File
		dirs  map[string]*dir
	}
	newDir := func() *dir { return &dir{files: map[string]File{}, dirs: map[string]*dir{}} }
	root := newDir()
	for p, f := range m {
		if f.Mode == ModeDir || !f.Mode.valid() {
			return hash.Zero, fmt.Errorf("manifest entry %q has invalid mode %v", p, f.Mode)
		}
		parts, err := SplitPath(p)
		if err != nil {
			return hash.Zero, err
		}
		d := root
		for _, name := range parts[:len(parts)-1] {
			if _, clash := d.files[name]; clash {
				return hash.Zero, fmt.Errorf("%w: %q is both a file and a directory", ErrBadPath, name)
			}
			sub, ok := d.dirs[name]
			if !ok {
				sub = newDir()
				d.dirs[name] = sub
			}
			d = sub
		}
		leaf := parts[len(parts)-1]
		if _, clash := d.dirs[leaf]; clash {
			return hash.Zero, fmt.Errorf("%w: %q is both a file and a directory", ErrBadPath, p)
		}
		d.files[leaf] = f
	}
	var write func(d *dir) (hash.Hash, error)
	write = func(d *dir) (hash.Hash, error) {
		t := &Tree{Entries: make([]Entry, 0, len(d.files)+len(d.dirs))}
		for name, f := range d.files {
			t.Entries = append(t.Entries, Entry{Name: name, Mode: f.Mode, Hash: f.Hash})
		}
		for name, sub := range d.dirs {
			h, err := write(sub)
			if err != nil {
				return hash.Zero, err
			}
			t.Entries = append(t.Entries, Entry{Name: name, Mode: ModeDir, Hash: h})
		}
		sort.Slice(t.Entries, func(i, j int) bool { return t.Entries[i].Name < t.Entries[j].Name })
		return PutTree(ctx, s, t)
	}
	return write(root)
}

// Op is the kind of a file change.
type Op string

const (
	OpAdd    Op = "add"
	OpModify Op = "modify"
	OpDelete Op = "delete"
)

// Change is one file that differs between two snapshots.
type Change struct {
	Path string
	Op   Op
	Old  File // zero for OpAdd
	New  File // zero for OpDelete
}

// DiffTrees lists file changes from tree a to tree b. Subtrees with equal
// hashes are skipped without being read, so the cost is proportional to the
// size of the change, not the size of the repository.
func DiffTrees(ctx context.Context, s blob.Store, a, b hash.Hash) ([]Change, error) {
	var out []Change
	var walk func(a, b hash.Hash, prefix string) error
	join := func(prefix, name string) string {
		if prefix == "" {
			return name
		}
		return prefix + "/" + name
	}
	// emitAll lists every file under tree h as added or deleted.
	var emitAll func(h hash.Hash, prefix string, op Op) error
	emitAll = func(h hash.Hash, prefix string, op Op) error {
		t, err := GetTree(ctx, s, h)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			p := join(prefix, e.Name)
			if e.Mode == ModeDir {
				if err := emitAll(e.Hash, p, op); err != nil {
					return err
				}
				continue
			}
			f := File{Hash: e.Hash, Mode: e.Mode}
			if op == OpAdd {
				out = append(out, Change{Path: p, Op: OpAdd, New: f})
			} else {
				out = append(out, Change{Path: p, Op: OpDelete, Old: f})
			}
		}
		return nil
	}
	walk = func(a, b hash.Hash, prefix string) error {
		if a == b {
			return nil
		}
		ta, err := GetTree(ctx, s, a)
		if err != nil {
			return err
		}
		tb, err := GetTree(ctx, s, b)
		if err != nil {
			return err
		}
		i, j := 0, 0
		for i < len(ta.Entries) || j < len(tb.Entries) {
			var ea, eb *Entry
			switch {
			case i >= len(ta.Entries):
				eb = &tb.Entries[j]
				j++
			case j >= len(tb.Entries):
				ea = &ta.Entries[i]
				i++
			case ta.Entries[i].Name < tb.Entries[j].Name:
				ea = &ta.Entries[i]
				i++
			case ta.Entries[i].Name > tb.Entries[j].Name:
				eb = &tb.Entries[j]
				j++
			default:
				ea, eb = &ta.Entries[i], &tb.Entries[j]
				i++
				j++
			}
			if err := diffEntry(ea, eb, prefix, join, walk, emitAll, &out); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(a, b, ""); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func diffEntry(
	ea, eb *Entry, prefix string,
	join func(string, string) string,
	walk func(a, b hash.Hash, prefix string) error,
	emitAll func(h hash.Hash, prefix string, op Op) error,
	out *[]Change,
) error {
	switch {
	case ea == nil:
		p := join(prefix, eb.Name)
		if eb.Mode == ModeDir {
			return emitAll(eb.Hash, p, OpAdd)
		}
		*out = append(*out, Change{Path: p, Op: OpAdd, New: File{eb.Hash, eb.Mode}})
	case eb == nil:
		p := join(prefix, ea.Name)
		if ea.Mode == ModeDir {
			return emitAll(ea.Hash, p, OpDelete)
		}
		*out = append(*out, Change{Path: p, Op: OpDelete, Old: File{ea.Hash, ea.Mode}})
	default:
		p := join(prefix, ea.Name)
		aDir, bDir := ea.Mode == ModeDir, eb.Mode == ModeDir
		switch {
		case aDir && bDir:
			return walk(ea.Hash, eb.Hash, p)
		case aDir && !bDir:
			if err := emitAll(ea.Hash, p, OpDelete); err != nil {
				return err
			}
			*out = append(*out, Change{Path: p, Op: OpAdd, New: File{eb.Hash, eb.Mode}})
		case !aDir && bDir:
			*out = append(*out, Change{Path: p, Op: OpDelete, Old: File{ea.Hash, ea.Mode}})
			return emitAll(eb.Hash, p, OpAdd)
		default:
			if ea.Hash != eb.Hash || ea.Mode != eb.Mode {
				*out = append(*out, Change{Path: p, Op: OpModify, Old: File{ea.Hash, ea.Mode}, New: File{eb.Hash, eb.Mode}})
			}
		}
	}
	return nil
}

// DiffManifests lists file changes from a to b.
func DiffManifests(a, b Manifest) []Change {
	var out []Change
	for p, fa := range a {
		fb, ok := b[p]
		switch {
		case !ok:
			out = append(out, Change{Path: p, Op: OpDelete, Old: fa})
		case fa != fb:
			out = append(out, Change{Path: p, Op: OpModify, Old: fa, New: fb})
		}
	}
	for p, fb := range b {
		if _, ok := a[p]; !ok {
			out = append(out, Change{Path: p, Op: OpAdd, New: fb})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Apply returns m with changes applied.
func (m Manifest) Apply(changes []Change) Manifest {
	out := m.Clone()
	for _, c := range changes {
		if c.Op == OpDelete {
			delete(out, c.Path)
		} else {
			out[c.Path] = c.New
		}
	}
	return out
}

// HasPrefixPath reports whether path is dir or inside dir.
func HasPrefixPath(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
}
