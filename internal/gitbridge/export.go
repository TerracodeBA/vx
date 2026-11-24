package gitbridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	gitobject "github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/provider"
	"github.com/TerracodeBA/vx/internal/store"
)

// BranchPrefix is the only namespace vx ever pushes to.
const BranchPrefix = "vx/"

// ExportBranch writes commits on top of the git commit baseSHA and pushes
// them to branch, which must start with "vx/". It returns the git SHA of
// every commit, oldest first.
func (b *Bridge) ExportBranch(ctx context.Context, repo store.Repo, baseSHA, branch string, commits []provider.Commit) ([]string, error) {
	if !strings.HasPrefix(branch, BranchPrefix) || len(branch) <= len(BranchPrefix) {
		return nil, fmt.Errorf("refusing to push %q: vx only pushes branches under %s", branch, BranchPrefix)
	}
	if len(commits) == 0 {
		return nil, errors.New("nothing to export")
	}
	defer b.lock(repo.ID)()
	r, err := b.mirror(repo)
	if err != nil {
		return nil, err
	}
	parent := plumbing.NewHash(baseSHA)
	if _, err := r.CommitObject(parent); err != nil {
		return nil, fmt.Errorf("base commit %s is not in the mirror; sync the branch first: %w", baseSHA, err)
	}
	w := &treeWriter{b: b, r: r, repoID: repo.ID, memo: map[hash.Hash]plumbing.Hash{}}
	var shas []string
	for _, c := range commits {
		th, err := w.tree(ctx, c.Tree)
		if err != nil {
			return nil, err
		}
		if err := w.flush(ctx); err != nil {
			return nil, err
		}
		name, email := c.AuthorName, c.AuthorEmail
		if name == "" {
			name = "vx"
		}
		if email == "" {
			email = "vx@localhost"
		}
		sig := gitobject.Signature{Name: name, Email: email, When: c.When}
		commit := &gitobject.Commit{
			Author: sig, Committer: sig, Message: c.Message,
			TreeHash: th, ParentHashes: []plumbing.Hash{parent},
		}
		obj := r.Storer.NewEncodedObject()
		if err := commit.Encode(obj); err != nil {
			return nil, err
		}
		h, err := r.Storer.SetEncodedObject(obj)
		if err != nil {
			return nil, err
		}
		shas = append(shas, h.String())
		parent = h
	}
	ref := plumbing.NewBranchReferenceName(branch)
	if err := r.Storer.SetReference(plumbing.NewHashReference(ref, parent)); err != nil {
		return nil, err
	}
	spec := gitconfig.RefSpec(fmt.Sprintf("+%s:%s", ref, ref))
	err = r.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin", RefSpecs: []gitconfig.RefSpec{spec}, Auth: b.auth(repo), Force: true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("push %s: %w", branch, err)
	}
	b.Log.Info("pushed branch", "repo", repo.Name, "branch", branch, "commits", len(shas), "head", parent.String()[:12])
	return shas, nil
}

// treeWriter writes vx trees into the git mirror, reusing git objects
// already known for the same content.
type treeWriter struct {
	b       *Bridge
	r       *git.Repository
	repoID  uuid.UUID
	memo    map[hash.Hash]plumbing.Hash
	pending []store.GitObject
}

func (w *treeWriter) flush(ctx context.Context) error {
	err := w.b.Store.SaveGitObjects(ctx, w.repoID, w.pending)
	w.pending = nil
	return err
}

func (w *treeWriter) known(ctx context.Context, hs []hash.Hash) (map[hash.Hash]plumbing.Hash, error) {
	out := map[hash.Hash]plumbing.Hash{}
	var ask []hash.Hash
	for _, h := range hs {
		if g, ok := w.memo[h]; ok {
			out[h] = g
		} else {
			ask = append(ask, h)
		}
	}
	m, err := w.b.Store.GitHashesFor(ctx, w.repoID, ask)
	if err != nil {
		return nil, err
	}
	for h, g := range m {
		gh := plumbing.NewHash(g)
		if w.r.Storer.HasEncodedObject(gh) == nil {
			out[h] = gh
			w.memo[h] = gh
		}
	}
	return out, nil
}

func (w *treeWriter) tree(ctx context.Context, h hash.Hash) (plumbing.Hash, error) {
	if k, err := w.known(ctx, []hash.Hash{h}); err != nil {
		return plumbing.ZeroHash, err
	} else if g, ok := k[h]; ok {
		return g, nil
	}
	t, err := object.GetTree(ctx, w.b.Blobs, h)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	hs := make([]hash.Hash, 0, len(t.Entries))
	for _, e := range t.Entries {
		hs = append(hs, e.Hash)
	}
	known, err := w.known(ctx, hs)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	gt := &gitobject.Tree{}
	for _, e := range t.Entries {
		var mode filemode.FileMode
		var g plumbing.Hash
		switch e.Mode {
		case object.ModeDir:
			mode = filemode.Dir
			if g, err = w.tree(ctx, e.Hash); err != nil {
				return plumbing.ZeroHash, err
			}
		default:
			switch e.Mode {
			case object.ModeExec:
				mode = filemode.Executable
			case object.ModeSymlink:
				mode = filemode.Symlink
			default:
				mode = filemode.Regular
			}
			if k, ok := known[e.Hash]; ok {
				g = k
			} else if g, err = w.blob(ctx, e.Hash); err != nil {
				return plumbing.ZeroHash, err
			}
		}
		gt.Entries = append(gt.Entries, gitobject.TreeEntry{Name: e.Name, Mode: mode, Hash: g})
	}
	// Git orders entries as if directory names ended in "/".
	sort.Slice(gt.Entries, func(i, j int) bool { return gitSortKey(gt.Entries[i]) < gitSortKey(gt.Entries[j]) })
	obj := w.r.Storer.NewEncodedObject()
	if err := gt.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	g, err := w.r.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	w.memo[h] = g
	w.pending = append(w.pending, store.GitObject{GitHash: g.String(), VXHash: h, Kind: "tree"})
	return g, nil
}

func gitSortKey(e gitobject.TreeEntry) string {
	if e.Mode == filemode.Dir {
		return e.Name + "/"
	}
	return e.Name
}

func (w *treeWriter) blob(ctx context.Context, h hash.Hash) (plumbing.Hash, error) {
	data, err := blob.GetKind(ctx, w.b.Blobs, hash.KindBlob, h)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	obj := w.r.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(data)))
	wr, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := wr.Write(data); err != nil {
		wr.Close()
		return plumbing.ZeroHash, err
	}
	if err := wr.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	g, err := w.r.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	w.memo[h] = g
	w.pending = append(w.pending, store.GitObject{GitHash: g.String(), VXHash: h, Kind: "blob"})
	return g, nil
}
