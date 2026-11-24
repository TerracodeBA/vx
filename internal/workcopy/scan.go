package workcopy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

// Always skipped, whatever the ignore files say.
var alwaysIgnored = map[string]bool{Dir: true, ".git": true}

// Scan walks the working copy and returns its files. Files whose size and
// modification time match the index are not read again. Contents of new or
// changed files are stored in the local object store as a side effect, so a
// snapshot never has to read them twice.
func (wc *WorkingCopy) Scan(ctx context.Context) (object.Manifest, error) {
	m := object.Manifest{}
	patterns := wc.rootPatterns()
	var walk func(dir string, parts []string, patterns []gitignore.Pattern) error
	walk = func(dir string, parts []string, patterns []gitignore.Pattern) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if gi, err := os.ReadFile(filepath.Join(dir, ".gitignore")); err == nil && len(parts) > 0 {
			patterns = append(patterns[:len(patterns):len(patterns)], parsePatterns(gi, parts)...)
		}
		matcher := gitignore.NewMatcher(patterns)
		for _, e := range entries {
			name := e.Name()
			if alwaysIgnored[name] {
				continue
			}
			childParts := append(parts[:len(parts):len(parts)], name)
			full := filepath.Join(dir, name)
			info, err := os.Lstat(full)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue // removed while scanning
				}
				return err
			}
			isDir := info.IsDir()
			if matcher.Match(childParts, isDir) {
				continue
			}
			if isDir {
				if err := walk(full, childParts, patterns); err != nil {
					return err
				}
				continue
			}
			rel := strings.Join(childParts, "/")
			f, err := wc.fileEntry(ctx, full, rel, info)
			if err != nil {
				return err
			}
			if f != nil {
				m[rel] = *f
			}
		}
		return nil
	}
	if err := walk(wc.Root, nil, patterns); err != nil {
		return nil, err
	}
	wc.index.prune(m)
	return m, nil
}

func (wc *WorkingCopy) rootPatterns() []gitignore.Pattern {
	var ps []gitignore.Pattern
	for _, name := range []string{".gitignore", ".vxignore"} {
		if data, err := os.ReadFile(filepath.Join(wc.Root, name)); err == nil {
			ps = append(ps, parsePatterns(data, nil)...)
		}
	}
	return ps
}

func parsePatterns(data []byte, domain []string) []gitignore.Pattern {
	var ps []gitignore.Pattern
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ps = append(ps, gitignore.ParsePattern(line, domain))
	}
	return ps
}

// fileEntry returns the manifest entry for one file, or nil for things vx
// does not track (sockets, devices).
func (wc *WorkingCopy) fileEntry(ctx context.Context, full, rel string, info fs.FileInfo) (*object.File, error) {
	mode := object.ModeFile
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		mode = object.ModeSymlink
	case !info.Mode().IsRegular():
		return nil, nil
	case info.Mode()&0o111 != 0:
		mode = object.ModeExec
	}
	if f, ok := wc.index.lookup(rel, info, mode); ok {
		return &f, nil
	}
	var data []byte
	var err error
	if mode == object.ModeSymlink {
		var target string
		target, err = os.Readlink(full)
		data = []byte(target)
	} else {
		data, err = os.ReadFile(full)
	}
	if err != nil {
		return nil, err
	}
	h, err := wc.Objects.Put(ctx, hash.KindBlob, data)
	if err != nil {
		return nil, err
	}
	f := object.File{Hash: h, Mode: mode}
	wc.index.store(rel, info, f)
	return &f, nil
}

// index caches file hashes by size and modification time.
type index struct {
	path    string
	mu      sync.Mutex
	Entries map[string]indexEntry `json:"entries"`
	dirty   bool
}

type indexEntry struct {
	Hash  hash.Hash `json:"hash"`
	Mode  string    `json:"mode"`
	Size  int64     `json:"size"`
	Mtime int64     `json:"mtime_ns"`
}

func newIndex(path string) *index {
	return &index{path: path, Entries: map[string]indexEntry{}}
}

func loadIndex(path string) (*index, error) {
	idx := newIndex(path)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, idx); err != nil {
		// A broken index only costs a rehash.
		return newIndex(path), nil
	}
	if idx.Entries == nil {
		idx.Entries = map[string]indexEntry{}
	}
	return idx, nil
}

// racyWindow: files modified this recently are not cached, because a later
// write within the same timestamp tick would go unnoticed.
const racyWindow = 2 * time.Second

func (x *index) lookup(rel string, info fs.FileInfo, mode object.Mode) (object.File, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.Entries[rel]
	if !ok || e.Size != info.Size() || e.Mtime != info.ModTime().UnixNano() || e.Mode != mode.String() {
		return object.File{}, false
	}
	return object.File{Hash: e.Hash, Mode: mode}, true
}

func (x *index) store(rel string, info fs.FileInfo, f object.File) {
	if time.Since(info.ModTime()) < racyWindow {
		return
	}
	x.mu.Lock()
	x.Entries[rel] = indexEntry{Hash: f.Hash, Mode: f.Mode.String(), Size: info.Size(), Mtime: info.ModTime().UnixNano()}
	x.dirty = true
	x.mu.Unlock()
}

// record caches a file vx just wrote with known content.
func (x *index) record(full, rel string, f object.File) {
	info, err := os.Lstat(full)
	if err != nil {
		return
	}
	x.store(rel, info, f)
}

func (x *index) remove(rel string) {
	x.mu.Lock()
	if _, ok := x.Entries[rel]; ok {
		delete(x.Entries, rel)
		x.dirty = true
	}
	x.mu.Unlock()
}

// prune drops entries for files no longer present.
func (x *index) prune(present object.Manifest) {
	x.mu.Lock()
	for p := range x.Entries {
		if _, ok := present[p]; !ok {
			delete(x.Entries, p)
			x.dirty = true
		}
	}
	x.mu.Unlock()
}

func (x *index) save() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.dirty {
		if _, err := os.Stat(x.path); err == nil {
			return nil
		}
	}
	data, err := json.Marshal(x)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(x.path, data, 0o644); err != nil {
		return err
	}
	x.dirty = false
	return nil
}
