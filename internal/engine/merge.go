// Package engine holds the operations that combine snapshots: three-way
// merges of trees and the rebase step used when landing changes.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
)

// ConflictKind names why a path could not be merged.
type ConflictKind string

const (
	ConflictContent      ConflictKind = "content"       // both sides edited the same lines
	ConflictAddAdd       ConflictKind = "add/add"       // both sides created the file differently
	ConflictModifyDelete ConflictKind = "modify/delete" // ours edited, theirs deleted
	ConflictDeleteModify ConflictKind = "delete/modify" // ours deleted, theirs edited
	ConflictMode         ConflictKind = "mode"          // both sides changed the mode differently
	ConflictBinary       ConflictKind = "binary"        // both sides changed a binary file
	ConflictPath         ConflictKind = "path"          // a file and a directory collide
)

// Conflict is one path that needs a decision.
type Conflict struct {
	Path   string
	Kind   ConflictKind
	Base   object.File // zero if the file did not exist in base
	Ours   object.File // zero if ours deleted it
	Theirs object.File // zero if theirs deleted it
	// Markers is the file with diff3-style conflict markers, for content
	// and add/add conflicts on text files.
	Markers []byte
	Detail  string
}

// MergeResult is the outcome of MergeTrees.
type MergeResult struct {
	// Tree is the merged root tree. It is Zero when there are conflicts.
	Tree hash.Hash
	// Conflicts lists paths that could not be merged.
	Conflicts []Conflict
	// AutoMerged lists paths both sides changed that merged line by line.
	AutoMerged []string
	// KeptBoth lists the paths in AutoMerged where both sides inserted
	// different lines at the same place and the merge kept both (the side
	// passed as theirs first). Worth a look: the order may matter.
	KeptBoth []string
}

// Clean reports whether the merge has no conflicts.
func (r MergeResult) Clean() bool { return len(r.Conflicts) == 0 }

// MergeTrees merges ours and theirs, which both descend from base. Paths
// changed on one side take that side. Paths changed on both sides are merged
// line by line when possible. With conflicts, the result has no tree.
func MergeTrees(ctx context.Context, s blob.Store, base, ours, theirs hash.Hash, labels merge.Labels) (MergeResult, error) {
	return mergeTrees(ctx, s, base, ours, theirs, labels, false)
}

// MergeTreesKeepConflicts is MergeTrees for a working copy: it always
// returns a tree. Text conflicts are written into the file with conflict
// markers; other conflicts keep our side. The caller shows the conflicts and
// the person or agent resolves them in place.
func MergeTreesKeepConflicts(ctx context.Context, s blob.Store, base, ours, theirs hash.Hash, labels merge.Labels) (MergeResult, error) {
	return mergeTrees(ctx, s, base, ours, theirs, labels, true)
}

func mergeTrees(ctx context.Context, s blob.Store, base, ours, theirs hash.Hash, labels merge.Labels, keep bool) (MergeResult, error) {
	var res MergeResult
	if ours == theirs || ours == base {
		res.Tree = theirs
		return res, nil
	}
	if theirs == base {
		res.Tree = ours
		return res, nil
	}
	oc, err := object.DiffTrees(ctx, s, base, ours)
	if err != nil {
		return res, err
	}
	tc, err := object.DiffTrees(ctx, s, base, theirs)
	if err != nil {
		return res, err
	}
	theirsByPath := make(map[string]object.Change, len(tc))
	for _, c := range tc {
		theirsByPath[c.Path] = c
	}

	var apply []object.Change
	for _, o := range oc {
		t, both := theirsByPath[o.Path]
		if !both {
			apply = append(apply, o)
			continue
		}
		switch {
		case o.Op == object.OpDelete && t.Op == object.OpDelete:
			continue
		case o.Op != object.OpDelete && t.Op != object.OpDelete && o.New == t.New:
			continue
		case o.Op == object.OpDelete:
			res.Conflicts = append(res.Conflicts, Conflict{Path: o.Path, Kind: ConflictDeleteModify, Base: o.Old, Theirs: t.New})
			// Theirs keeps its edit (it is the tree we apply onto), so no
			// work is lost; the conflict tells the caller ours deleted it.
			continue
		case t.Op == object.OpDelete:
			res.Conflicts = append(res.Conflicts, Conflict{Path: o.Path, Kind: ConflictModifyDelete, Base: o.Old, Ours: o.New})
			if keep {
				apply = append(apply, object.Change{Path: o.Path, Op: object.OpAdd, New: o.New})
			}
			continue
		}
		merged, keptBoth, conflict, err := mergeFile(ctx, s, o, t, labels)
		if err != nil {
			return res, err
		}
		if conflict != nil {
			res.Conflicts = append(res.Conflicts, *conflict)
			if keep {
				f := o.New
				if conflict.Markers != nil {
					h, err := s.Put(ctx, hash.KindBlob, conflict.Markers)
					if err != nil {
						return res, err
					}
					f = object.File{Hash: h, Mode: o.New.Mode}
				}
				apply = append(apply, object.Change{Path: o.Path, Op: object.OpModify, Old: t.New, New: f})
			}
			continue
		}
		res.AutoMerged = append(res.AutoMerged, o.Path)
		if keptBoth {
			res.KeptBoth = append(res.KeptBoth, o.Path)
		}
		apply = append(apply, object.Change{Path: o.Path, Op: object.OpModify, Old: t.New, New: merged})
	}
	if len(res.Conflicts) > 0 && !keep {
		return res, nil
	}
	tree, err := object.ApplyChanges(ctx, s, theirs, apply)
	if errors.Is(err, object.ErrBadPath) {
		res.Conflicts = append(res.Conflicts, Conflict{Kind: ConflictPath, Detail: err.Error()})
		return res, nil
	}
	if err != nil {
		return res, err
	}
	res.Tree = tree
	return res, nil
}

