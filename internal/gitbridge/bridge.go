// Package gitbridge connects vx to git hosts. It imports upstream branches
// as changesets and exports effort changes as git commits on vx/ branches.
//
// Each repo has a bare git mirror under the data directory. Imports fetch
// only the branch in use, shallow at first, and map git objects to vx
// objects in Postgres so later imports skip everything that did not change.
package gitbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	gitobject "github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// Bridge imports from and exports to git remotes.
type Bridge struct {
	Store *store.Store
	Blobs blob.Store
	Dir   string // root for bare mirrors
	// GitHubToken returns a token for github.com, or "" for anonymous access.
	GitHubToken func() string
	// GitHubAPI is the REST API base URL (tests point it at a fake).
	GitHubAPI string
	// GitHubGitBase is the base URL for git over HTTPS (tests override it).
	GitHubGitBase string
	// FirstDepth is how many commits the first import of a branch fetches.
	FirstDepth int
	// SyncDepth bounds how many new commits one sync imports.
	SyncDepth int
	Log       *slog.Logger

	mu    sync.Mutex
	locks map[uuid.UUID]*sync.Mutex
}

// New returns a bridge with defaults.
func New(st *store.Store, blobs blob.Store, dir string, githubToken func() string) *Bridge {
	return &Bridge{
		Store: st, Blobs: blobs, Dir: dir, GitHubToken: githubToken,
		GitHubAPI: "https://api.github.com", GitHubGitBase: defaultGitHubBase,
		FirstDepth: 1, SyncDepth: 200, Log: slog.Default(),
		locks: map[uuid.UUID]*sync.Mutex{},
	}
}

// lock serializes git work per repo inside this process.
func (b *Bridge) lock(id uuid.UUID) func() {
	b.mu.Lock()
	l, ok := b.locks[id]
	if !ok {
		l = &sync.Mutex{}
		b.locks[id] = l
	}
	b.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// defaultGitHubBase is github.com over HTTPS.
const defaultGitHubBase = "https://github.com"

func (b *Bridge) githubToken() string {
	if b.GitHubToken == nil {
		return ""
	}
	return b.GitHubToken()
}

func (b *Bridge) remoteURL(repo store.Repo) (string, error) {
	switch repo.Provider {
	case store.ProviderGitHub:
		base := strings.TrimRight(b.GitHubGitBase, "/")
		if base == defaultGitHubBase && b.githubToken() == "" {
			// No token: use SSH with the server user's keys, like git does.
			return "git@github.com:" + repo.Remote + ".git", nil
		}
		return base + "/" + repo.Remote + ".git", nil
	case store.ProviderGit:
		return repo.Remote, nil
	}
	return "", fmt.Errorf("repo %s has no git upstream", repo.Name)
}

// auth returns HTTPS credentials for GitHub when a token is set. Without
// one, SSH remotes use the SSH agent and known_hosts, as git does.
func (b *Bridge) auth(repo store.Repo) transport.AuthMethod {
	if repo.Provider == store.ProviderGitHub {
		if tok := b.githubToken(); tok != "" {
			return &githttp.BasicAuth{Username: "x-access-token", Password: tok}
		}
	}
	return nil
}

// mirror opens the repo's bare mirror, creating it on first use.
func (b *Bridge) mirror(repo store.Repo) (*git.Repository, error) {
	url, err := b.remoteURL(repo)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(b.Dir, repo.ID.String()+".git")
	r, err := git.PlainOpen(path)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		if err := os.MkdirAll(b.Dir, 0o755); err != nil {
			return nil, err
		}
		r, err = git.PlainInit(path, true)
	}
	if err != nil {
		return nil, fmt.Errorf("git mirror: %w", err)
	}
	cfg, err := r.Config()
	if err != nil {
		return nil, err
	}
	rc, ok := cfg.Remotes["origin"]
	if !ok || len(rc.URLs) != 1 || rc.URLs[0] != url {
		cfg.Remotes["origin"] = &gitconfig.RemoteConfig{Name: "origin", URLs: []string{url}}
		if err := r.SetConfig(cfg); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// DefaultBranch asks the remote which branch HEAD points to.
func (b *Bridge) DefaultBranch(ctx context.Context, repo store.Repo) (string, error) {
	url, err := b.remoteURL(repo)
	if err != nil {
		return "", err
	}
	rem := git.NewRemote(nil, &gitconfig.RemoteConfig{Name: "origin", URLs: []string{url}})
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: b.auth(repo)})
	if err != nil {
		return "", err
	}
	branches := map[string]bool{}
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference && ref.Target().IsBranch() {
			return ref.Target().Short(), nil
		}
		if ref.Name().IsBranch() {
			branches[ref.Name().Short()] = true
		}
	}
	for _, name := range []string{"main", "master", "trunk", "develop"} {
		if branches[name] {
			return name, nil
		}
	}
	names := make([]string, 0, len(branches))
	for n := range branches {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", fmt.Errorf("%s has no branches", url)
	}
	return names[0], nil
}

