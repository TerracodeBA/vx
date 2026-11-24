package workcopy

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

func write(t *testing.T, root, rel, content string, perm os.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func paths(m object.Manifest) []string {
	ps := m.Paths()
	sort.Strings(ps)
	return ps
}

func TestScanIgnoresAndModes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	wc, err := Create(root, Config{Repo: "r", Effort: "e"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "main.go", "package main\n", 0o644)
	write(t, root, "run.sh", "#!/bin/sh\n", 0o755)
	write(t, root, "build/out.bin", "x", 0o644)
	write(t, root, "pkg/a.go", "a", 0o644)
	write(t, root, "pkg/secret.env", "s", 0o644)
	write(t, root, "pkg/.gitignore", "*.env\n", 0o644)
	write(t, root, ".gitignore", "build/\n", 0o644)
	write(t, root, ".git/HEAD", "ref", 0o644)
	if err := os.Symlink("main.go", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	m, err := wc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "link", "main.go", "pkg/.gitignore", "pkg/a.go", "run.sh"}
	got := paths(m)
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
	if m["run.sh"].Mode != object.ModeExec || m["link"].Mode != object.ModeSymlink || m["main.go"].Mode != object.ModeFile {
		t.Fatalf("modes: %+v", m)
	}
	if m["link"].Hash != hash.Of(hash.KindBlob, []byte("main.go")) {
		t.Fatal("symlink content must be its target")
	}
	// Contents are in the local store after a scan.
	if _, err := wc.ReadBlob(ctx, m["pkg/a.go"].Hash); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotApplyRoundTrip(t *testing.T) {
	ctx := context.Background()
	a := t.TempDir()
	wa, _ := Create(a, Config{})
	write(t, a, "src/one.go", "one\n", 0o644)
	write(t, a, "src/two.go", "two\n", 0o644)
	write(t, a, "tool.sh", "echo\n", 0o755)
	tree, changes, err := wa.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %d", len(changes))
	}
	// A second working copy applies the same changes from the shared objects.
	b := t.TempDir()
	wb, _ := Create(b, Config{})
	wb.Objects = wa.Objects
	if err := wb.Apply(ctx, changes); err != nil {
		t.Fatal(err)
	}
	wb.Config.BaseTree = tree.String()
	st, err := wb.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 0 {
		t.Fatalf("copy should match its base, got %+v", st)
	}
	info, _ := os.Stat(filepath.Join(b, "tool.sh"))
	if info.Mode()&0o111 == 0 {
		t.Fatal("exec bit lost")
	}
	// Deleting a whole directory prunes it.
	var del []object.Change
	for _, c := range changes {
		if filepath.Dir(c.Path) == "src" {
			del = append(del, object.Change{Path: c.Path, Op: object.OpDelete, Old: c.New})
		}
	}
	if err := wb.Apply(ctx, del); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, "src")); !os.IsNotExist(err) {
		t.Fatal("empty directory should be removed")
	}
	if err := wb.Save(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Find(filepath.Join(b))
	if err != nil || reopened.Config.BaseTree != tree.String() {
		t.Fatalf("Find = %v", err)
	}
}

func TestConflictMarkers(t *testing.T) {
	if !HasConflictMarkers([]byte("a\n<<<<<<< workspace\nx\n=======\ny\n>>>>>>> effort\n")) {
		t.Fatal("markers not detected")
	}
	if HasConflictMarkers([]byte("a\n=======\nb\n")) {
		t.Fatal("a lone separator is not a conflict")
	}
}
