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

func (f *fixture) merge(base, ours, theirs map[string]string) MergeResult {
	f.t.Helper()
	r, err := MergeTrees(f.ctx, f.s, f.tree(base), f.tree(ours), f.tree(theirs), merge.Labels{Base: "base", Ours: "ours", Theirs: "theirs"})
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