// ErrBranchNotFound is returned when the remote has no such branch.
var ErrBranchNotFound = errors.New("branch not found on the remote")

func (b *Bridge) fetch(ctx context.Context, r *git.Repository, repo store.Repo, branch string, depth int) (plumbing.Hash, error) {
	local := plumbing.NewRemoteReferenceName("origin", branch)
	spec := gitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:%s", branch, local))
	err := r.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{spec},
		Depth:      depth,
		Auth:       b.auth(repo),
		Tags:       git.NoTags,
		Force:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			return plumbing.ZeroHash, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
		}
		return plumbing.ZeroHash, fmt.Errorf("fetch %s: %w", branch, err)
	}
	ref, err := r.Reference(local, true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	return ref.Hash(), nil
}

// SyncBranch fetches branch and imports its new first-parent commits.
func (b *Bridge) SyncBranch(ctx context.Context, repo store.Repo, branch string) (store.Branch, int, error) {
	defer b.lock(repo.ID)()
	prev, err := b.Store.GetBranch(ctx, repo.ID, branch)
	known := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Branch{}, 0, err
	}
	r, err := b.mirror(repo)
	if err != nil {
		return store.Branch{}, 0, err
	}
	depth := b.FirstDepth
	if known {
		depth = b.SyncDepth
	}
	tip, err := b.fetch(ctx, r, repo, branch, depth)
	if err != nil {
		if errors.Is(err, ErrBranchNotFound) {
			return store.Branch{}, 0, fmt.Errorf("%w: %v", store.ErrNotFound, err)
		}
		return store.Branch{}, 0, err
	}
	if known && prev.GitSHA == tip.String() {
		return prev, 0, nil
	}

	// Walk first parents back to something already imported.
	var todo []*gitobject.Commit
	parent := hash.Zero
	if known {
		parent = prev.Head
	}
	h := tip
	for len(todo) < max(b.SyncDepth, 1) {
		if row, err := b.Store.ChangesetByGitSHA(ctx, repo.ID, h.String()); err == nil {
			parent = row.Hash
			break
		}
		c, err := r.CommitObject(h)
		if err != nil {
			break // shallow boundary
		}
		todo = append(todo, c)
		if !known || len(c.ParentHashes) == 0 {
			break // first import: only the tip
		}
		h = c.ParentHashes[0]
	}
	if len(todo) == 0 {
		// The tip was imported before, for example on another branch.
		row, err := b.Store.ChangesetByGitSHA(ctx, repo.ID, tip.String())
		if err != nil {
			return store.Branch{}, 0, err
		}
		nb := store.Branch{RepoID: repo.ID, Name: branch, Head: row.Hash, GitSHA: tip.String()}
		return nb, 0, b.Store.UpsertBranch(ctx, nb)
	}

	conv := newConverter(b, r, repo.ID)
	parentTree := hash.Zero
	if !parent.IsZero() {
		row, err := b.Store.GetChangeset(ctx, repo.ID, parent)
		if err != nil {
			return store.Branch{}, 0, err
		}
		parentTree = row.Tree
	}
	for i := len(todo) - 1; i >= 0; i-- {
		c := todo[i]
		tree, err := conv.tree(ctx, c.TreeHash)
		if err != nil {
			return store.Branch{}, 0, fmt.Errorf("import %s: %w", c.Hash.String()[:12], err)
		}
		if err := conv.flush(ctx); err != nil {
			return store.Branch{}, 0, err
		}
		files, err := b.fileChanges(ctx, parentTree, tree)
		if err != nil {
			return store.Branch{}, 0, err
		}
		cs := &object.Changeset{
			Version:  object.CurrentVersion,
			ChangeID: object.ChangeIDFrom(c.Hash[:]),
			Tree:     tree,
			Origin:   object.OriginUpstream,
			GitSHA:   c.Hash.String(),
			Author: object.Author{
				Principal: "git:" + c.Author.Email,
				Name:      c.Author.Name,
				Email:     c.Author.Email,
			},
			Intent:    commitIntent(c.Message),
			CreatedAt: c.Committer.When,
		}
		if !parent.IsZero() {
			cs.Parents = []hash.Hash{parent}
		}
		h, err := b.Store.InsertChangeset(ctx, repo.ID, nil, cs, files)
		if err != nil {
			return store.Branch{}, 0, err
		}
		parent, parentTree = h, tree
	}
	nb := store.Branch{RepoID: repo.ID, Name: branch, Head: parent, GitSHA: tip.String()}
	if err := b.Store.UpsertBranch(ctx, nb); err != nil {
		return store.Branch{}, 0, err
	}
	b.Log.Info("imported branch", "repo", repo.Name, "branch", branch, "commits", len(todo), "head", parent.Short())
	return nb, len(todo), nil
}

