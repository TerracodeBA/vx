package object

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
)

func putFile(t *testing.T, s blob.Store, content string) hash.Hash {
	t.Helper()
	h, err := s.Put(context.Background(), hash.KindBlob, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestTreeEncodeDecode(t *testing.T) {
	tr := &Tree{Entries: []Entry{
		{Name: "a.go", Mode: ModeFile, Hash: hash.Of(hash.KindBlob, []byte("a"))},
		{Name: "bin", Mode: ModeExec, Hash: hash.Of(hash.KindBlob, []byte("b"))},
		{Name: "pkg", Mode: ModeDir, Hash: hash.Of(hash.KindTree, []byte("c"))},
	}}
	data, err := tr.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeTree(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, tr) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	bad := &Tree{Entries: []Entry{{Name: "b"}, {Name: "a"}}}
	bad.Entries[0].Mode, bad.Entries[1].Mode = ModeFile, ModeFile
	if _, err := bad.Encode(); err == nil {
		t.Fatal("unsorted tree should not encode")
	}
	for _, name := range []string{"", ".", "..", "a/b", "a\x00b"} {
		if err := ValidName(name); !errors.Is(err, ErrBadPath) {
			t.Errorf("ValidName(%q) = %v", name, err)
		}
	}
}

func TestManifestRoundTripAndDiff(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMem()
	m := Manifest{
		"README.md":                  {putFile(t, s, "# hi"), ModeFile},
		"internal/http/routes.go":    {putFile(t, s, "routes"), ModeFile},
		"internal/http/ratelimit.go": {putFile(t, s, "limit"), ModeFile},
		"internal/auth/login.go":     {putFile(t, s, "login"), ModeFile},
		"scripts/run.sh":             {putFile(t, s, "#!/bin/sh"), ModeExec},
	}
	root, err := WriteManifest(ctx, s, m)
	if err != nil {
		t.Fatal(err)
	}
	again, err := WriteManifest(ctx, s, m)
	if err != nil || again != root {
		t.Fatal("WriteManifest must be deterministic")
	}
	got, err := ReadManifest(ctx, s, root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("manifest mismatch:\n%v\n%v", got, m)
	}
	e, ok, err := Lookup(ctx, s, root, "internal/http/routes.go")
	if err != nil || !ok || e.Hash != m["internal/http/routes.go"].Hash {
		t.Fatal("Lookup failed")
	}
	if _, ok, _ := Lookup(ctx, s, root, "internal/nope.go"); ok {
		t.Fatal("Lookup found a missing path")
	}

	m2 := m.Clone()
	m2["internal/http/routes.go"] = File{putFile(t, s, "routes v2"), ModeFile}
	delete(m2, "internal/auth/login.go")
	m2["internal/audit/audit.go"] = File{putFile(t, s, "audit"), ModeFile}
	m2["scripts/run.sh"] = File{m["scripts/run.sh"].Hash, ModeFile} // mode change
	root2, err := WriteManifest(ctx, s, m2)
	if err != nil {
		t.Fatal(err)
	}
	treeDiff, err := DiffTrees(ctx, s, root, root2)
	if err != nil {
		t.Fatal(err)
	}
	manDiff := DiffManifests(m, m2)
	if !reflect.DeepEqual(treeDiff, manDiff) {
		t.Fatalf("DiffTrees and DiffManifests disagree:\n%+v\n%+v", treeDiff, manDiff)
	}
	wantPaths := []string{"internal/audit/audit.go", "internal/auth/login.go", "internal/http/routes.go", "scripts/run.sh"}
	var paths []string
	for _, c := range treeDiff {
		paths = append(paths, c.Path)
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("paths = %v", paths)
	}
	if !reflect.DeepEqual(m.Apply(manDiff), m2) {
		t.Fatal("Apply(diff) should give the new manifest")
	}
	if d, _ := DiffTrees(ctx, s, root, root); len(d) != 0 {
		t.Fatal("diff of identical trees should be empty")
	}
	// From the empty tree everything is added.
	all, err := DiffTrees(ctx, s, hash.Zero, root)
	if err != nil || len(all) != len(m) {
		t.Fatalf("diff from empty = %d changes, %v", len(all), err)
	}
}

func TestDiffFileBecomesDirectory(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMem()
	a := Manifest{"docs": {putFile(t, s, "x"), ModeFile}}
	b := Manifest{"docs/index.md": {putFile(t, s, "y"), ModeFile}}
	ra, _ := WriteManifest(ctx, s, a)
	rb, _ := WriteManifest(ctx, s, b)
	d, err := DiffTrees(ctx, s, ra, rb)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, DiffManifests(a, b)) {
		t.Fatalf("got %+v", d)
	}
}

func TestManifestConflicts(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMem()
	h := putFile(t, s, "x")
	_, err := WriteManifest(ctx, s, Manifest{"a": {h, ModeFile}, "a/b": {h, ModeFile}})
	if !errors.Is(err, ErrBadPath) {
		t.Fatalf("file/dir clash err = %v", err)
	}
	if _, err := WriteManifest(ctx, s, Manifest{"/abs": {h, ModeFile}}); !errors.Is(err, ErrBadPath) {
		t.Fatalf("absolute path err = %v", err)
	}
	empty, err := WriteManifest(ctx, s, Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := ReadManifest(ctx, s, empty); err != nil || len(m) != 0 {
		t.Fatal("empty manifest round trip failed")
	}
}

func TestChangesetHash(t *testing.T) {
	c := &Changeset{
		Version:   CurrentVersion,
		ChangeID:  NewChangeID(),
		Tree:      hash.Of(hash.KindTree, []byte("t")),
		Origin:    OriginVX,
		Author:    Author{Principal: "agent:alice/claude", OnBehalfOf: "user:alice", Model: "claude-opus-5-5"},
		Intent:    "Add rate limiting\n\nDetails here.",
		CreatedAt: time.Date(2026, 11, 24, 14, 0, 0, 123456789, time.FixedZone("CET", 3600)),
		Evidence:  &Evidence{},
	}
	h1, err := c.Hash()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := c.Encode()
	back, err := DecodeChangeset(data)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := back.Hash()
	if h1 != h2 {
		t.Fatal("decoded changeset hashes differently")
	}
	c2 := *c
	c2.Author.Model = "other"
	if h3, _ := c2.Hash(); h3 == h1 {
		t.Fatal("provenance must be covered by the hash")
	}
	if c.Title() != "Add rate limiting" {
		t.Fatalf("Title = %q", c.Title())
	}
	bad := *c
	bad.Intent = "  "
	if _, err := bad.Hash(); err == nil {
		t.Fatal("empty intent should fail validation")
	}
}

func TestChangeID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewChangeID()
		if !ValidChangeID(id) {
			t.Fatalf("invalid id %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 990 {
		t.Fatal("change ids collide too often")
	}
	for _, s := range []string{"", "kxqr7m2p", "kxqr-7m2a", "KXQR-7M2P", "kxqr-7m2pp"} {
		if ValidChangeID(s) {
			t.Errorf("ValidChangeID(%q) = true", s)
		}
	}
}

func TestApplyChangesMatchesManifest(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMem()
	base := Manifest{
		"a.txt":         {putFile(t, s, "a"), ModeFile},
		"docs":          {putFile(t, s, "docs file"), ModeFile},
		"pkg/x/one.go":  {putFile(t, s, "one"), ModeFile},
		"pkg/x/two.go":  {putFile(t, s, "two"), ModeFile},
		"pkg/y/keep.go": {putFile(t, s, "keep"), ModeFile},
	}
	root, _ := WriteManifest(ctx, s, base)
	target := base.Clone()
	delete(target, "docs") // file becomes a directory
	target["docs/index.md"] = File{putFile(t, s, "idx"), ModeFile}
	delete(target, "pkg/x/one.go") // directory becomes a file
	delete(target, "pkg/x/two.go")
	target["pkg/x"] = File{putFile(t, s, "now a file"), ModeFile}
	target["a.txt"] = File{putFile(t, s, "a2"), ModeExec}
	target["new/deep/file.go"] = File{putFile(t, s, "deep"), ModeFile}

	changes := DiffManifests(base, target)
	got, err := ApplyChanges(ctx, s, root, changes)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := WriteManifest(ctx, s, target)
	if got != want {
		gm, _ := ReadManifest(ctx, s, got)
		t.Fatalf("ApplyChanges tree differs from WriteManifest\ngot  %v\nwant %v", gm, target)
	}
	// Deleting everything gives the empty tree.
	var all []Change
	for _, p := range target.Paths() {
		all = append(all, Change{Path: p, Op: OpDelete, Old: target[p]})
	}
	empty, err := ApplyChanges(ctx, s, want, all)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := ReadManifest(ctx, s, empty); len(m) != 0 {
		t.Fatal("expected empty tree")
	}
	if _, err := ApplyChanges(ctx, s, root, []Change{{Path: "missing.go", Op: OpDelete}}); err == nil {
		t.Fatal("deleting a missing file should fail")
	}
	if _, err := ApplyChanges(ctx, s, root, []Change{{Path: "a.txt/x", Op: OpAdd, New: File{putFile(t, s, "x"), ModeFile}}}); !errors.Is(err, ErrBadPath) {
		t.Fatalf("creating inside a file should fail, got %v", err)
	}
}
