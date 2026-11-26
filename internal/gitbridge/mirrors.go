package gitbridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// RemoveMirror deletes the bare mirror of a repository on this server, for
// the purge of a deleted organization. It reports whether there was one.
func (b *Bridge) RemoveMirror(repoID uuid.UUID) (bool, error) {
	unlock := b.lock(repoID)
	defer unlock()
	path := filepath.Join(b.Dir, repoID.String()+".git")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return false, fmt.Errorf("remove git mirror %s: %w", path, err)
	}
	b.mu.Lock()
	delete(b.locks, repoID)
	b.mu.Unlock()
	return true, nil
}

// MirrorRepoIDs lists the repositories that have a mirror on this server.
func (b *Bridge) MirrorRepoIDs() ([]uuid.UUID, error) {
	entries, err := os.ReadDir(b.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".git")
		if !ok || !e.IsDir() {
			continue
		}
		if id, err := uuid.Parse(name); err == nil {
			out = append(out, id)
		}
	}
	return out, nil
}
