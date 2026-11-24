package cli_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
)

// recordingBlobs remembers every object the server stores, to show that a
// client really sent the bytes of an object it uploads.
type recordingBlobs struct {
	blob.Store
	mu   sync.Mutex
	puts [][]byte
}

func (r *recordingBlobs) Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	r.mu.Lock()
	r.puts = append(r.puts, append([]byte(nil), data...))
	r.mu.Unlock()
	return r.Store.Put(ctx, kind, data)
}

func (r *recordingBlobs) received(data []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.puts {
		if bytes.Equal(p, data) {
			return true
		}
	}
	return false
}

func (r *recordingBlobs) reset() {
	r.mu.Lock()
	r.puts = nil
	r.mu.Unlock()
}

func newObjectsHarness(t *testing.T) (*harness, *recordingBlobs) {
	t.Helper()
	st := storetest.New(t)
	disk, err := blob.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blobs := &recordingBlobs{Store: disk}
	svc := service.New(st, blobs, nil)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, server: srv.URL, home: t.TempDir(), st: st}, blobs
}

func (h *harness) person(handle string) string {
	h.t.Helper()
	ctx := context.Background()
	p, err := h.st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: handle, DisplayName: handle, Email: handle + "@example.com"})
	if err != nil {
		h.t.Fatal(err)
	}
	tok, err := h.st.CreateToken(ctx, p.ID, "test")
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// Objects are scoped to repositories, end to end: bob cannot fetch alice's
// file by its hash, his vx uploads the same content again (bytes and all)
// when he lands it into his own repository, and old clients that send no
// repository are told to update.
func TestObjectsScopedToRepositoriesE2E(t *testing.T) {
	h, blobs := newObjectsHarness(t)
	alice := h.user("alice", h.person("alice"), t.TempDir())
	bobToken := h.person("bob")
	bob := h.user("bob", bobToken, t.TempDir())

	secret := "API_KEY=alice-only-0123456789\n"
	alice.must("repo", "create", "shop-api")
	alice.must("effort", "new", "Config", "--repo", "shop-api")
	alice.must("clone", "shop-api", "config", "wc")
	alice.dir = filepath.Join(alice.dir, "wc")
	alice.must("new", "Add config")
	alice.write("secret.env", secret)
	alice.must("land")
	h1 := hash.Of(hash.KindBlob, []byte(secret))

	bob.must("repo", "create", "notes")
	bob.must("effort", "new", "Notes", "--repo", "notes")
	bob.must("clone", "notes", "notes", "wc")
	bob.dir = filepath.Join(bob.dir, "wc")

	ctx := context.Background()
	c := client.New(h.server, bobToken)
	get := func(repo string) error {
		_, err := c.Objects.GetObjects(ctx, connect.NewRequest(&vxv1.GetObjectsRequest{Repo: repo, Hashes: []string{h1.String()}}))
		return err
	}
	// The blob is in the server's store, but not part of bob's repository.
	if err := get("notes"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("bob get through his repo = %v, want not found", err)
	}
	// Nor can he read it through alice's repository, which he cannot read.
	if err := get("shop-api"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("bob get through alice's repo = %v, want not found", err)
	}
	miss, err := c.Objects.MissingObjects(ctx, connect.NewRequest(&vxv1.MissingObjectsRequest{Repo: "notes", Hashes: []string{h1.String()}}))
	if err != nil || len(miss.Msg.Missing) != 1 {
		t.Fatalf("missing = %v %v, want the hash reported missing", miss, err)
	}

	// Bob lands the same content: his vx sends the bytes, which the
	// server stores again (a no-op on disk), proving he has them.
	blobs.reset()
	bob.must("new", "Same content, by coincidence")
	bob.write("copy.env", secret)
	bob.must("land")
	if !blobs.received([]byte(secret)) {
		t.Fatal("bob's vx did not upload the file's bytes")
	}
	if err := get("notes"); err != nil {
		t.Fatalf("bob get after his own upload = %v", err)
	}
	// A second working copy of bob's repository clones and syncs it.
	other := h.user("bob2", bobToken, t.TempDir())
	other.must("clone", "notes", "notes", "wc")
	other.dir = filepath.Join(other.dir, "wc")
	if got := other.read("copy.env"); got != secret {
		t.Fatalf("clone has %q", got)
	}

	// A client from before v0.2 sends no repository and is told to update.
	_, errGet := c.Objects.GetObjects(ctx, connect.NewRequest(&vxv1.GetObjectsRequest{Hashes: []string{h1.String()}}))
	_, errMiss := c.Objects.MissingObjects(ctx, connect.NewRequest(&vxv1.MissingObjectsRequest{Hashes: []string{h1.String()}}))
	_, errPut := c.Objects.PutObjects(ctx, connect.NewRequest(&vxv1.PutObjectsRequest{Objects: []*vxv1.Object{{Kind: "blob", Data: []byte("x")}}}))
	for _, err := range []error{errGet, errMiss, errPut} {
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(client.Message(err), "this vx is too old for the server: update vx") {
			t.Fatalf("old-style request = %v, want the update message", err)
		}
	}
}
