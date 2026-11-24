// Package workcopy manages a vx working copy on disk: the files, the .vx
// directory with its config, a local object cache and a stat index that
// avoids rehashing unchanged files.
package workcopy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

// Dir is the name of the metadata directory at the root of a working copy.
const Dir = ".vx"

// Config is what a working copy knows about where it came from.
type Config struct {
	Repo   string `json:"repo"`
	Effort string `json:"effort"`
	// Base is the changeset the files are based on, and BaseTree its tree.
	Base     string `json:"base"`
	BaseTree string `json:"base_tree"`
	// Workspace is the open workspace, if any.
	Workspace string `json:"workspace,omitempty"`
	Intent    string `json:"intent,omitempty"`
	ChangeID  string `json:"change_id,omitempty"`
	// PendingLand holds the evidence flags of a land that did not finish
	// (conflicts, failed checks), so a bare `vx land` can run it again.
	PendingLand *PendingLand `json:"pending_land,omitempty"`
}

// PendingLand is the evidence a `vx land` asked for, kept until a land in
// the same workspace succeeds.
type PendingLand struct {
	Workspace       string   `json:"workspace"`
	Runs            []string `json:"runs,omitempty"`
	Checks          []string `json:"checks,omitempty"`
	NewTests        []string `json:"new_tests,omitempty"`
	DependsOn       []string `json:"depends_on,omitempty"`
	AllowFailing    bool     `json:"allow_failing,omitempty"`
	AllowUnverified bool     `json:"allow_unverified,omitempty"`
}

// BaseHash returns the parsed base changeset.
func (c Config) BaseHash() hash.Hash { h, _ := hash.Parse(c.Base); return h }

// BaseTreeHash returns the parsed base tree.
func (c Config) BaseTreeHash() hash.Hash { h, _ := hash.Parse(c.BaseTree); return h }

// WorkingCopy is an opened working copy.
type WorkingCopy struct {
	Root    string
	Config  Config
	Objects blob.Store
	index   *index
}

// ErrNotFound means no working copy contains the directory.
var ErrNotFound = errors.New("not inside a vx working copy (no .vx directory found); use `vx clone` first")

// Find opens the working copy containing dir.
func Find(dir string) (*WorkingCopy, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if st, err := os.Stat(filepath.Join(d, Dir, "config.json")); err == nil && !st.IsDir() {
			return Open(d)
		}
		if filepath.Dir(d) == d {
			return nil, ErrNotFound
		}
	}
}

// Open opens the working copy rooted at root.
func Open(root string) (*WorkingCopy, error) {
	data, err := os.ReadFile(filepath.Join(root, Dir, "config.json"))
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s/config.json: %w", Dir, err)
	}
	objs, err := blob.NewDisk(filepath.Join(root, Dir, "objects"))
	if err != nil {
		return nil, err
	}
	idx, err := loadIndex(filepath.Join(root, Dir, "index.json"))
	if err != nil {
		return nil, err
	}
	return &WorkingCopy{Root: root, Config: cfg, Objects: objs, index: idx}, nil
}

// Create makes a new working copy at root, which must be empty or missing.
func Create(root string, cfg Config) (*WorkingCopy, error) {
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty", root)
	}
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		return nil, err
	}
	objs, err := blob.NewDisk(filepath.Join(root, Dir, "objects"))
	if err != nil {
		return nil, err
	}
	wc := &WorkingCopy{Root: root, Config: cfg, Objects: objs, index: newIndex(filepath.Join(root, Dir, "index.json"))}
	return wc, wc.Save()
}

// Save writes the config and index.
func (wc *WorkingCopy) Save() error {
	data, err := json.MarshalIndent(wc.Config, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(wc.Root, Dir, "config.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return wc.index.save()
}

// BaseManifest returns the files of the base snapshot.
func (wc *WorkingCopy) BaseManifest(ctx context.Context) (object.Manifest, error) {
	return object.ReadManifest(ctx, wc.Objects, wc.Config.BaseTreeHash())
}

// Status lists the local changes relative to the base snapshot.
func (wc *WorkingCopy) Status(ctx context.Context) ([]object.Change, error) {
	base, err := wc.BaseManifest(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := wc.Scan(ctx)
	if err != nil {
		return nil, err
	}
	return object.DiffManifests(base, cur), nil
}

// Snapshot stores the current files as a tree and returns it with the
// changes relative to the base. File contents are already in the local
// store after Scan; only the changed directories' trees are written.
func (wc *WorkingCopy) Snapshot(ctx context.Context) (hash.Hash, []object.Change, error) {
	changes, err := wc.Status(ctx)
	if err != nil {
		return hash.Zero, nil, err
	}
	tree, err := object.ApplyChanges(ctx, wc.Objects, wc.Config.BaseTreeHash(), changes)
	if err != nil {
		return hash.Zero, nil, err
	}
	return tree, changes, nil
}

// ReadFile returns the content of a blob from the local store.
func (wc *WorkingCopy) ReadBlob(ctx context.Context, h hash.Hash) ([]byte, error) {
	if h.IsZero() {
		return nil, nil
	}
	return blob.GetKind(ctx, wc.Objects, hash.KindBlob, h)
}

// Apply writes changes to disk: deletes first (pruning emptied
// directories), then writes. The index is updated for every written file.
func (wc *WorkingCopy) Apply(ctx context.Context, changes []object.Change) error {
	for _, c := range changes {
		if c.Op != object.OpDelete {
			continue
		}
		p := wc.abs(c.Path)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		wc.index.remove(c.Path)
		wc.pruneEmptyDirs(filepath.Dir(p))
	}
	for _, c := range changes {
		if c.Op == object.OpDelete {
			continue
		}
		data, err := wc.ReadBlob(ctx, c.New.Hash)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Path, err)
		}
		if err := wc.writeFile(c.Path, c.New.Mode, data); err != nil {
			return fmt.Errorf("%s: %w", c.Path, err)
		}
		wc.index.record(wc.abs(c.Path), c.Path, c.New)
	}
	return nil
}

func (wc *WorkingCopy) abs(rel string) string {
	return filepath.Join(wc.Root, filepath.FromSlash(rel))
}

func (wc *WorkingCopy) writeFile(rel string, mode object.Mode, data []byte) error {
	p := wc.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// A directory in the way is removed only if it is empty.
	if st, err := os.Lstat(p); err == nil && st.IsDir() {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("a directory is in the way: %w", err)
		}
	}
	if mode == object.ModeSymlink {
		_ = os.Remove(p)
		return os.Symlink(string(data), p)
	}
	perm := fs.FileMode(0o644)
	if mode == object.ModeExec {
		perm = 0o755
	}
	if st, err := os.Lstat(p); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		_ = os.Remove(p)
	}
	return writeFileAtomic(p, data, perm)
}

func (wc *WorkingCopy) pruneEmptyDirs(dir string) {
	for dir != wc.Root && strings.HasPrefix(dir, wc.Root) {
		if err := os.Remove(dir); err != nil {
			return // not empty or already gone
		}
		dir = filepath.Dir(dir)
	}
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vx-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// HasConflictMarkers reports whether text contains unresolved conflict
// markers written by vx sync.
func HasConflictMarkers(data []byte) bool {
	s := string(data)
	return (strings.HasPrefix(s, "<<<<<<< ") || strings.Contains(s, "\n<<<<<<< ")) &&
		(strings.Contains(s, "\n=======\n")) &&
		(strings.Contains(s, "\n>>>>>>> "))
}
