package blob_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/blob/s3test"
	"github.com/TerracodeBA/vx/internal/hash"
)

// storeSuite is what every Store must do.
func storeSuite(t *testing.T, s blob.Store) {
	t.Helper()
	ctx := context.Background()
	data := bytes.Repeat([]byte("package main\n"), 1000)
	h, err := s.Put(ctx, hash.KindBlob, data)
	if err != nil {
		t.Fatal(err)
	}
	if h != hash.Of(hash.KindBlob, data) {
		t.Fatal("Put returned the wrong hash")
	}
	if h2, err := s.Put(ctx, hash.KindBlob, data); err != nil || h2 != h {
		t.Fatalf("second Put = %v, %v", h2, err)
	}
	kind, got, err := s.Get(ctx, h)
	if err != nil || kind != hash.KindBlob || !bytes.Equal(got, data) {
		t.Fatalf("Get = %v, %d bytes, %v", kind, len(got), err)
	}
	if ok, err := s.Has(ctx, h); !ok || err != nil {
		t.Fatalf("Has = %v, %v", ok, err)
	}
	if _, err := blob.GetKind(ctx, s, hash.KindTree, h); err == nil {
		t.Fatal("GetKind should reject the wrong kind")
	}
	missing := hash.Of(hash.KindBlob, []byte("nope"))
	if _, _, err := s.Get(ctx, missing); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing Get err = %v", err)
	}
	if ok, err := s.Has(ctx, missing); ok || err != nil {
		t.Fatalf("Has(missing) = %v, %v", ok, err)
	}
	empty, err := s.Put(ctx, hash.KindTree, nil)
	if err != nil {
		t.Fatal(err)
	}
	if k, got, err := s.Get(ctx, empty); err != nil || len(got) != 0 || k != hash.KindTree {
		t.Fatalf("empty object round trip: %v %d %v", k, len(got), err)
	}
	if sz, ok := s.(blob.Sizer); ok {
		n, err := sz.Size(ctx, h)
		if err != nil || n <= 0 || n > int64(len(data)) {
			t.Fatalf("Size = %d, %v", n, err)
		}
	}
	if l, ok := s.(blob.Lister); ok {
		seen := map[hash.Hash]bool{}
		if err := l.List(ctx, func(x hash.Hash) error { seen[x] = true; return nil }); err != nil {
			t.Fatal(err)
		}
		if !seen[h] || !seen[empty] {
			t.Fatalf("List misses objects: %d listed", len(seen))
		}
	}
	if d, ok := blob.AsDeleter(s); ok {
		if err := d.Delete(ctx, h); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Has(ctx, h); ok || err != nil {
			t.Fatalf("Has after Delete = %v, %v", ok, err)
		}
		if _, _, err := s.Get(ctx, h); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("Get after Delete err = %v", err)
		}
		if err := d.Delete(ctx, h); err != nil {
			t.Fatalf("deleting a missing object: %v", err)
		}
		if h2, err := s.Put(ctx, hash.KindBlob, data); err != nil || h2 != h {
			t.Fatalf("Put after Delete = %v, %v", h2, err)
		}
		if _, got, err := s.Get(ctx, h); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get after putting back = %d bytes, %v", len(got), err)
		}
	}
}

