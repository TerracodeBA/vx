package service

// Object access. Objects (blobs and trees) are content-addressed and stored
// once, but each repository records which objects are part of it
// (repo_objects). Objects are served only through a repository the caller
// can read and only when they are part of it; an object stored for another
// repository is reported as not found, never as "exists elsewhere".
//
// An object becomes part of a repository when a client uploads its bytes
// with that repository (PutObjects: the server hashes what it receives, so
// the uploader proves it has the content), when the git bridge imports it,
// or when the server makes it from the repository's own objects (merges).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// Object is one object in transfer.
type Object struct {
	Kind hash.Kind
	Data []byte
	Hash hash.Hash // for uploads: what the client says it is; zero skips the check
}

// MsgClientTooOld is the error for object requests that do not name a
// repository, which only clients from before v0.2 send.
const MsgClientTooOld = "this vx is too old for the server: update vx (object requests must name a repository)"

// emptyTree is the hash of the tree with no entries. Every repository
// starts from it and it holds nothing, so it is part of every repository.
var emptyTree = func() hash.Hash {
	data, err := (&object.Tree{}).Encode()
	if err != nil {
		panic(err)
	}
	return hash.Of(hash.KindTree, data)
}()

// objectRepo authorizes an object request on repo and makes sure the
// repository's objects are indexed.
func (s *Service) objectRepo(ctx context.Context, actor Actor, repo string, need RepoRole) (store.Repo, error) {
	if strings.TrimSpace(repo) == "" {
		return store.Repo{}, errf(CodeFailedPrecondition, MsgClientTooOld)
	}
	r, _, err := s.Authorize(ctx, actor, repo, need)
	if err != nil {
		return store.Repo{}, err
	}
	if err := s.ensureObjectsIndexed(ctx, r.ID); err != nil {
		return store.Repo{}, err
	}
	return r, nil
}

// notInRepo returns the hashes that are not part of the repo.
func (s *Service) notInRepo(ctx context.Context, repoID uuid.UUID, hashes []hash.Hash) ([]hash.Hash, error) {
	ask := make([]hash.Hash, 0, len(hashes))
	for _, h := range hashes {
		if h != emptyTree {
			ask = append(ask, h)
		}
	}
	return s.Store.RepoObjectsMissing(ctx, repoID, ask)
}

// MissingObjects returns the hashes that are not part of repo yet, which
// the client then uploads. An object stored only for another repository is
// missing here: uploading it again costs little, because storage dedupes,
// and proves the uploader has the bytes.
func (s *Service) MissingObjects(ctx context.Context, actor Actor, repo string, hashes []hash.Hash) ([]hash.Hash, error) {
	r, err := s.objectRepo(ctx, actor, repo, RoleRead)
	if err != nil {
		return nil, err
	}
	return s.notInRepo(ctx, r.ID, hashes)
}

