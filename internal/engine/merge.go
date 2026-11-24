// Package engine holds the operations that combine snapshots: three-way
// merges of trees and the rebase step used when landing changes.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	// MergedByDecl lists the .go paths in AutoMerged whose lines conflicted
	// but whose top-level declarations did not: each side changed different
	// functions, methods, types, vars or consts, so the file was rebuilt
	// from both sides' declarations, imports united, and gofmt'ed. Worth a
	// look, like KeptBoth.
	MergedByDecl []string
	// Generated lists generated files both sides changed. They are never
	// text-merged: the result has ours (the side being landed, synced,
	// shipped or replayed), and the file has to be regenerated.
	Generated []GeneratedFile
}

// GeneratedFile is a generated file both sides changed, kept from ours.
type GeneratedFile struct {
	Path string
	// Command regenerates it, from a [[generated]] rule in vx.toml; empty
	// when the rule has none or the file was recognised by its
	// "Code generated ... DO NOT EDIT." line.
	Command string
}

// MergeOption changes how MergeTrees treats some paths.
type MergeOption func(*mergeConfig)

type mergeConfig struct {
	generated func(path string) (command string, ok bool)
}

// WithGenerated says which paths are generated (vx.toml [[generated]]
// rules, see rules.Rules.GeneratedMatcher). Files that say "Code generated
// ... DO NOT EDIT." are generated with or without this option.
func WithGenerated(f func(path string) (command string, ok bool)) MergeOption {
	return func(c *mergeConfig) { c.generated = f }
}

// WarningGenerated: both sides changed a generated file; the merge kept
// ours and the file needs regenerating.
const WarningGenerated WarningKind = "generated"