func commitIntent(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "(no commit message)"
	}
	return msg
}

// fileChanges lists changes with line counts. Very large imports skip the
// counts, which would mean reading every file.
func (b *Bridge) fileChanges(ctx context.Context, from, to hash.Hash) ([]store.FileChange, error) {
	changes, err := object.DiffTrees(ctx, b.Blobs, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]store.FileChange, 0, len(changes))
	for _, c := range changes {
		fc := store.FileChange{Change: c}
		if len(changes) <= 500 {
			var old, cur []byte
			if !c.Old.Hash.IsZero() {
				if old, err = blob.GetKind(ctx, b.Blobs, hash.KindBlob, c.Old.Hash); err != nil {
					return nil, err
				}
			}
			if !c.New.Hash.IsZero() {
				if cur, err = blob.GetKind(ctx, b.Blobs, hash.KindBlob, c.New.Hash); err != nil {
					return nil, err
				}
			}
			fc.Added, fc.Deleted = merge.LineStats(old, cur)
		}
		out = append(out, fc)
	}
	return out, nil
}

// converter turns git trees into vx trees, remembering every mapping.
type converter struct {
	b       *Bridge
	r       *git.Repository
	repoID  uuid.UUID
	memo    map[plumbing.Hash]hash.Hash
	pending []store.GitObject
}

func newConverter(b *Bridge, r *git.Repository, repoID uuid.UUID) *converter {
	return &converter{b: b, r: r, repoID: repoID, memo: map[plumbing.Hash]hash.Hash{}}
}

func (c *converter) remember(g plumbing.Hash, v hash.Hash, kind string) {
	c.memo[g] = v
	c.pending = append(c.pending, store.GitObject{GitHash: g.String(), VXHash: v, Kind: kind})
}

