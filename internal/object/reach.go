package object

import (
	"context"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
)

// NewObjects lists the trees and blobs reachable from tree b that are not
// reachable through the same path in tree a. These are the objects a client
// must upload when the server already has a. Subtrees with equal hashes are
// skipped, so the cost depends on the size of the change.
func NewObjects(ctx context.Context, s blob.Store, a, b hash.Hash) (trees, blobs []hash.Hash, err error) {
	seen := map[hash.Hash]bool{}
	var walk func(a, b hash.Hash) error
	walk = func(a, b hash.Hash) error {
		if a == b || seen[b] {
			return nil
		}
		seen[b] = true
		trees = append(trees, b)
		ta, err := GetTree(ctx, s, a)
		if err != nil {
			return err
		}
		tb, err := GetTree(ctx, s, b)
		if err != nil {
			return err
		}
		for _, e := range tb.Entries {
			old, ok := ta.Find(e.Name)
			if e.Mode == ModeDir {
				var oldTree hash.Hash
				if ok && old.Mode == ModeDir {
					oldTree = old.Hash
				}
				if err := walk(oldTree, e.Hash); err != nil {
					return err
				}
				continue
			}
			if ok && old.Hash == e.Hash {
				continue
			}
			if !seen[e.Hash] {
				seen[e.Hash] = true
				blobs = append(blobs, e.Hash)
			}
		}
		return nil
	}
	if b.IsZero() {
		return nil, nil, nil
	}
	err = walk(a, b)
	return trees, blobs, err
}