// mergeFile merges one path changed on both sides (neither deleted it).
// keptBoth reports a clean merge that kept both sides' insertions somewhere.
func mergeFile(ctx context.Context, s blob.Store, o, t object.Change, labels merge.Labels) (_ object.File, keptBoth bool, _ *Conflict, _ error) {
	base := o.Old // equal to t.Old; zero when both added the file
	kind := ConflictContent
	if o.Op == object.OpAdd {
		kind = ConflictAddAdd
	}
	conflict := &Conflict{Path: o.Path, Kind: kind, Base: base, Ours: o.New, Theirs: t.New}

	mode, ok := mergeMode(base.Mode, o.New.Mode, t.New.Mode, o.Op == object.OpAdd)
	if !ok {
		conflict.Kind = ConflictMode
		conflict.Detail = fmt.Sprintf("ours set %s, theirs set %s", o.New.Mode, t.New.Mode)
		return object.File{}, false, conflict, nil
	}
	if o.New.Hash == t.New.Hash {
		return object.File{Hash: o.New.Hash, Mode: mode}, false, nil, nil
	}
	if o.New.Mode == object.ModeSymlink || t.New.Mode == object.ModeSymlink {
		conflict.Detail = "symbolic links point to different targets"
		return object.File{}, false, conflict, nil
	}
	var baseData []byte
	if !base.Hash.IsZero() {
		var err error
		if baseData, err = blob.GetKind(ctx, s, hash.KindBlob, base.Hash); err != nil {
			return object.File{}, false, nil, err
		}
	}
	ourData, err := blob.GetKind(ctx, s, hash.KindBlob, o.New.Hash)
	if err != nil {
		return object.File{}, false, nil, err
	}
	theirData, err := blob.GetKind(ctx, s, hash.KindBlob, t.New.Hash)
	if err != nil {
		return object.File{}, false, nil, err
	}
	r, ok := merge.Merge3Bytes(baseData, ourData, theirData, labels)
	if !ok {
		conflict.Kind = ConflictBinary
		return object.File{}, false, conflict, nil
	}
	if r.Conflicts > 0 {
		conflict.Markers = r.Bytes()
		conflict.Detail = fmt.Sprintf("%d conflicting region(s)", r.Conflicts)
		return object.File{}, false, conflict, nil
	}
	h, err := s.Put(ctx, hash.KindBlob, r.Bytes())
	if err != nil {
		return object.File{}, false, nil, err
	}
	return object.File{Hash: h, Mode: mode}, r.KeptBoth > 0, nil, nil
}

// mergeMode picks the merged file mode. A side that kept the base mode
// yields to the side that changed it.
func mergeMode(base, ours, theirs object.Mode, added bool) (object.Mode, bool) {
	switch {
	case ours == theirs:
		return ours, true
	case added:
		return 0, false
	case ours == base:
		return theirs, true
	case theirs == base:
		return ours, true
	}
	return 0, false
}
