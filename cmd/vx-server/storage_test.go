package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/blob/s3test"
	"github.com/TerracodeBA/vx/internal/hash"
)

func run(t *testing.T, stdin []byte, args ...string) (string, string, error) {
	t.Helper()
	cmd := root()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(bytes.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func TestBlobsCopyToBucketThenServeFromIt(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	disk, err := blob.NewDisk(filepath.Join(data, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	var hs []hash.Hash
	for i := range 40 {
		h, _ := disk.Put(ctx, hash.KindBlob, bytes.Repeat([]byte{byte(i)}, 100+i))
		hs = append(hs, h)
	}
	f := s3test.New(t)
	t.Setenv("VX_BLOB_URL", "")
	t.Setenv("VX_BLOB_TO_ACCESS_KEY_ID", f.Creds.AccessKeyID)
	t.Setenv("VX_BLOB_TO_SECRET_ACCESS_KEY", f.Creds.SecretAccessKey)
	to := f.BucketURL("prod")

	out, _, err := run(t, nil, "blobs", "copy", "--data", data, "--to", to)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "40 objects, 40 copied") || !strings.Contains(out, "VX_BLOB_URL="+to) {
		t.Fatalf("first copy said:\n%s", out)
	}
	out, _, err = run(t, nil, "blobs", "copy", "--data", data, "--to", to)
	if err != nil || !strings.Contains(out, "0 copied (0 B), 40 already there") {
		t.Fatalf("second copy: %v\n%s", err, out)
	}
	// After switching, the server's store (VX_BLOB_URL) has every object.
	t.Setenv("VX_BLOB_ACCESS_KEY_ID", f.Creds.AccessKeyID)
	t.Setenv("VX_BLOB_SECRET_ACCESS_KEY", f.Creds.SecretAccessKey)
	bucket, err := openBlobs(to, data, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if _, _, err := bucket.Get(ctx, h); err != nil {
			t.Fatalf("object %s missing after the copy: %v", h.Short(), err)
		}
	}
	if _, _, err := run(t, nil, "blobs", "copy", "--data", data, "--from", to, "--to", to); err == nil {
		t.Fatal("copying a store onto itself should be refused")
	}
	if _, _, err := run(t, nil, "blobs", "copy", "--data", data); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Fatalf("copy without --to = %v", err)
	}
}

func TestBackupPutGetListPrune(t *testing.T) {
	f := s3test.New(t)
	t.Setenv("VX_BACKUP_URL", f.BucketURL("app.example.com"))
	t.Setenv("VX_BACKUP_ACCESS_KEY_ID", f.Creds.AccessKeyID)
	t.Setenv("VX_BACKUP_SECRET_ACCESS_KEY", f.Creds.SecretAccessKey)
	dump := make([]byte, blob.MinPartSize+4321) // two parts
	rand.Read(dump)

	out, _, err := run(t, dump, "backup", "put", "vx-db-2026-11-01-0315.dump")
	if err != nil || !strings.Contains(out, "Uploaded vx-db-2026-11-01-0315.dump") {
		t.Fatalf("put: %v\n%s", err, out)
	}
	if _, _, err := run(t, []byte("tar"), "backup", "put", "vx-data-2026-11-01-0315.tgz"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, []byte("newer"), "backup", "put", "vx-db-2026-11-20-0315.dump"); err != nil {
		t.Fatal(err)
	}
	got, _, err := run(t, nil, "backup", "get", "vx-db-2026-11-01-0315.dump")
	if err != nil || !bytes.Equal([]byte(got), dump) {
		t.Fatalf("get: %v (%d bytes)", err, len(got))
	}
	out, _, err = run(t, nil, "backup", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "vx-data-2026-11-01-0315.tgz\t3\t") || strings.Contains(out, ".sha256") {
		t.Fatalf("list:\n%s", out)
	}

	// A damaged download fails.
	f.SetObject("app.example.com/vx-db-2026-11-20-0315.dump", []byte("newEr"))
	if _, _, err := run(t, nil, "backup", "get", "vx-db-2026-11-20-0315.dump"); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("get of a damaged backup = %v", err)
	}
	if _, _, err := run(t, nil, "backup", "get", "vx-db-1999.dump"); err == nil || !strings.Contains(err.Error(), "backup list") {
		t.Fatalf("get of a missing backup = %v", err)
	}
	if _, _, err := run(t, nil, "backup", "put", "../escape"); err == nil {
		t.Fatal("a name with a path was accepted")
	}

	// Prune drops old backups but always keeps the newest of each kind.
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, k := range []string{"vx-db-2026-11-01-0315.dump", "vx-data-2026-11-01-0315.tgz"} {
		f.SetModified("app.example.com/"+k, old)
	}
	out, _, err = run(t, nil, "backup", "prune", "--keep-days", "30")
	if err != nil || !strings.Contains(out, "Deleted 1 backups") {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	keys := strings.Join(f.Keys(), " ")
	if strings.Contains(keys, "vx-db-2026-11-01") || !strings.Contains(keys, "vx-data-2026-11-01-0315.tgz") || !strings.Contains(keys, "vx-db-2026-11-20") {
		t.Fatalf("after prune: %s", keys)
	}

	t.Setenv("VX_BACKUP_URL", "")
	if _, _, err := run(t, nil, "backup", "list"); err == nil || !strings.Contains(err.Error(), "VX_BACKUP_URL") {
		t.Fatalf("list without a bucket = %v", err)
	}
}
