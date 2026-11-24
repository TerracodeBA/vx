package blob

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Disk stores objects as files under a root directory, fanned out by the
// first two bytes of the hash: root/9f/1c/9f1c....
type Disk struct {
	root string
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

// NewDisk opens (and creates) a disk store at root.
func NewDisk(root string) (*Disk, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("blob store: %w", err)
	}
	enc, dec, err := newCodec()
	if err != nil {
		return nil, err
	}
	return &Disk{root: root, enc: enc, dec: dec}, nil
}

func (d *Disk) path(h hash.Hash) string {
	return filepath.Join(d.root, filepath.FromSlash(objectKey(h)))
}

// Put implements Store.
func (d *Disk) Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	code, err := kindCode(kind)
	if err != nil {
		return hash.Zero, err
	}
	h := hash.Of(kind, data)
	if _, err := os.Stat(d.path(h)); err == nil {
		return h, nil
	}
	return h, d.write(ctx, h, code, data)
}

// overwrite replaces an object, for copies that found a corrupt one.
func (d *Disk) overwrite(ctx context.Context, kind hash.Kind, data []byte) error {
	code, err := kindCode(kind)
	if err != nil {
		return err
	}
	return d.write(ctx, hash.Of(kind, data), code, data)
}

func (d *Disk) write(ctx context.Context, h hash.Hash, code byte, data []byte) error {
	p := d.path(h)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	buf := encodeObject(d.enc, code, data)

	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Get implements Store.
func (d *Disk) Get(ctx context.Context, h hash.Hash) (hash.Kind, []byte, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(d.path(h))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("%w: %s", ErrNotFound, h.Short())
	}
	if err != nil {
		return "", nil, err
	}
	return decodeObject(d.dec, h, raw)
}

// Has implements Store.
func (d *Disk) Has(ctx context.Context, h hash.Hash) (bool, error) {
	_, err := os.Stat(d.path(h))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Root is the directory the store keeps its objects in.
func (d *Disk) Root() string { return d.root }

// String names the store for logs.
func (d *Disk) String() string { return "disk " + d.root }

// Size implements Sizer: the bytes the object's file takes.
func (d *Disk) Size(_ context.Context, h hash.Hash) (int64, error) {
	fi, err := os.Stat(d.path(h))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, h.Short())
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// List implements Lister. Temporary files of unfinished writes and anything
// else that is not an object are skipped.
func (d *Disk) List(ctx context.Context, fn func(hash.Hash) error) error {
	return filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() {
			return ctx.Err()
		}
		h, ok := parseObjectKey(e.Name())
		if !ok || p != d.path(h) {
			return nil
		}
		return fn(h)
	})
}
