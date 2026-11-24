package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/TerracodeBA/vx/internal/blob/s3test"
	"github.com/TerracodeBA/vx/internal/server"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// Two vx-server processes share one database and one bucket, as replicas
// behind a load balancer do. Agents on both land into one effort at the
// same time, editing their own files and different lines of a shared one;
// every change arrives exactly once and both servers serve the result.
func TestReplicasShareDatabaseAndBucket(t *testing.T) {
	ctx := context.Background()
	st, dbURL := storetest.NewWithURL(t)
	bucket := s3test.New(t)
	t.Setenv("VX_BLOB_ACCESS_KEY_ID", bucket.Creds.AccessKeyID)
	t.Setenv("VX_BLOB_SECRET_ACCESS_KEY", bucket.Creds.SecretAccessKey)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var urls []string
	var srvs []*server.Server
	var dataDirs []string
	for range 2 {
		dir := t.TempDir()
		srv, err := server.New(ctx, server.Config{DatabaseURL: dbURL, DataDir: dir, BlobURL: bucket.BucketURL("vx"), Logger: quiet})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
		hs := httptest.NewServer(srv.Handler)
		t.Cleanup(hs.Close)
		urls, srvs, dataDirs = append(urls, hs.URL), append(srvs, srv), append(dataDirs, dir)
	}
	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateToken(ctx, alice.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, server: urls[0], home: t.TempDir(), st: st}
	work := t.TempDir()
	a := h.user("alice", tok, work)
	a.must("repo", "create", "shop")
	a.must("effort", "new", "Replicas", "--repo", "shop")
	a.must("clone", "shop", "replicas", "seed")
	a.dir = filepath.Join(work, "seed")
	const agents = 6
	var lines []string
	for i := range agents {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	shared := strings.Join(lines, "\n\n\n") + "\n"
	a.must("new", "Shared file")
	a.write("shared.txt", shared)
	a.must("land")

	// Agents alternate between the two servers.
	tokenOf := func(name string) string {
		var resp struct{ Token string }
		if err := json.Unmarshal([]byte(a.must("agent", "add", name, "--json")), &resp); err != nil || resp.Token == "" {
			t.Fatalf("agent add %s: %v", name, err)
		}
		return resp.Token
	}
	var us []*user
	for i := range agents {
		name := fmt.Sprintf("agent%d", i)
		u := h.user(name, tokenOf(name), work)
		u.env["VX_SERVER"] = urls[i%2]
		u.must("clone", "shop", "replicas", name)
		u.dir = filepath.Join(work, name)
		u.must("new", "Edit from "+name)
		u.write(name+".go", "package "+name+"\n")
		u.write("shared.txt", strings.Replace(u.read("shared.txt"), fmt.Sprintf("line %d\n", i), fmt.Sprintf("line %d edited by %s\n", i, name), 1))
		us = append(us, u)
	}
	var wg sync.WaitGroup
	outs := make([]string, agents)
	codes := make([]int, agents)
	for i, u := range us {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], codes[i] = u.run("land")
		}()
	}
	wg.Wait()
	for i := range us {
		if codes[i] != 0 {
			t.Fatalf("agent%d land on server %d exited %d:\n%s", i, i%2, codes[i], outs[i])
		}
	}

	// Both servers serve every change and the merged file.
	for s, url := range urls {
		r := h.user(fmt.Sprintf("reader%d", s), tok, work)
		r.env["VX_SERVER"] = url
		name := fmt.Sprintf("check%d", s)
		r.must("clone", "shop", "replicas", name)
		r.dir = filepath.Join(work, name)
		got := r.read("shared.txt")
		for i := range agents {
			if !strings.Contains(got, fmt.Sprintf("line %d edited by agent%d\n", i, i)) {
				t.Fatalf("server %d: shared.txt misses agent%d's edit:\n%s", s, i, got)
			}
			if r.read(fmt.Sprintf("agent%d.go", i)) != fmt.Sprintf("package agent%d\n", i) {
				t.Fatalf("server %d: agent%d.go missing", s, i)
			}
		}
		log := r.must("log")
		for i := range agents {
			if n := strings.Count(log, fmt.Sprintf("Edit from agent%d", i)); n != 1 {
				t.Fatalf("server %d: agent%d's change appears %d times in the log:\n%s", s, i, n, log)
			}
		}
	}

	// Objects went to the bucket only; the data directories hold none.
	if len(bucket.Keys()) == 0 {
		t.Fatal("no objects in the bucket")
	}
	for _, dir := range dataDirs {
		if _, err := os.Stat(filepath.Join(dir, "objects")); err == nil {
			t.Fatalf("%s has a disk object store although the bucket is configured", dir)
		}
	}

	// A web sign-in code issued by one server is redeemed on the other,
	// once.
	actor, err := srvs[0].Service.ResolveActor(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := srvs[0].Service.CreateLoginCode(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	if p, _, err := srvs[1].Service.RedeemLoginCode(ctx, code); err != nil || p.ID != alice.ID {
		t.Fatalf("redeem on the other server = %v, %v", p.Handle, err)
	}
	if _, _, err := srvs[0].Service.RedeemLoginCode(ctx, code); err == nil {
		t.Fatal("a code redeemed on one server works again on the other")
	}

	// Object sizes for quotas: recorded from the bucket, counted per repo.
	if _, err := srvs[1].Service.SizeAllObjects(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := srvs[0].Service.ObjectSizesPending(ctx); err != nil || n != 0 {
		t.Fatalf("objects without a size = %d, %v", n, err)
	}
	repo, err := st.RepoByName(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.RepoStoredBytes(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	var inBucket int64
	for _, k := range bucket.Keys() {
		b, _ := bucket.Object(k)
		inBucket += int64(len(b))
	}
	if stored <= 0 || stored > inBucket {
		t.Fatalf("repo stores %d bytes, the bucket holds %d", stored, inBucket)
	}
	if org, err := st.OrgStoredBytes(ctx, repo.OrgID); err != nil || org != stored {
		t.Fatalf("org stores %d bytes (%v), its only repo %d", org, err, stored)
	}
}
