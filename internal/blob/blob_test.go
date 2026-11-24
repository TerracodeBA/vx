package blob

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/TerracodeBA/vx/internal/hash"
)

func stores(t *testing.T) map[string]Store {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Store{"disk": d, "mem": NewMem()}
}

func TestPutGet(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			data := bytes.Repeat([]byte("package main\n"), 1000)
			h, err := s.Put(ctx, hash.KindBlob, data)
			if err != nil {
				t.Fatal(err)
			}
			if h != hash.Of(hash.KindBlob, data) {
				t.Fatal("Put returned wrong hash")
			}
			h2, err := s.Put(ctx, hash.KindBlob, data)
			if err != nil || h2 != h {
				t.Fatal("second Put should be a no-op with the same hash")
			}
			kind, got, err := s.Get(ctx, h)
			if err != nil || kind != hash.KindBlob || !bytes.Equal(got, data) {
				t.Fatalf("Get = %v, %d bytes, %v", kind, len(got), err)
			}
			if ok, _ := s.Has(ctx, h); !ok {
				t.Fatal("Has = false")
			}
			if _, err := GetKind(ctx, s, hash.KindTree, h); err == nil {
				t.Fatal("GetKind should reject the wrong kind")
			}
			missing := hash.Of(hash.KindBlob, []byte("nope"))
			if _, _, err := s.Get(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing Get err = %v", err)
			}
			if ok, _ := s.Has(ctx, missing); ok {
				t.Fatal("Has(missing) = true")
			}
			empty, err := s.Put(ctx, hash.KindBlob, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, got, err := s.Get(ctx, empty); err != nil || len(got) != 0 {
				t.Fatal("empty object round trip failed")
			}
		})
	}
}

func TestDiskDetectsCorruption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := NewDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := d.Put(ctx, hash.KindBlob, []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	// Replace the object with a valid encoding of different content.
	other := t.TempDir()
	d2, _ := NewDisk(other)
	h2, _ := d2.Put(ctx, hash.KindBlob, []byte("tampered"))
	raw, err := os.ReadFile(d2.path(h2))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.path(h), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Get(ctx, h); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt, got %v", err)
	}
	if err := os.WriteFile(d.path(h), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Get(ctx, h); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt for garbage, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, h.Hex()[:2])); err != nil {
		t.Fatal("expected fan-out directory")
	}
}