// GeneratedWarnings turns generated files into warnings, sorted by path:
//
//	generated file gen/x.go: regenerate with `buf generate`
func GeneratedWarnings(gs []GeneratedFile) []Warning {
	out := make([]Warning, 0, len(gs))
	for _, g := range gs {
		d := fmt.Sprintf("generated file %s: regenerate with `%s`", g.Path, g.Command)
		if g.Command == "" {
			d = fmt.Sprintf("generated file %s: regenerate it (both sides changed it; vx kept one side instead of merging the text)", g.Path)
		}
		out = append(out, Warning{Path: g.Path, Kind: WarningGenerated, Name: g.Command, Detail: d})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Clean reports whether the merge has no conflicts.
func (r MergeResult) Clean() bool { return len(r.Conflicts) == 0 }

// MergeTrees merges ours and theirs, which both descend from base. Paths
// changed on one side take that side. Paths changed on both sides are merged
// line by line when possible. With conflicts, the result has no tree.
//
// Generated files (see WithGenerated) changed on both sides are not merged:
// ours wins and the path is listed in Generated.
func MergeTrees(ctx context.Context, s blob.Store, base, ours, theirs hash.Hash, labels merge.Labels, opts ...MergeOption) (MergeResult, error) {
	return mergeTrees(ctx, s, base, ours, theirs, labels, false, opts)
}

// MergeTreesKeepConflicts is MergeTrees for a working copy: it always
// returns a tree. Text conflicts are written into the file with conflict
// markers; other conflicts keep our side. The caller shows the conflicts and
// the person or agent resolves them in place.
func MergeTreesKeepConflicts(ctx context.Context, s blob.Store, base, ours, theirs hash.Hash, labels merge.Labels, opts ...MergeOption) (MergeResult, error) {
	return mergeTrees(ctx, s, base, ours, theirs, labels, true, opts)
}

func mergeTrees(ctx context.Context, s blob.Store, base, ours, theirs hash.Hash, labels merge.Labels, keep bool, opts []MergeOption) (MergeResult, error) {
	var cfg mergeConfig
	for _, o := range opts {
		o(&cfg)
	}
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
		}
		if cmd, gen, err := cfg.isGenerated(ctx, s, o, t); err != nil {
			return res, err
		} else if gen {
			res.Generated = append(res.Generated, GeneratedFile{Path: o.Path, Command: cmd})
			switch {
			case o.Op == object.OpDelete:
				apply = append(apply, object.Change{Path: o.Path, Op: object.OpDelete, Old: t.New})
			case t.Op == object.OpDelete:
				apply = append(apply, object.Change{Path: o.Path, Op: object.OpAdd, New: o.New})
			default:
				apply = append(apply, object.Change{Path: o.Path, Op: object.OpModify, Old: t.New, New: o.New})
			}
			continue
		}
		switch {
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
		merged, how, conflict, err := mergeFile(ctx, s, o, t, labels)
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
		switch how {
		case mergedKeptBoth:
			res.KeptBoth = append(res.KeptBoth, o.Path)
		case mergedByDecl:
			res.MergedByDecl = append(res.MergedByDecl, o.Path)
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

// isGenerated reports whether a path both sides changed is generated: a
// rule says so, or one of its versions says "Code generated ... DO NOT
// EDIT.".
func (c mergeConfig) isGenerated(ctx context.Context, s blob.Store, o, t object.Change) (string, bool, error) {
	if c.generated != nil {
		if cmd, ok := c.generated(o.Path); ok {
			return cmd, true, nil
		}
	}
	for _, f := range []object.File{o.New, t.New, o.Old} {
		if f.Hash.IsZero() || f.Mode == object.ModeSymlink {
			continue
		}
		data, err := blob.GetKind(ctx, s, hash.KindBlob, f.Hash)
		if err != nil {
			return "", false, err
		}
		if IsGenerated(data) {
			return "", true, nil
		}
	}
	return "", false, nil
}

// mergeHow says how a file merged without conflicts.
type mergeHow int

const (
	mergedLines    mergeHow = iota // line by line
	mergedKeptBoth                 // line by line, keeping both sides' insertions somewhere
	mergedByDecl                   // the lines conflicted, the Go declarations did not
)

// mergeFile merges one path changed on both sides (neither deleted it).
// how says how a clean merge was made.
func mergeFile(ctx context.Context, s blob.Store, o, t object.Change, labels merge.Labels) (_ object.File, how mergeHow, _ *Conflict, _ error) {
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
		return object.File{}, mergedLines, conflict, nil
	}
	if o.New.Hash == t.New.Hash {
		return object.File{Hash: o.New.Hash, Mode: mode}, mergedLines, nil, nil
	}
	if o.New.Mode == object.ModeSymlink || t.New.Mode == object.ModeSymlink {
		conflict.Detail = "symbolic links point to different targets"
		return object.File{}, mergedLines, conflict, nil
	}
	var baseData []byte
	if !base.Hash.IsZero() {
		var err error
		if baseData, err = blob.GetKind(ctx, s, hash.KindBlob, base.Hash); err != nil {
			return object.File{}, mergedLines, nil, err
		}
	}
	ourData, err := blob.GetKind(ctx, s, hash.KindBlob, o.New.Hash)
	if err != nil {
		return object.File{}, mergedLines, nil, err
	}
	theirData, err := blob.GetKind(ctx, s, hash.KindBlob, t.New.Hash)
	if err != nil {
		return object.File{}, mergedLines, nil, err
	}
	r, ok := merge.Merge3Bytes(baseData, ourData, theirData, labels)
	if !ok {
		conflict.Kind = ConflictBinary
		return object.File{}, mergedLines, conflict, nil
	}
	if r.Conflicts > 0 && !base.Hash.IsZero() && strings.HasSuffix(o.Path, ".go") {
		if data, ok := mergeGoDecls(baseData, ourData, theirData); ok {
			h, err := s.Put(ctx, hash.KindBlob, data)
			if err != nil {
				return object.File{}, mergedLines, nil, err
			}
			return object.File{Hash: h, Mode: mode}, mergedByDecl, nil, nil
		}
	}
	if r.Conflicts > 0 {
		conflict.Markers = r.Bytes()
		conflict.Detail = fmt.Sprintf("%d conflicting region(s)", r.Conflicts)
		return object.File{}, mergedLines, conflict, nil
	}
	h, err := s.Put(ctx, hash.KindBlob, r.Bytes())
	if err != nil {
		return object.File{}, mergedLines, nil, err
	}
	if r.KeptBoth > 0 {
		return object.File{Hash: h, Mode: mode}, mergedKeptBoth, nil, nil
	}
	return object.File{Hash: h, Mode: mode}, mergedLines, nil, nil
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
