package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"github.com/TerracodeBA/vx/internal/hash"
)

// On-disk format: magic "vx1", one kind byte, then the zstd-compressed data.
var diskMagic = []byte("vx1")

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
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, err
	}
	return &Disk{root: root, enc: enc, dec: dec}, nil
}

func (d *Disk) path(h hash.Hash) string {
	x := h.Hex()
	return filepath.Join(d.root, x[0:2], x[2:4], x)
}

// Put implements Store.
func (d *Disk) Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	code, err := kindCode(kind)
	if err != nil {
		return hash.Zero, err
	}
	h := hash.Of(kind, data)
	p := d.path(h)
	if _, err := os.Stat(p); err == nil {
		return h, nil
	}
	if err := ctx.Err(); err != nil {
		return hash.Zero, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return hash.Zero, err
	}
	buf := make([]byte, 0, len(diskMagic)+1+len(data)/2+64)
	buf = append(buf, diskMagic...)
	buf = append(buf, code)
	buf = d.enc.EncodeAll(data, buf)

	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return hash.Zero, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return hash.Zero, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return hash.Zero, err
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return hash.Zero, err
	}
	return h, nil
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
	if len(raw) < len(diskMagic)+1 || !bytes.Equal(raw[:len(diskMagic)], diskMagic) {
		return "", nil, fmt.Errorf("%w: %s has a bad header", ErrCorrupt, h.Short())
	}
	kind, err := codeKind(raw[len(diskMagic)])
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	data, err := d.dec.DecodeAll(raw[len(diskMagic)+1:], nil)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, h.Short(), err)
	}
	if hash.Of(kind, data) != h {
		return "", nil, fmt.Errorf("%w: %s content does not match its hash", ErrCorrupt, h.Short())
	}
	return kind, data, nil
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