// flush saves the git mappings and records the converted objects as part
// of the repo, so they are served through it. Objects reused from earlier
// imports of the same repo were recorded then.
func (c *converter) flush(ctx context.Context) error {
	for len(c.pending) > 0 {
		n := min(len(c.pending), 5000)
		hashes := make([]hash.Hash, 0, n)
		for _, g := range c.pending[:n] {
			hashes = append(hashes, g.VXHash)
		}
		if err := c.b.Store.AddRepoObjects(ctx, c.repoID, hashes); err != nil {
			return err
		}
		if err := c.b.Store.SaveGitObjects(ctx, c.repoID, c.pending[:n]); err != nil {
			return err
		}
		c.pending = c.pending[n:]
	}
	return nil
}

func (c *converter) tree(ctx context.Context, g plumbing.Hash) (hash.Hash, error) {
	if v, ok := c.memo[g]; ok {
		return v, nil
	}
	known, err := c.b.Store.LookupGitObjects(ctx, c.repoID, []string{g.String()})
	if err != nil {
		return hash.Zero, err
	}
	if v, ok := known[g.String()]; ok {
		if ok, _ := c.b.Blobs.Has(ctx, v); ok {
			c.memo[g] = v
			return v, nil
		}
	}
	gt, err := c.r.TreeObject(g)
	if err != nil {
		return hash.Zero, err
	}
	ids := make([]string, 0, len(gt.Entries))
	for _, e := range gt.Entries {
		ids = append(ids, e.Hash.String())
	}
	known, err = c.b.Store.LookupGitObjects(ctx, c.repoID, ids)
	if err != nil {
		return hash.Zero, err
	}
	t := &object.Tree{Entries: make([]object.Entry, 0, len(gt.Entries))}
	for _, e := range gt.Entries {
		var mode object.Mode
		switch e.Mode {
		case filemode.Dir:
			mode = object.ModeDir
		case filemode.Regular, filemode.Deprecated:
			mode = object.ModeFile
		case filemode.Executable:
			mode = object.ModeExec
		case filemode.Symlink:
			mode = object.ModeSymlink
		case filemode.Submodule:
			c.b.Log.Warn("skipping git submodule; vx does not track submodules yet", "path", e.Name)
			continue
		default:
			continue
		}
		var v hash.Hash
		if mode == object.ModeDir {
			if v, err = c.tree(ctx, e.Hash); err != nil {
				return hash.Zero, err
			}
		} else if m, ok := c.memo[e.Hash]; ok {
			v = m
		} else if k, ok := known[e.Hash.String()]; ok && c.hasBlob(ctx, k) {
			v = k
			c.memo[e.Hash] = k
		} else {
			if v, err = c.blob(ctx, e.Hash); err != nil {
				return hash.Zero, err
			}
		}
		t.Entries = append(t.Entries, object.Entry{Name: e.Name, Mode: mode, Hash: v})
	}
	sort.Slice(t.Entries, func(i, j int) bool { return t.Entries[i].Name < t.Entries[j].Name })
	v, err := object.PutTree(ctx, c.b.Blobs, t)
	if err != nil {
		return hash.Zero, err
	}
	c.remember(g, v, "tree")
	if len(c.pending) >= 5000 {
		if err := c.flush(ctx); err != nil {
			return hash.Zero, err
		}
	}
	return v, nil
}

func (c *converter) hasBlob(ctx context.Context, h hash.Hash) bool {
	ok, err := c.b.Blobs.Has(ctx, h)
	return err == nil && ok
}

func (c *converter) blob(ctx context.Context, g plumbing.Hash) (hash.Hash, error) {
	gb, err := c.r.BlobObject(g)
	if err != nil {
		return hash.Zero, err
	}
	rd, err := gb.Reader()
	if err != nil {
		return hash.Zero, err
	}
	data, err := io.ReadAll(rd)
	rd.Close()
	if err != nil {
		return hash.Zero, err
	}
	v, err := c.b.Blobs.Put(ctx, hash.KindBlob, data)
	if err != nil {
		return hash.Zero, err
	}
	c.remember(g, v, "blob")
	return v, nil
}
