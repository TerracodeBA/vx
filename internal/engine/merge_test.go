package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
)

type fixture struct {
	t   *testing.T
	ctx context.Context
	s   *blob.Mem
}

func newFixture(t *testing.T) *fixture {
	return &fixture{t: t, ctx: context.Background(), s: blob.NewMem()}
}

// tree builds a tree from path -> content. A content of "\x00exec:" prefix
// marks an executable.
func (f *fixture) tree(files map[string]string) hash.Hash {
	f.t.Helper()
	m := object.Manifest{}
	for p, c := range files {
		mode := object.ModeFile
		if strings.HasPrefix(c, "exec:") {
			mode = object.ModeExec
			c = strings.TrimPrefix(c, "exec:")
		}
		h, err := f.s.Put(f.ctx, hash.KindBlob, []byte(c))
		if err != nil {
			f.t.Fatal(err)
		}
		m[p] = object.File{Hash: h, Mode: mode}
	}
	root, err := object.WriteManifest(f.ctx, f.s, m)
	if err != nil {
		f.t.Fatal(err)
	}
	return root
}

func (f *fixture) read(root hash.Hash) map[string]string {
	f.t.Helper()
	m, err := object.ReadManifest(f.ctx, f.s, root)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for p, file := range m {
		_, data, err := f.s.Get(f.ctx, file.Hash)
		if err != nil {
			f.t.Fatal(err)
		}
		c := string(data)
		if file.Mode == object.ModeExec {
			c = "exec:" + c
		}
		out[p] = c
	}
	return out
}

func (f *fixture) merge(base, ours, theirs map[string]string, opts ...MergeOption) MergeResult {
	f.t.Helper()
	r, err := MergeTrees(f.ctx, f.s, f.tree(base), f.tree(ours), f.tree(theirs), merge.Labels{Base: "base", Ours: "ours", Theirs: "theirs"}, opts...)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func eq(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q want %q\nfull: %v", k, got[k], v, got)
		}
	}
}

func TestMergeDisjointFiles(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"a.go": "a\n", "b.go": "b\n", "c.go": "c\n"}
	ours := map[string]string{"a.go": "a2\n", "b.go": "b\n", "new.go": "n\n", "c.go": "c\n"}
	theirs := map[string]string{"a.go": "a\n", "b.go": "b2\n"} // deleted c.go
	r := f.merge(base, ours, theirs)
	if !r.Clean() {
		t.Fatalf("conflicts: %+v", r.Conflicts)
	}
	eq(t, f.read(r.Tree), map[string]string{"a.go": "a2\n", "b.go": "b2\n", "new.go": "n\n"})
}

func TestMergeSameFileDifferentLines(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"auth.go": "func Login() {\n}\n\nfunc Logout() {\n}\n"}
	ours := map[string]string{"auth.go": "func Login(ctx) {\n}\n\nfunc Logout() {\n}\n"}
	theirs := map[string]string{"auth.go": "func Login() {\n}\n\nfunc Logout() {\n\taudit()\n}\n"}
	r := f.merge(base, ours, theirs)
	if !r.Clean() || len(r.AutoMerged) != 1 {
		t.Fatalf("expected clean auto-merge, got %+v", r)
	}
	eq(t, f.read(r.Tree), map[string]string{"auth.go": "func Login(ctx) {\n}\n\nfunc Logout() {\n\taudit()\n}\n"})
}

