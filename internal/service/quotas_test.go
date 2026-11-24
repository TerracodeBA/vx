package service_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/metrics"
	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func quota(n int64) *int64 { return &n }

// TestOrgQuotas: site defaults, overrides per organization (superadmins
// only), and refusals that say what is used and what the limit is.
func TestOrgQuotas(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	svc.Quotas.Defaults = service.Quota{Repos: 2, OpenEfforts: 1, PushBytes: 1000}
	root := e.siteAdmin("root")
	if _, err := svc.CreateOrg(ctx, e.alice, "acme", ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"acme/one", "acme/two"} {
		if _, err := svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	before := metrics.QuotaRefused.Value("repos")
	_, err := svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/three"})
	wantErr(t, err, service.CodeTooMany, "organization acme has 2 of its 2 repositories (quota)", "vx-server quota acme")
	if metrics.QuotaRefused.Value("repos") != before+1 {
		t.Error("refusal not counted in vx_quota_refused_total")
	}

	// Open efforts.
	if _, err := svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/one", Name: "first"}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/two", Name: "second"})
	wantErr(t, err, service.CodeTooMany, "organization acme has 1 of its 1 open efforts (quota)")

	// One upload.
	_, err = svc.PutObjects(ctx, e.alice, "acme/one", []service.Object{{Kind: hash.KindBlob, Data: bytes.Repeat([]byte("x"), 1500)}})
	wantErr(t, err, service.CodeTooMany, "this upload is 1.5 KB and one upload to organization acme may be at most 1000 B (quota)")
	if _, err := svc.PutObjects(ctx, e.alice, "acme/one", []service.Object{{Kind: hash.KindBlob, Data: []byte("small")}}); err != nil {
		t.Fatalf("small upload: %v", err)
	}

	// Only superadmins change quotas; an owner is refused.
	_, err = svc.SetOrgQuota(ctx, e.alice, "acme", store.OrgQuota{Repos: quota(10)})
	wantErr(t, err, service.CodePermission, "only superadmins change quotas")
	u, err := svc.SetOrgQuota(ctx, root, "acme", store.OrgQuota{Repos: quota(3), OpenEfforts: quota(0)})
	if err != nil {
		t.Fatal(err)
	}
	if u.Limits.Repos != 3 || u.Limits.OpenEfforts != 0 || u.Limits.PushBytes != 1000 || u.Repos != 2 || u.OpenEfforts != 1 {
		t.Fatalf("usage after override = %+v", u)
	}
	if _, err := svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/three"}); err != nil {
		t.Fatalf("after raising the quota: %v", err)
	}
	if _, err := svc.CreateEffort(ctx, e.alice, service.CreateEffortInput{Repo: "acme/two", Name: "second"}); err != nil {
		t.Fatalf("open efforts unlimited (0) for acme: %v", err)
	}

	// Members see the quotas on the org page; people outside do not.
	u, err = svc.OrgQuota(ctx, e.alice, "acme")
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]service.QuotaRow{}
	for _, r := range u.Rows() {
		rows[r.Name] = r
	}
	if r := rows["repos"]; r.Used != "3" || r.Limit != "3" || !r.Over || r.Default {
		t.Errorf("repos row = %+v", r)
	}
	if r := rows["open_efforts"]; r.Limit != "no limit" || r.Default {
		t.Errorf("open efforts row = %+v", r)
	}
	if r := rows["push"]; r.Limit != "1000 B" || !r.Default || r.Used != "" {
		t.Errorf("push row = %+v", r)
	}
	// Stored bytes come from object_sizes (r3-storage); these repositories
	// have no objects yet.
	if r := rows["stored"]; r.Used != "0 B" || r.Over {
		t.Errorf("stored row = %+v", r)
	}
	if _, err := svc.OrgQuota(ctx, e.person("mallory"), "acme"); code(err) != service.CodeNotFound {
		t.Errorf("outsider sees quotas: %v", err)
	}

	// Back to the defaults, from the console; the change is op-logged.
	if _, err := svc.ConsoleSetOrgQuota(ctx, "acme", store.OrgQuota{}); err != nil {
		t.Fatal(err)
	}
	if got := opsOf(t, svc, root.ID, "org.quota"); len(got) != 1 {
		t.Errorf("org.quota ops by root = %v", got)
	}
	_, err = svc.SetOrgQuota(ctx, root, "acme", store.OrgQuota{Repos: quota(-1)})
	wantErr(t, err, service.CodeInvalid, "cannot be negative")
}

