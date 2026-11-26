package gitbridge_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
)

func TestRemoveMirror(t *testing.T) {
	dir := t.TempDir()
	b := gitbridge.New(nil, blob.NewMem(), dir, nil)
	if ids, err := b.MirrorRepoIDs(); err != nil || len(ids) != 0 {
		t.Fatalf("no mirrors yet: %v, %v", ids, err)
	}
	id := uuid.New()
	if err := os.MkdirAll(filepath.Join(dir, id.String()+".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o644)           // not a mirror
	os.MkdirAll(filepath.Join(dir, id.String()+".git.broken-1"), 0o755) // set aside, not listed
	if ids, err := b.MirrorRepoIDs(); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("MirrorRepoIDs = %v, %v", ids, err)
	}
	if ok, err := b.RemoveMirror(id); err != nil || !ok {
		t.Fatalf("RemoveMirror = %v, %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, id.String()+".git")); !os.IsNotExist(err) {
		t.Fatal("the mirror is still there")
	}
	if ok, err := b.RemoveMirror(id); err != nil || ok {
		t.Fatalf("removing it again = %v, %v", ok, err)
	}
}