func TestMergeConflicts(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"x.go": "1\n2\n3\n", "gone.go": "g\n", "keep.go": "k\n"}
	ours := map[string]string{"x.go": "1\nours\n3\n", "gone.go": "g edited\n", "keep.go": "k\n", "both.go": "ours\n"}
	theirs := map[string]string{"x.go": "1\ntheirs\n3\n", "keep.go": "k\n", "both.go": "theirs\n"}
	r := f.merge(base, ours, theirs)
	if r.Clean() || !r.Tree.IsZero() {
		t.Fatal("expected conflicts and no tree")
	}
	kinds := map[string]ConflictKind{}
	for _, c := range r.Conflicts {
		kinds[c.Path] = c.Kind
	}
	want := map[string]ConflictKind{"x.go": ConflictContent, "gone.go": ConflictModifyDelete, "both.go": ConflictAddAdd}
	for p, k := range want {
		if kinds[p] != k {
			t.Errorf("%s: kind %q, want %q", p, kinds[p], k)
		}
	}
	for _, c := range r.Conflicts {
		if c.Path == "x.go" && !strings.Contains(string(c.Markers), "<<<<<<< ours") {
			t.Errorf("missing markers: %q", c.Markers)
		}
	}
}

func TestMergeIdenticalAndDeleteBoth(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"a": "1\n", "b": "2\n"}
	ours := map[string]string{"a": "same\n"}
	theirs := map[string]string{"a": "same\n"}
	r := f.merge(base, ours, theirs)
	if !r.Clean() {
		t.Fatalf("conflicts: %+v", r.Conflicts)
	}
	eq(t, f.read(r.Tree), map[string]string{"a": "same\n"})
}

func TestMergeModes(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"run.sh": "echo\n"}
	ours := map[string]string{"run.sh": "exec:echo\n"}
	theirs := map[string]string{"run.sh": "echo hi\n"}
	r := f.merge(base, ours, theirs)
	if !r.Clean() {
		t.Fatalf("conflicts: %+v", r.Conflicts)
	}
	eq(t, f.read(r.Tree), map[string]string{"run.sh": "exec:echo hi\n"})
}

func TestMergePathClash(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"keep": "k\n"}
	ours := map[string]string{"keep": "k\n", "docs": "a file\n"}
	theirs := map[string]string{"keep": "k\n", "docs/index.md": "a dir\n"}
	r := f.merge(base, ours, theirs)
	if r.Clean() || r.Conflicts[0].Kind != ConflictPath {
		t.Fatalf("expected path conflict, got %+v", r)
	}
}

func TestMergeShortcuts(t *testing.T) {
	f := newFixture(t)
	a := f.tree(map[string]string{"a": "1\n"})
	b := f.tree(map[string]string{"a": "2\n"})
	for _, tc := range []struct{ base, ours, theirs, want hash.Hash }{
		{a, a, b, b},
		{a, b, a, b},
		{a, b, b, b},
	} {
		r, err := MergeTrees(f.ctx, f.s, tc.base, tc.ours, tc.theirs, merge.Labels{})
		if err != nil || r.Tree != tc.want {
			t.Fatalf("shortcut merge wrong: %v %v", r, err)
		}
	}
}

