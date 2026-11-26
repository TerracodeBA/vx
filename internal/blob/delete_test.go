package blob_test

import (
	"context"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
)

// A purge on another server deletes an object behind this server's cache:
// Put through the cache must write it again, and Fresh must see it gone.
func TestCachedPutRestoresObjectDeletedBehindIt(t *testing.T) {
	ctx := context.Background()
	inner := blob.NewMem()
	c := blob.NewCached(inner, 1<<20)
	data := []byte("shared content\n")
	h, err := c.Put(ctx, hash.KindBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := inner.Delete(ctx, h); err != nil { // the other server's purge
		t.Fatal(err)
	}
	if ok, _ := blob.Fresh(c).Has(ctx, h); ok {
		t.Fatal("Fresh(cache).Has should ask the store behind the cache")
	}
	if _, err := c.Put(ctx, hash.KindBlob, data); err != nil {
		t.Fatal(err)
	}
	if ok, _ := inner.Has(ctx, h); !ok {
		t.Fatal("Put through the cache did not write the object back")
	}
	// Deleting through the cache empties both.
	d, ok := blob.AsDeleter(c)
	if !ok {
		t.Fatal("a cache over a memory store should delete")
	}
	if err := d.Delete(ctx, h); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Has(ctx, h); ok {
		t.Fatal("the cache still has a deleted object")
	}
	if st := c.Stats(); st.Objects != 0 || st.Bytes != 0 {
		t.Fatalf("cache after delete: %+v", st)
	}
}

func TestDiskDelete(t *testing.T) {
	d, err := blob.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, _ := d.Put(ctx, hash.KindBlob, []byte("x"))
	if err := d.Delete(ctx, h); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Has(ctx, h); ok {
		t.Fatal("Disk.Delete left the file")
	}
	if err := d.Delete(ctx, h); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
}