// PutObjects stores objects and records them as part of repo. The caller
// needs write access. The server hashes the bytes it receives, so an object
// only becomes part of a repository when its content was sent.
func (s *Service) PutObjects(ctx context.Context, actor Actor, repo string, objs []Object) ([]hash.Hash, error) {
	r, err := s.objectRepo(ctx, actor, repo, RoleWrite)
	if err != nil {
		return nil, err
	}
	if err := s.requireAgreement(ctx, r, "uploading files"); err != nil {
		return nil, err
	}
	var size int64
	for _, o := range objs {
		size += int64(len(o.Data))
	}
	if err := s.checkPushQuota(ctx, r, size); err != nil {
		return nil, err
	}
	out := make([]hash.Hash, 0, len(objs))
	for _, o := range objs {
		if o.Kind != hash.KindBlob && o.Kind != hash.KindTree {
			return nil, errf(CodeInvalid, "object kind must be blob or tree, got %q", o.Kind)
		}
		if o.Kind == hash.KindTree {
			if _, err := object.DecodeTree(o.Data); err != nil {
				return nil, errf(CodeInvalid, "invalid tree object: %v", err)
			}
		}
		if !o.Hash.IsZero() {
			if got := hash.Of(o.Kind, o.Data); got != o.Hash {
				return nil, errf(CodeInvalid, "object hash mismatch: client says %s, content is %s", o.Hash, got)
			}
		}
		h, err := s.Blobs.Put(ctx, o.Kind, o.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	missing, err := s.Store.RecordRepoObjects(ctx, r.ID, out, s.blobPresent)
	for try := 0; err == nil && len(missing) > 0 && try < 3; try++ {
		// An organization purge deleted some of these objects between our
		// put (which found them and skipped writing) and recording them:
		// write them again (Put asks the store, not a cache) and record.
		gone := make(map[hash.Hash]bool, len(missing))
		for _, h := range missing {
			gone[h] = true
		}
		for _, o := range objs {
			if h := hash.Of(o.Kind, o.Data); gone[h] {
				if _, err = s.Blobs.Put(ctx, o.Kind, o.Data); err != nil {
					return nil, err
				}
				delete(gone, h)
			}
		}
		missing, err = s.Store.RecordRepoObjects(ctx, r.ID, missing, s.blobPresent)
	}
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, errObjectsPurged(len(missing))
	}
	s.sizes.queue(out)
	return out, nil
}

// GetObjects returns objects that are part of repo, in the order asked,
// stopping before maxBytes (but always returning at least one); the client
// asks again for the rest. An object that is not part of repo is not found,
// whether or not another repository has it.
func (s *Service) GetObjects(ctx context.Context, actor Actor, repo string, hashes []hash.Hash, maxBytes int) ([]Object, error) {
	r, err := s.objectRepo(ctx, actor, repo, RoleRead)
	if err != nil {
		return nil, err
	}
	missing, err := s.notInRepo(ctx, r.ID, hashes)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, objectNotFound(r.Name, missing)
	}
	out := make([]Object, 0, len(hashes))
	size := 0
	for _, h := range hashes {
		kind, data, err := s.Blobs.Get(ctx, h)
		if err != nil {
			if errors.Is(err, blob.ErrNotFound) {
				return nil, objectNotFound(r.Name, []hash.Hash{h})
			}
			return nil, err
		}
		size += len(data)
		if maxBytes > 0 && size > maxBytes && len(out) > 0 {
			break
		}
		out = append(out, Object{Kind: kind, Data: data, Hash: h})
	}
	return out, nil
}

func objectNotFound(repo string, missing []hash.Hash) error {
	more := ""
	if len(missing) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(missing)-1)
	}
	return &Error{Code: CodeNotFound, Msg: fmt.Sprintf("object %s not found in %s%s", missing[0], repo, more), Err: blob.ErrNotFound}
}

// BlobReader returns a function that reads file contents of the repository
// with id repoID, after checking once that actor can read it. The web UI
// and the MCP tools read blobs through it. Zero hashes read as empty.
func (s *Service) BlobReader(ctx context.Context, actor Actor, repoID uuid.UUID) (func(context.Context, hash.Hash) ([]byte, error), error) {
	if _, err := s.AuthorizeID(ctx, actor, repoID, RoleRead); err != nil {
		return nil, err
	}
	if err := s.ensureObjectsIndexed(ctx, repoID); err != nil {
		return nil, err
	}
	return func(ctx context.Context, h hash.Hash) ([]byte, error) {
		if h.IsZero() {
			return nil, nil
		}
		missing, err := s.notInRepo(ctx, repoID, []hash.Hash{h})
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			return nil, &Error{Code: CodeNotFound, Msg: fmt.Sprintf("object %s not found in this repository", h), Err: blob.ErrNotFound}
		}
		return blob.GetKind(ctx, s.Blobs, hash.KindBlob, h)
	}, nil
}

// ReadBlob reads one file's contents from a repository actor can read.
func (s *Service) ReadBlob(ctx context.Context, actor Actor, repoID uuid.UUID, h hash.Hash) ([]byte, error) {
	read, err := s.BlobReader(ctx, actor, repoID)
	if err != nil {
		return nil, err
	}
	return read(ctx, h)
}