func TestMergeKeepConflicts(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"x.go": "1\n2\n3\n", "y.go": "y\n", "z.go": "z\n"}
	ours := map[string]string{"x.go": "1\nours\n3\n", "y.go": "y ours\n"}
	theirs := map[string]string{"x.go": "1\ntheirs\n3\n", "z.go": "z theirs\n"}
	r, err := MergeTreesKeepConflicts(f.ctx, f.s, f.tree(base), f.tree(ours), f.tree(theirs), merge.Labels{Ours: "local", Theirs: "effort"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 3 || r.Tree.IsZero() {
		t.Fatalf("got %d conflicts, tree %v", len(r.Conflicts), r.Tree)
	}
	got := f.read(r.Tree)
	if !strings.Contains(got["x.go"], "<<<<<<< local") || got["y.go"] != "y ours\n" || got["z.go"] != "z theirs\n" {
		t.Fatalf("kept tree = %v", got)
	}
}

func TestMergeKeptBoth(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{
		"cli.go":     "func cmds() {\n\troot.AddCommand(\n\t\tnewA(),\n\t)\n}\n",
		"guide.md":   "# Guide\n\n## Old\n",
		"plain.go":   "a\nb\nc\n",
		"binary.bin": "\x00base",
	}
	ours := map[string]string{
		"cli.go":     "func cmds() {\n\troot.AddCommand(\n\t\tnewA(),\n\t\tnewOurs(),\n\t)\n}\n",
		"guide.md":   "# Guide\n\n## Ours\n\n## Old\n",
		"plain.go":   "a\nB\nc\n",
		"binary.bin": "\x00base",
	}
	theirs := map[string]string{
		"cli.go":     "func cmds() {\n\troot.AddCommand(\n\t\tnewA(),\n\t\tnewTheirs(),\n\t)\n}\n",
		"guide.md":   "# Guide\n\n## Theirs\n\n## Old\n",
		"plain.go":   "a\nb\nc\nd\n",
		"binary.bin": "\x00base",
	}
	r := f.merge(base, ours, theirs)
	if !r.Clean() {
		t.Fatalf("conflicts: %+v", r.Conflicts)
	}
	if strings.Join(r.AutoMerged, ",") != "cli.go,guide.md,plain.go" || strings.Join(r.KeptBoth, ",") != "cli.go,guide.md" {
		t.Fatalf("auto-merged %v, kept both %v", r.AutoMerged, r.KeptBoth)
	}
	got := f.read(r.Tree)
	if got["cli.go"] != "func cmds() {\n\troot.AddCommand(\n\t\tnewA(),\n\t\tnewTheirs(),\n\t\tnewOurs(),\n\t)\n}\n" {
		t.Fatalf("cli.go = %q", got["cli.go"])
	}
	if got["guide.md"] != "# Guide\n\n## Theirs\n\n## Ours\n\n## Old\n" {
		t.Fatalf("guide.md = %q", got["guide.md"])
	}
}

func TestMergeKeptBothNotForBinaryOrAddAdd(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"blob.bin": "\x00a\n"}
	ours := map[string]string{"blob.bin": "\x00a\nours\n", "new.go": "package ours\n"}
	theirs := map[string]string{"blob.bin": "\x00a\ntheirs\n", "new.go": "package theirs\n"}
	r := f.merge(base, ours, theirs)
	kinds := map[string]ConflictKind{}
	for _, c := range r.Conflicts {
		kinds[c.Path] = c.Kind
	}
	if kinds["blob.bin"] != ConflictBinary || kinds["new.go"] != ConflictAddAdd || len(r.KeptBoth) != 0 {
		t.Fatalf("conflicts %v, kept both %v", kinds, r.KeptBoth)
	}
}

func TestMergeKeepConflictsReportsKeptBoth(t *testing.T) {
	f := newFixture(t)
	base := map[string]string{"x.go": "1\n2\n", "y.md": "# Y\n"}
	ours := map[string]string{"x.go": "1\nours\n", "y.md": "# Y\nlocal\n"}
	theirs := map[string]string{"x.go": "1\ntheirs\n", "y.md": "# Y\neffort\n"}
	r, err := MergeTreesKeepConflicts(f.ctx, f.s, f.tree(base), f.tree(ours), f.tree(theirs), merge.Labels{Ours: "local", Theirs: "effort"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].Path != "x.go" || strings.Join(r.KeptBoth, ",") != "y.md" {
		t.Fatalf("conflicts %+v, kept both %v", r.Conflicts, r.KeptBoth)
	}
	if got := f.read(r.Tree)["y.md"]; got != "# Y\neffort\nlocal\n" {
		t.Fatalf("y.md = %q", got)
	}
}

func TestIsGenerated(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"// Code generated by protoc-gen-go. DO NOT EDIT.\n// versions:\npackage v1\n", true},
		{"// Copyright 2026\n\n// Code generated by stringer -type=Kind; DO NOT EDIT.\r\n\npackage x\n", true},
		{"package x\n\n// Code generated by hand. Do edit.\n", false},
		{"// Code generated by protoc-gen-go. DO NOT EDIT\npackage v1\n", false}, // no final period
		{"// not generated\n", false},
		{"", false},
		{"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n// Code generated by x. DO NOT EDIT.\n", false}, // past the first lines
	} {
		if got := IsGenerated([]byte(tc.src)); got != tc.want {
			t.Errorf("IsGenerated(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}
