package service

// Which change of a shipment wrote a line: the review comment on that line
// goes to its author. vx keeps the tree of every pushed commit
// (shipment_changes.tree), so the line is followed back through the
// commits, newest first, until the commit that inserted it.

import (
	"context"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// blameLine returns the change id of the shipment's commit that wrote line
// (1-based) of path as of commitSHA (a pushed commit of the shipment; any
// other value means its head). It returns "" when it cannot tell: a
// shipment from before v0.4, a line the pull request did not change, or a
// comment on the whole file (line 0), where the change that last touched
// the file is returned instead.
func (s *Service) blameLine(ctx context.Context, repo store.Repo, sh store.Shipment, path string, line int, commitSHA string) (string, error) {
	n := len(sh.Changes)
	if n == 0 || sh.BaseTree.IsZero() {
		return "", nil
	}
	for _, c := range sh.Changes {
		if c.Tree.IsZero() {
			return "", nil
		}
	}
	start := n - 1
	for i, c := range sh.Changes {
		if commitSHA != "" && c.GitSHA == commitSHA {
			start = i
		}
	}
	cur := line - 1 // 0-based index in the file of commit i
	for i := start; i >= 0; i-- {
		newTree := sh.Changes[i].Tree
		oldTree := sh.BaseTree
		if i > 0 {
			oldTree = sh.Changes[i-1].Tree
		}
		newFile, ok, err := s.fileAt(ctx, newTree, path)
		if err != nil || !ok {
			return "", err
		}
		oldFile, had, err := s.fileAt(ctx, oldTree, path)
		if err != nil {
			return "", err
		}
		if had && oldFile == newFile {
			continue // this commit left the file alone
		}
		if !had || line <= 0 {
			return sh.Changes[i].ChangeID, nil
		}
		oldData, err := blob.GetKind(ctx, s.Blobs, hash.KindBlob, oldFile)
		if err != nil {
			return "", err
		}
		newData, err := blob.GetKind(ctx, s.Blobs, hash.KindBlob, newFile)
		if err != nil {
			return "", err
		}
		prev, inserted := lineBefore(merge.SplitLines(oldData), merge.SplitLines(newData), cur)
		if inserted {
			return sh.Changes[i].ChangeID, nil
		}
		if prev < 0 {
			return "", nil // past the end of the file
		}
		cur = prev
	}
	return "", nil // the line was there before the pull request
}

// lineBefore maps line idx of b back to a: its index in a, or inserted
// when b's diff from a added it. -1 when idx is not a line of b.
func lineBefore(a, b []string, idx int) (int, bool) {
	if idx < 0 || idx >= len(b) {
		return -1, false
	}
	for _, e := range merge.Diff(a, b) {
		if idx < e.B0 || idx >= e.B1 {
			continue
		}
		if e.Kind == merge.Equal {
			return e.A0 + (idx - e.B0), false
		}
		return -1, true
	}
	return -1, false
}

// fileAt returns the blob of path in tree.
func (s *Service) fileAt(ctx context.Context, tree hash.Hash, path string) (hash.Hash, bool, error) {
	e, ok, err := object.Lookup(ctx, s.Blobs, tree, path)
	if err != nil || !ok || e.Mode == object.ModeDir {
		return hash.Zero, false, err
	}
	return e.Hash, true, nil
}
