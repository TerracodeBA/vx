package object

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
)

// ApplyChanges returns the tree that results from applying changes to root.
// Only the directories on the changed paths are read and rewritten, so the
// cost depends on the size of the change, not of the repository. Directories
// left empty are removed. Adds and modifies set the file; deletes remove it.
func ApplyChanges(ctx context.Context, s blob.Store, root hash.Hash, changes []Change) (hash.Hash, error) {
	rel := make([]relChange, 0, len(changes))
	for _, c := range changes {
		parts, err := SplitPath(c.Path)
		if err != nil {
			return hash.Zero, err
		}
		if c.Op != OpDelete && (c.New.Mode == ModeDir || !c.New.Mode.valid()) {
			return hash.Zero, fmt.Errorf("change to %q has invalid mode", c.Path)
		}
		rel = append(rel, relChange{parts: parts, c: c})
	}
	h, err := applyAt(ctx, s, root, rel, "")
	if err != nil {
		return hash.Zero, err
	}
	if h.IsZero() {
		return EmptyTree(ctx, s)
	}
	return h, nil
}

type relChange struct {
	parts []string
	c     Change
}

// applyAt applies changes to the tree h (which may be Zero for a new
// directory). It returns Zero if the resulting directory is empty.
//
// Changes run in three passes so that a file can replace a directory and a
// directory can replace a file in one batch: first deletes of entries in this
// directory, then changes inside subdirectories, then writes of entries here.
func applyAt(ctx context.Context, s blob.Store, h hash.Hash, changes []relChange, prefix string) (hash.Hash, error) {
	t, err := GetTree(ctx, s, h)
	if err != nil {
		return hash.Zero, err
	}
	entries := make(map[string]Entry, len(t.Entries))
	for _, e := range t.Entries {
		entries[e.Name] = e
	}
	var writes []relChange
	sub := map[string][]relChange{}
	for _, rc := range changes {
		name := rc.parts[0]
		switch {
		case len(rc.parts) > 1:
			sub[name] = append(sub[name], relChange{parts: rc.parts[1:], c: rc.c})
		case rc.c.Op == OpDelete:
			e, ok := entries[name]
			if !ok || e.Mode == ModeDir {
				return hash.Zero, fmt.Errorf("cannot delete %q: no such file", rc.c.Path)
			}
			delete(entries, name)
		default:
			writes = append(writes, rc)
		}
	}
	names := make([]string, 0, len(sub))
	for name := range sub {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var base hash.Hash
		if e, ok := entries[name]; ok {
			if e.Mode != ModeDir {
				return hash.Zero, fmt.Errorf("%w: %q is a file, cannot create files inside it", ErrBadPath, joinPath(prefix, name))
			}
			base = e.Hash
		}
		nh, err := applyAt(ctx, s, base, sub[name], joinPath(prefix, name))
		if err != nil {
			return hash.Zero, err
		}
		if nh.IsZero() {
			delete(entries, name)
			continue
		}
		entries[name] = Entry{Name: name, Mode: ModeDir, Hash: nh}
	}
	for _, rc := range writes {
		name := rc.parts[0]
		if e, ok := entries[name]; ok && e.Mode == ModeDir {
			return hash.Zero, fmt.Errorf("%w: %q is a directory, cannot write a file there", ErrBadPath, rc.c.Path)
		}
		entries[name] = Entry{Name: name, Mode: rc.c.New.Mode, Hash: rc.c.New.Hash}
	}
	if len(entries) == 0 {
		return hash.Zero, nil
	}
	out := &Tree{Entries: make([]Entry, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return PutTree(ctx, s, out)
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// ChangedDirs returns the set of directories touched by changes, useful for
// display ("internal/http").
func ChangedDirs(changes []Change) []string {
	seen := map[string]bool{}
	for _, c := range changes {
		dir := "."
		if i := strings.LastIndexByte(c.Path, '/'); i >= 0 {
			dir = c.Path[:i]
		}
		seen[dir] = true
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