// recordTree records the objects of tree that are not at the same path in
// from as part of the repo. It is for trees the server makes itself (merges)
// from objects already in the repository.
func (s *Service) recordTree(ctx context.Context, repoID uuid.UUID, from, tree hash.Hash) error {
	trees, blobs, err := object.NewObjects(ctx, s.Blobs, from, tree)
	if err != nil {
		return err
	}
	made := append(trees, blobs...)
	missing, err := s.Store.RecordRepoObjects(ctx, repoID, made, s.blobPresent)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return errObjectsPurged(len(missing))
	}
	s.sizes.queue(made)
	return nil
}

// checkInRepo verifies that every tree and blob a snapshot adds over
// baseTree is part of the repository, so a land never references content
// the lander did not upload to this repository (or that is missing). It
// checks each level of trees before reading it, so a snapshot that points
// at another repository's trees learns nothing about them: the answer is
// the same as for trees that do not exist.
func (s *Service) checkInRepo(ctx context.Context, repo store.Repo, baseTree, tree hash.Hash) error {
	if tree.IsZero() {
		return errf(CodeInvalid, "land needs a snapshot tree")
	}
	if err := s.ensureObjectsIndexed(ctx, repo.ID); err != nil {
		return err
	}
	type pair struct{ base, tree hash.Hash }
	seen := map[hash.Hash]bool{}
	level := []pair{{baseTree, tree}}
	var missingBlobs []hash.Hash
	for len(level) > 0 {
		trees := make([]hash.Hash, 0, len(level))
		for _, p := range level {
			trees = append(trees, p.tree)
		}
		missing, err := s.notInRepo(ctx, repo.ID, trees)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return errf(CodeFailedPrecondition, "snapshot trees are missing on the server: upload objects first (%d trees, e.g. %s, are not part of %s)", len(missing), missing[0].Short(), repo.Name)
		}
		var next []pair
		var blobs []hash.Hash
		for _, p := range level {
			ta, err := object.GetTree(ctx, s.Blobs, p.base)
			if err != nil {
				return err
			}
			tb, err := object.GetTree(ctx, s.Blobs, p.tree)
			if errors.Is(err, blob.ErrNotFound) {
				return errf(CodeFailedPrecondition, "snapshot trees are missing on the server: upload objects first (%v)", err)
			}
			if err != nil {
				return err
			}
			for _, e := range tb.Entries {
				old, ok := ta.Find(e.Name)
				if e.Mode == object.ModeDir {
					var oldTree hash.Hash
					if ok && old.Mode == object.ModeDir {
						oldTree = old.Hash
					}
					if e.Hash != oldTree && !seen[e.Hash] {
						seen[e.Hash] = true
						next = append(next, pair{oldTree, e.Hash})
					}
					continue
				}
				if (ok && old.Hash == e.Hash) || seen[e.Hash] {
					continue
				}
				seen[e.Hash] = true
				blobs = append(blobs, e.Hash)
			}
		}
		missing, err = s.notInRepo(ctx, repo.ID, blobs)
		if err != nil {
			return err
		}
		missingBlobs = append(missingBlobs, missing...)
		level = next
	}
	if len(missingBlobs) == 0 {
		return nil
	}
	// Name the files, as before repositories scoped objects.
	paths, err := s.pathsOf(ctx, baseTree, tree, missingBlobs)
	if err != nil || len(paths) == 0 {
		return errf(CodeFailedPrecondition, "file contents missing on the server: %d objects, e.g. %s, are not part of %s: upload them first", len(missingBlobs), missingBlobs[0].Short(), repo.Name)
	}
	if len(paths) > 5 {
		paths = append(paths[:5], fmt.Sprintf("and %d more", len(paths)-5))
	}
	return errf(CodeFailedPrecondition, "file contents missing on the server: %s", strings.Join(paths, ", "))
}

// pathsOf names the changed paths between two trees whose new content is
// one of hashes.
func (s *Service) pathsOf(ctx context.Context, from, to hash.Hash, hashes []hash.Hash) ([]string, error) {
	want := make(map[hash.Hash]bool, len(hashes))
	for _, h := range hashes {
		want[h] = true
	}
	changes, err := object.DiffTrees(ctx, s.Blobs, from, to)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range changes {
		if c.Op != object.OpDelete && want[c.New.Hash] {
			out = append(out, c.Path)
		}
	}
	return out, nil
}
