package blob

import (
	"context"
	"fmt"

	"github.com/klauspost/compress/zstd"

	"github.com/TerracodeBA/vx/internal/hash"
)

// S3 stores objects in a bucket of an S3-compatible service, under the
// client's prefix and with the disk store's layout and format
// (prefix/9f/1c/9f1c..., "vx1" + kind + zstd). Objects are immutable and
// content-addressed, so several servers can share one bucket: two writes of
// one object write the same bytes.
type S3 struct {
	c   *S3Client
	enc *zstd.Encoder
	dec *zstd.Decoder
}

// NewS3 returns a store on the client's bucket and prefix.
func NewS3(c *S3Client) (*S3, error) {
	enc, dec, err := newCodec()
	if err != nil {
		return nil, err
	}
	return &S3{c: c, enc: enc, dec: dec}, nil
}

// String names the store for logs.
func (s *S3) String() string { return s.c.String() }

// Client is the underlying client.
func (s *S3) Client() *S3Client { return s.c }

// Put implements Store. An object already in the bucket is not uploaded
// again: a HEAD request is cheaper than a PUT.
func (s *S3) Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	code, err := kindCode(kind)
	if err != nil {
		return hash.Zero, err
	}
	h := hash.Of(kind, data)
	key := objectKey(h)
	if _, ok, err := s.c.Head(ctx, key); err != nil {
		return hash.Zero, err
	} else if ok {
		return h, nil
	}
	if err := s.c.Put(ctx, key, encodeObject(s.enc, code, data), "application/octet-stream"); err != nil {
		return hash.Zero, err
	}
	return h, nil
}

// overwrite replaces an object, for copies that found a corrupt one.
func (s *S3) overwrite(ctx context.Context, kind hash.Kind, data []byte) error {
	code, err := kindCode(kind)
	if err != nil {
		return err
	}
	return s.c.Put(ctx, objectKey(hash.Of(kind, data)), encodeObject(s.enc, code, data), "application/octet-stream")
}

// Get implements Store; the object is verified against its hash.
func (s *S3) Get(ctx context.Context, h hash.Hash) (hash.Kind, []byte, error) {
	raw, err := s.c.GetBytes(ctx, objectKey(h))
	if IsNotFound(err) {
		return "", nil, fmt.Errorf("%w: %s", ErrNotFound, h.Short())
	}
	if err != nil {
		return "", nil, err
	}
	return decodeObject(s.dec, h, raw)
}

// Has implements Store with a HEAD request: nothing is downloaded.
func (s *S3) Has(ctx context.Context, h hash.Hash) (bool, error) {
	_, ok, err := s.c.Head(ctx, objectKey(h))
	return ok, err
}

// Size implements Sizer: the stored (compressed) bytes, from a HEAD request.
func (s *S3) Size(ctx context.Context, h hash.Hash) (int64, error) {
	n, ok, err := s.c.Head(ctx, objectKey(h))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, h.Short())
	}
	return n, nil
}

// List implements Lister. Keys under the prefix that are not objects of
// this layout (for example backups kept in the same bucket) are skipped.
func (s *S3) List(ctx context.Context, fn func(hash.Hash) error) error {
	return s.c.List(ctx, "", func(e ListEntry) error {
		h, ok := parseObjectKey(e.Key)
		if !ok || e.Key != objectKey(h) {
			return nil
		}
		return fn(h)
	})
}