func openFake(t *testing.T, f *s3test.Server, rawURL string, hc *http.Client) blob.Store {
	t.Helper()
	s, err := blob.Open(rawURL, blob.OpenOptions{Getenv: f.Env(""), HTTPClient: hc, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3StorePathAndVirtualStyle(t *testing.T) {
	f := s3test.New(t)
	t.Run("path", func(t *testing.T) {
		storeSuite(t, openFake(t, f, f.BucketURL("vx/objects"), nil))
		for _, k := range f.Keys() {
			if !strings.HasPrefix(k, "vx/objects/") {
				t.Fatalf("key %q outside the prefix", k)
			}
		}
	})
	t.Run("virtual", func(t *testing.T) {
		// bucket.127.0.0.1 does not resolve: dial the fake for every host.
		addr := strings.TrimPrefix(f.URL, "http://")
		hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
		storeSuite(t, openFake(t, f, "s3://vx-test/virtual?style=virtual&endpoint="+f.URL, hc))
	})
	t.Run("cached", func(t *testing.T) {
		s, err := blob.Open(f.BucketURL("cached"), blob.OpenOptions{Getenv: f.Env("")})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := s.(*blob.Cached); !ok {
			t.Fatalf("bucket stores are cached by default, got %T", s)
		}
		storeSuite(t, s)
	})
}

func TestS3RetriesWithBackoff(t *testing.T) {
	f := s3test.New(t)
	ctx := context.Background()
	cfg, _, err := blob.ParseBucketURL(f.BucketURL("r"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Credentials, cfg.RetryBase, cfg.MaxAttempts = f.Creds, time.Millisecond, 4
	c, err := blob.NewS3Client(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := blob.NewS3(c)

	f.FailNext(503, 500, 429)
	h, err := s.Put(ctx, hash.KindBlob, []byte("retried"))
	if err != nil {
		t.Fatalf("Put after three failures: %v", err)
	}
	f.FailNext(503, 503, 503, 503)
	if _, _, err := s.Get(ctx, h); err == nil || !strings.Contains(err.Error(), "gave up after 4 tries") {
		t.Fatalf("Get after four failures = %v", err)
	}
	// Errors that retrying cannot fix are not retried, and say what to do.
	f.FailNext(403)
	before := f.Count(http.MethodHead) + f.Count(http.MethodGet)
	if _, err := s.Has(ctx, h); err == nil || !strings.Contains(err.Error(), "check the access key") {
		t.Fatalf("Has after 403 = %v", err)
	}
	if after := f.Count(http.MethodHead) + f.Count(http.MethodGet); after != before {
		t.Fatalf("a 403 was retried: %d requests", after-before)
	}
	// A body cut short is read again.
	f.CutNextBodies(1)
	if _, got, err := s.Get(ctx, h); err != nil || string(got) != "retried" {
		t.Fatalf("Get with a cut body = %q, %v", got, err)
	}
}

func TestS3WrongCredentialsAreRefusedAndNotShown(t *testing.T) {
	f := s3test.New(t)
	secret := "wrong-secret-value-123"
	s, err := blob.Open(f.BucketURL(""), blob.OpenOptions{NoCache: true, Getenv: func(k string) string {
		return map[string]string{"VX_BLOB_ACCESS_KEY_ID": f.Creds.AccessKeyID, "VX_BLOB_SECRET_ACCESS_KEY": secret}[k]
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(context.Background(), hash.KindBlob, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Put with a wrong secret = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprintf("%v %+v %#v", s, s, s), secret) {
		t.Fatal("the secret shows up in an error or in the store's printed form")
	}
}

func TestCredentialsRedacted(t *testing.T) {
	c := blob.Credentials{AccessKeyID: "AKIAVISIBLE", SecretAccessKey: "very-secret", SessionToken: "tok-secret"}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "creds", c)
	cfg := blob.S3Config{Bucket: "b", Credentials: c}
	for _, out := range []string{fmt.Sprint(c), fmt.Sprintf("%+v %#v %s", c, c, c), fmt.Sprintf("%+v %#v", cfg, cfg), logs.String()} {
		if strings.Contains(out, "very-secret") || strings.Contains(out, "tok-secret") || strings.Contains(out, "AKIAVISIBLE") {
			t.Fatalf("credentials printed: %s", out)
		}
	}
}

// The example from the AWS documentation ("Signature Calculations for the
// Authorization Header", GET Object).
func TestSigV4KnownVector(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	cr := blob.Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	blob.SignV4ForTest(req, cr, "us-east-1", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

func TestS3ListPaginatesAndSkipsOtherKeys(t *testing.T) {
	f := s3test.New(t)
	s := openFake(t, f, f.BucketURL("p"), nil)
	ctx := context.Background()
	want := map[hash.Hash]bool{}
	for i := range 25 {
		h, err := s.Put(ctx, hash.KindBlob, []byte(fmt.Sprint("object ", i)))
		if err != nil {
			t.Fatal(err)
		}
		want[h] = true
	}
	f.SetObject("p/backups/vx-db-2026.dump", []byte("not an object"))
	f.SetObject("p/ab/cd/short", []byte("not an object"))
	f.SetObject("other/00/00/"+strings.Repeat("0", 64), []byte("outside the prefix"))
	got := map[hash.Hash]bool{}
	if err := s.(blob.Lister).List(ctx, func(h hash.Hash) error { got[h] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d objects, want %d", len(got), len(want))
	}
	for h := range want {
		if !got[h] {
			t.Fatalf("List misses %s", h.Short())
		}
	}
}

func TestS3DetectsCorruption(t *testing.T) {
	f := s3test.New(t)
	s := openFake(t, f, f.BucketURL(""), nil)
	ctx := context.Background()
	h, _ := s.Put(ctx, hash.KindBlob, []byte("original"))
	other, _ := s.Put(ctx, hash.KindBlob, []byte("tampered"))
	key := func(h hash.Hash) string { x := h.Hex(); return x[:2] + "/" + x[2:4] + "/" + x }
	raw, _ := f.Object(key(other))
	f.SetObject(key(h), raw)
	if _, _, err := s.Get(ctx, h); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("swapped object: %v", err)
	}
	f.SetObject(key(h), []byte("garbage"))
	if _, _, err := s.Get(ctx, h); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("garbage object: %v", err)
	}
}

func TestS3StreamsLargeUploadsInParts(t *testing.T) {
	f := s3test.New(t)
	cfg, _, _ := blob.ParseBucketURL(f.BucketURL("backups"))
	cfg.Credentials, cfg.RetryBase = f.Creds, time.Millisecond
	c, err := blob.NewS3Client(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	big := make([]byte, 2*blob.MinPartSize+12345)
	rand.Read(big)
	n, err := c.PutStream(ctx, "vx-db.dump", bytes.NewReader(big), blob.MinPartSize, "")
	if err != nil || n != int64(len(big)) {
		t.Fatalf("PutStream = %d, %v", n, err)
	}
	if got, _ := f.Object("backups/vx-db.dump"); !bytes.Equal(got, big) {
		t.Fatal("multipart upload stored different bytes")
	}
	if f.Count(http.MethodPost) != 2 {
		t.Fatalf("want one start and one complete, got %d POSTs", f.Count(http.MethodPost))
	}
	// A small stream is one PUT.
	if n, err := c.PutStream(ctx, "small", strings.NewReader("tiny"), 0, ""); err != nil || n != 4 {
		t.Fatalf("small PutStream = %d, %v", n, err)
	}
	// A download whose connection breaks resumes where it stopped.
	f.CutNextBodies(1)
	rc, size, err := c.Open(ctx, "vx-db.dump")
	if err != nil || size != int64(len(big)) {
		t.Fatalf("Open = %d, %v", size, err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("resumed download: %d bytes, %v", len(got), err)
	}
	// A reader that fails part way aborts the upload: nothing is left.
	_, err = c.PutStream(ctx, "broken", io.MultiReader(bytes.NewReader(big[:blob.MinPartSize+1]), errReader{}), blob.MinPartSize, "")
	if err == nil {
		t.Fatal("PutStream of a failing reader succeeded")
	}
	if f.OpenUploads() != 0 {
		t.Fatal("a failed multipart upload was not aborted")
	}
	if _, ok := f.Object("backups/broken"); ok {
		t.Fatal("a failed upload left an object")
	}
	// Listing and deleting.
	var keys []string
	if err := c.List(ctx, "", func(e blob.ListEntry) error { keys = append(keys, e.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "small,vx-db.dump" {
		t.Fatalf("List = %v", keys)
	}
	if err := c.Delete(ctx, "small"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Head(ctx, "small"); ok {
		t.Fatal("Delete left the object")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk read error") }

func TestCacheIsBounded(t *testing.T) {
	ctx := context.Background()
	inner := blob.NewMem()
	const max = 64 * 1024
	c := blob.NewCached(inner, max)
	var hs []hash.Hash
	for i := range 200 {
		h, err := c.Put(ctx, hash.KindBlob, bytes.Repeat([]byte{byte(i)}, 1000))
		if err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
		if st := c.Stats(); st.Bytes > max {
			t.Fatalf("cache holds %d bytes, over its %d bound", st.Bytes, max)
		}
	}
	st := c.Stats()
	if st.Objects == 0 || st.Objects >= 200 {
		t.Fatalf("cache holds %d objects", st.Objects)
	}
	// The newest objects are cached, the oldest were evicted; both read.
	hits := c.Stats().Hits
	if _, _, err := c.Get(ctx, hs[199]); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Hits != hits+1 {
		t.Fatal("the newest object was not served from the cache")
	}
	misses := c.Stats().Misses
	if _, _, err := c.Get(ctx, hs[0]); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Misses != misses+1 {
		t.Fatal("the oldest object should have been evicted")
	}
	// Using an object keeps it: touch hs[0] (now cached) and add more.
	for i := range 40 {
		c.Get(ctx, hs[0])
		c.Put(ctx, hash.KindBlob, bytes.Repeat([]byte{byte(i), 1}, 1000))
	}
	hits = c.Stats().Hits
	c.Get(ctx, hs[0])
	if c.Stats().Hits != hits+1 {
		t.Fatal("a recently used object was evicted")
	}
	// Large objects are never cached.
	big, _ := c.Put(ctx, hash.KindBlob, make([]byte, max/4))
	misses = c.Stats().Misses
	c.Get(ctx, big)
	if c.Stats().Misses != misses+1 {
		t.Fatal("an object over an eighth of the cache was cached")
	}
	// Callers get copies: changing one does not change the cache.
	_, got, _ := c.Get(ctx, hs[199])
	got[0] ^= 0xff
	if _, again, _ := c.Get(ctx, hs[199]); again[0] == got[0] {
		t.Fatal("Get returned the cache's own slice")
	}
}

func TestCacheConcurrentUse(t *testing.T) {
	ctx := context.Background()
	c := blob.NewCached(blob.NewMem(), 32*1024)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				data := []byte(fmt.Sprintf("%d-%d", g, i%50))
				h, err := c.Put(ctx, hash.KindBlob, data)
				if err != nil {
					t.Error(err)
					return
				}
				if _, got, err := c.Get(ctx, h); err != nil || !bytes.Equal(got, data) {
					t.Errorf("Get = %q, %v", got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if st := c.Stats(); st.Bytes > 32*1024 {
		t.Fatalf("cache over its bound: %d", st.Bytes)
	}
}

func TestCopyResumesAndVerifies(t *testing.T) {
	ctx := context.Background()
	src, err := blob.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := map[hash.Hash][]byte{}
	for i := range 60 {
		data := []byte(fmt.Sprintf("file %d\n%s", i, strings.Repeat("x", i*10)))
		h, _ := src.Put(ctx, hash.KindBlob, data)
		want[h] = data
	}
	// Leftovers of an interrupted disk write are not objects.
	os.WriteFile(filepath.Join(src.Root(), ".tmp-123"), []byte("partial"), 0o644)

	f := s3test.New(t)
	dst := openFake(t, f, f.BucketURL("objects"), nil)

	// The first run breaks part way: the bucket starts failing.
	cfg, _, _ := blob.ParseBucketURL(f.BucketURL("objects"))
	cfg.Credentials, cfg.MaxAttempts, cfg.RetryBase = f.Creds, 1, time.Millisecond
	c, _ := blob.NewS3Client(cfg)
	flaky, _ := blob.NewS3(c)
	first, _ := blob.Copy(ctx, src, &failAfter{Store: flaky, n: 20}, blob.CopyOptions{Jobs: 2})
	if first.Copied == 0 || first.Copied >= 60 {
		t.Fatalf("interrupted copy copied %d", first.Copied)
	}

	// Running it again finishes the job, skipping what is there.
	var progress []blob.CopyStats
	second, err := blob.Copy(ctx, src, dst, blob.CopyOptions{Jobs: 4, Progress: func(s blob.CopyStats) { progress = append(progress, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if second.Seen != 60 || second.Copied+second.Skipped != 60 || second.Skipped < first.Copied {
		t.Fatalf("resumed copy: %+v after %+v", second, first)
	}
	if len(progress) == 0 || progress[len(progress)-1].Seen != 60 {
		t.Fatalf("progress = %+v", progress)
	}
	for h, data := range want {
		if _, got, err := dst.Get(ctx, h); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("copied %s: %v", h.Short(), err)
		}
	}

	// A corrupt object in the destination is found and replaced with
	// --verify-existing; a plain run trusts it.
	var victim hash.Hash
	for h := range want {
		victim = h
		break
	}
	x := victim.Hex()
	f.SetObject("objects/"+x[:2]+"/"+x[2:4]+"/"+x, []byte("garbage"))
	third, err := blob.Copy(ctx, src, dst, blob.CopyOptions{VerifyExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	if third.Copied != 1 || third.Verified != 59 {
		t.Fatalf("verifying copy: %+v", third)
	}
	if _, _, err := dst.Get(ctx, victim); err != nil {
		t.Fatalf("corrupt object not repaired: %v", err)
	}

	// A corrupt source object stops the copy with an error naming it.
	src2 := blob.NewMem()
	h, _ := src2.Put(ctx, hash.KindBlob, []byte("fine"))
	if _, err := blob.Copy(ctx, &corruptStore{Mem: src2}, blob.NewMem(), blob.CopyOptions{}); err == nil || !strings.Contains(err.Error(), h.Short()) {
		t.Fatalf("copy of a corrupt source = %v", err)
	}
}

// failAfter fails every Put after the first n.
type failAfter struct {
	blob.Store
	mu sync.Mutex
	n  int
}

func (f *failAfter) Put(ctx context.Context, k hash.Kind, d []byte) (hash.Hash, error) {
	f.mu.Lock()
	f.n--
	n := f.n
	f.mu.Unlock()
	if n < 0 {
		return hash.Zero, errors.New("bucket unreachable")
	}
	return f.Store.Put(ctx, k, d)
}

type corruptStore struct{ *blob.Mem }

func (c *corruptStore) Get(ctx context.Context, h hash.Hash) (hash.Kind, []byte, error) {
	return "", nil, fmt.Errorf("%w: %s", blob.ErrCorrupt, h.Short())
}

func TestOpenURLs(t *testing.T) {
	dir := t.TempDir()
	if s, err := blob.Open("", blob.OpenOptions{DiskDir: dir}); err != nil {
		t.Fatal(err)
	} else if d, ok := s.(*blob.Disk); !ok || d.Root() != dir {
		t.Fatalf("empty URL opens %T", s)
	}
	if s, err := blob.Open("file://"+dir+"/x", blob.OpenOptions{}); err != nil {
		t.Fatal(err)
	} else if d, ok := s.(*blob.Disk); !ok || d.Root() != dir+"/x" {
		t.Fatalf("file URL opens %v", s)
	}
	for _, tc := range []struct {
		url                    string
		bucket, prefix, region string
		endpoint               string
		path                   bool
		cache                  int64
	}{
		{"s3://my-bucket", "my-bucket", "", "", "", false, blob.DefaultCacheBytes},
		{"s3://my-bucket/vx/objects/?region=eu-central-1", "my-bucket", "vx/objects", "eu-central-1", "", false, blob.DefaultCacheBytes},
		{"s3://b/p?endpoint=http://127.0.0.1:9000&cache=0", "b", "p", "", "http://127.0.0.1:9000", true, 0},
		{"s3://b?endpoint=acc.r2.cloudflarestorage.com&region=auto&cache=16MB", "b", "", "auto", "https://acc.r2.cloudflarestorage.com", true, 16 << 20},
		{"s3://b?endpoint=https://minio.internal/&style=virtual", "b", "", "", "https://minio.internal", false, blob.DefaultCacheBytes},
		{"gs://b/vx", "b", "vx", "auto", "https://storage.googleapis.com", true, blob.DefaultCacheBytes},
	} {
		cfg, cache, err := blob.ParseBucketURL(tc.url)
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if cfg.Bucket != tc.bucket || cfg.Prefix != tc.prefix || cfg.Region != tc.region || cfg.Endpoint != tc.endpoint || cfg.PathStyle != tc.path || cache != tc.cache {
			t.Errorf("%s = %+v cache %d", tc.url, cfg, cache)
		}
	}
	for url, want := range map[string]string{
		"s3://key:secret@b/p":       "not in the URL",
		"s3://b?regoin=x":           `unknown parameter "regoin"`,
		"s3://b?style=dns":          "style=path or style=virtual",
		"s3://b?cache=lots":         "64MB",
		"s3:///p":                   "names no bucket",
		"ftp://b/p":                 "s3://, gs:// or file://",
		"s3://b?endpoint=ftp://x/y": "not an http(s) URL",
	} {
		_, err := blob.Open(url, blob.OpenOptions{Getenv: func(k string) string {
			return map[string]string{"VX_BLOB_ACCESS_KEY_ID": "k", "VX_BLOB_SECRET_ACCESS_KEY": "s"}[k]
		}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Open(%s) = %v, want %q", url, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") && !strings.Contains(want, "URL") {
			t.Errorf("Open(%s) error shows a secret: %v", url, err)
		}
	}
}

func TestCredentialsFromEnvAndFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret"), []byte("from-file\n"), 0o600)
	env := map[string]string{"VX_BACKUP_ACCESS_KEY_ID": "id", "VX_BACKUP_SECRET_ACCESS_KEY_FILE": filepath.Join(dir, "secret")}
	c, err := blob.CredentialsFromEnv("VX_BACKUP", func(k string) string { return env[k] })
	if err != nil || c.AccessKeyID != "id" || c.SecretAccessKey != "from-file" {
		t.Fatalf("from file: %v", err)
	}
	env = map[string]string{"AWS_ACCESS_KEY_ID": "aws-id", "AWS_SECRET_ACCESS_KEY": "aws-secret", "AWS_SESSION_TOKEN": "t"}
	c, err = blob.CredentialsFromEnv("", func(k string) string { return env[k] })
	if err != nil || c.AccessKeyID != "aws-id" || c.SessionToken != "t" {
		t.Fatalf("AWS fallback: %v", err)
	}
	_, err = blob.CredentialsFromEnv("VX_BLOB", func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "VX_BLOB_ACCESS_KEY_ID") {
		t.Fatalf("missing credentials = %v", err)
	}
	env = map[string]string{"VX_BLOB_ACCESS_KEY_ID": "id", "VX_BLOB_SECRET_ACCESS_KEY_FILE": filepath.Join(dir, "missing")}
	if _, err := blob.CredentialsFromEnv("", func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "VX_BLOB_SECRET_ACCESS_KEY_FILE") {
		t.Fatalf("unreadable file = %v", err)
	}
}

// TestS3AgainstMinIO runs the store against a real S3 implementation. Start
// one and set VX_TEST_S3_URL, VX_TEST_S3_ACCESS_KEY_ID and
// VX_TEST_S3_SECRET_ACCESS_KEY (make test-minio does both); VX_REQUIRE_MINIO=1
// makes a missing one fail.
func TestS3AgainstMinIO(t *testing.T) {
	url := os.Getenv("VX_TEST_S3_URL")
	if url == "" {
		if os.Getenv("VX_REQUIRE_MINIO") != "" {
			t.Fatal("VX_REQUIRE_MINIO is set but VX_TEST_S3_URL is not: start MinIO (make minio)")
		}
		t.Skip("no MinIO: set VX_TEST_S3_URL (make minio)")
	}
	ctx := context.Background()
	prefix := fmt.Sprintf("vx-test-%d", time.Now().UnixNano())
	cfg, _, err := blob.ParseBucketURL(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Prefix = strings.Trim(cfg.Prefix+"/"+prefix, "/")
	if cfg.Credentials, err = blob.CredentialsFromEnv("VX_TEST_S3", nil); err != nil {
		t.Fatal(err)
	}
	c, err := blob.NewS3Client(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.List(context.Background(), "", func(e blob.ListEntry) error { return c.Delete(context.Background(), e.Key) })
	})
	s, _ := blob.NewS3(c)
	storeSuite(t, s)
	storeSuite(t, blob.NewCached(s, 1<<20))

	big := make([]byte, blob.MinPartSize*2+999)
	rand.Read(big)
	if _, err := c.PutStream(ctx, "backups/big.dump", bytes.NewReader(big), blob.MinPartSize, ""); err != nil {
		t.Fatal(err)
	}
	rc, _, err := c.Open(ctx, "backups/big.dump")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("multipart round trip: %d bytes, %v", len(got), err)
	}

	src := blob.NewMem()
	for i := range 30 {
		src.Put(ctx, hash.KindBlob, []byte(fmt.Sprint("minio copy ", i)))
	}
	st, err := blob.Copy(ctx, src, s, blob.CopyOptions{})
	if err != nil || st.Copied != 30 {
		t.Fatalf("copy to MinIO: %+v, %v", st, err)
	}
	n := 0
	if err := s.List(ctx, func(hash.Hash) error { n++; return nil }); err != nil || n < 30 {
		t.Fatalf("List on MinIO: %d, %v", n, err)
	}
}