// fakeSizer is an upstream that knows repository sizes and fails the
// test if anything is downloaded.
type fakeSizer struct {
	t    *testing.T
	size int64
}

func (f *fakeSizer) DefaultBranch(context.Context, store.Repo) (string, error) { return "main", nil }
func (f *fakeSizer) SyncBranch(context.Context, store.Repo, string) (store.Branch, int, error) {
	f.t.Fatal("the import downloaded although the quota refuses it")
	return store.Branch{}, 0, nil
}
func (f *fakeSizer) RepoSize(context.Context, store.Repo) (int64, bool, error) {
	return f.size, true, nil
}

func TestImportQuotaChecksHostSizeFirst(t *testing.T) {
	e := newEnv(t)
	e.svc.Upstream = &fakeSizer{t: t, size: 3 << 30}
	e.svc.Quotas.Defaults = service.Quota{ImportBytes: 1 << 30}
	_, err := e.svc.CreateRepo(e.ctx, e.alice, service.CreateRepoInput{Name: "alice/big", Provider: store.ProviderGitHub, Remote: "octo/big"})
	wantErr(t, err, service.CodeTooMany, "octo/big is 3 GB on GitHub and organization alice may import at most 1 GB at once (quota): nothing was downloaded")
	if _, err := e.svc.Store.RepoByName(e.ctx, "alice/big"); err == nil {
		t.Error("the refused repository was created")
	}
}

func TestParseFormatBytes(t *testing.T) {
	for in, want := range map[string]int64{"500MB": 500 << 20, "10GB": 10 << 30, "2GiB": 2 << 30, "1048576": 1 << 20, "0": 0, "none": 0, "1.5k": 1536} {
		got, err := service.ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"lots", "-1GB", "5XB"} {
		if _, err := service.ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) accepted", bad)
		}
	}
	if s := service.FormatBytes(3 << 30); s != "3 GB" {
		t.Errorf("FormatBytes = %q", s)
	}
}

func TestDataDisk(t *testing.T) {
	e := newEnv(t)
	if _, ok := e.svc.DataDisk(); ok {
		t.Fatal("disk reported without a data directory")
	}
	e.svc.Quotas.DataDir = t.TempDir()
	e.svc.Quotas.DiskWarnPercent = 99
	d, ok := e.svc.DataDisk()
	if !ok || d.Total <= 0 || d.Free > d.Total {
		t.Fatalf("DataDisk = %+v, %v", d, ok)
	}
	if d.FreePercent < 99 && !d.Low {
		t.Errorf("%d%% free with a 99%% threshold is not low", d.FreePercent)
	}
}

func TestAllowSignupRate(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.AllowSignup(service.WithClientIP(e.ctx, "203.0.113.7")); err != nil {
		t.Fatalf("no limiter: %v", err)
	}
	now := time.Unix(1000, 0)
	l := ratelimit.New(ratelimit.Config{ratelimit.SignUp: {PerSecond: 1.0 / 60, Burst: 2}})
	l.Now = func() time.Time { return now }
	e.svc.Limits = l
	ctx := service.WithClientIP(e.ctx, "203.0.113.7")
	for i := 0; i < 2; i++ {
		if err := e.svc.AllowSignup(ctx); err != nil {
			t.Fatalf("sign-up %d: %v", i+1, err)
		}
	}
	err := e.svc.AllowSignup(ctx)
	wantErr(t, err, service.CodeTooMany, "slow down: too many new accounts from your address; retry in 60 s")
	if err := e.svc.AllowSignup(service.WithClientIP(e.ctx, "198.51.100.1")); err != nil {
		t.Fatalf("another address: %v", err)
	}
	if !strings.Contains(err.Error(), "slow down") {
		t.Fatal(err)
	}
}
