package blob

import (
	"bytes"
	"fmt"

	"github.com/klauspost/compress/zstd"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Stored format, the same on disk and in buckets: magic "vx1", one kind
// byte, then the zstd-compressed data. A disk store's directory can be
// copied into a bucket as is (and back).
var diskMagic = []byte("vx1")

// objectKey is where an object lives relative to the store's root, fanned
// out by the first two bytes of the hash: 9f/1c/9f1c....
func objectKey(h hash.Hash) string {
	x := h.Hex()
	return x[0:2] + "/" + x[2:4] + "/" + x
}

// parseObjectKey reads the hash back from the last element of a key.
func parseObjectKey(key string) (hash.Hash, bool) {
	if i := lastSlash(key); i >= 0 {
		key = key[i+1:]
	}
	if len(key) != 2*hash.Size {
		return hash.Zero, false
	}
	h, err := hash.Parse(key)
	if err != nil || h.IsZero() {
		return hash.Zero, false
	}
	return h, true
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' || s[i] == '\\' {
			return i
		}
	}
	return -1
}

func encodeObject(enc *zstd.Encoder, code byte, data []byte) []byte {
	buf := make([]byte, 0, len(diskMagic)+1+len(data)/2+64)
	buf = append(buf, diskMagic...)
	buf = append(buf, code)
	return enc.EncodeAll(data, buf)
}

// decodeObject checks the header, decompresses and verifies the hash.
func decodeObject(dec *zstd.Decoder, h hash.Hash, raw []byte) (hash.Kind, []byte, error) {
	if len(raw) < len(diskMagic)+1 || !bytes.Equal(raw[:len(diskMagic)], diskMagic) {
		return "", nil, fmt.Errorf("%w: %s has a bad header", ErrCorrupt, h.Short())
	}
	kind, err := codeKind(raw[len(diskMagic)])
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	data, err := dec.DecodeAll(raw[len(diskMagic)+1:], nil)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, h.Short(), err)
	}
	if hash.Of(kind, data) != h {
		return "", nil, fmt.Errorf("%w: %s content does not match its hash", ErrCorrupt, h.Short())
	}
	return kind, data, nil
}

func newCodec() (*zstd.Encoder, *zstd.Decoder, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, nil, err
	}
	return enc, dec, nil
}
