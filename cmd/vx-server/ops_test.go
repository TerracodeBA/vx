package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/server"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

func TestApplyOpsFromEnv(t *testing.T) {
	env := map[string]string{
		"VX_METRICS_TOKEN":      " scrape ",
		"VX_RATE_SIGNIN":        "5/m",
		"VX_QUOTA_REPOS":        "50",
		"VX_QUOTA_STORED":       "20GB",
		"VX_QUOTA_PUSH":         "500MB",
		"VX_QUOTA_OPEN_EFFORTS": "none",
		"VX_DISK_WARN_PERCENT":  "15",
		"VX_AUDIT_RETENTION":    "365d",
	}
	var cfg server.Config
	if err := applyOps(&cfg, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	want := service.Quota{Repos: 50, StoredBytes: 20 << 30, PushBytes: 500 << 20}
	if cfg.MetricsToken != "scrape" || cfg.Quotas.Defaults != want || cfg.Quotas.DiskWarnPercent != 15 ||
		cfg.Audit.Retention != 365*24*time.Hour || cfg.Limits[ratelimit.SignIn].Burst != 5 || cfg.Limits[ratelimit.APIToken] != ratelimit.Defaults()[ratelimit.APIToken] {
		t.Fatalf("cfg = %+v", cfg)
	}
	for k, v := range map[string]string{
		"VX_QUOTA_REPOS":       "many",
		"VX_QUOTA_STORED":      "big",
		"VX_RATE_WEBHOOK":      "fast",
		"VX_AUDIT_RETENTION":   "2d",
		"VX_DISK_WARN_PERCENT": "0",
	} {
		bad := map[string]string{k: v}
		err := applyOps(&server.Config{}, func(k string) string { return bad[k] })
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: err = %v", k, v, err)
		}
	}
}

func TestQuotaCommand(t *testing.T) {
	db := storetest.NewDatabase(t)
	run := func(args ...string) (string, error) {
		cmd := root()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--db", db}, args...))
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	if _, err := run("user-add", "alice", "--org", "acme"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VX_QUOTA_REPOS", "10")
	out, err := run("quota", "acme")
	if err != nil || !strings.Contains(out, "Quotas of acme") || !strings.Contains(out, "Repositories   0") || strings.Contains(out, "set for this organization") {
		t.Fatalf("quota acme = %v\n%s", err, out)
	}
	out, err = run("quota", "acme", "--repos", "50", "--stored", "20GB", "--push", "0")
	if err != nil || !strings.Contains(out, "Saved the quotas of acme.") || !strings.Contains(out, "20 GB") || !strings.Contains(out, "50  (set for this organization)") {
		t.Fatalf("quota set = %v\n%s", err, out)
	}
	out, err = run("quota", "acme", "--repos", "default")
	if err != nil || !strings.Contains(out, "10\n") {
		t.Fatalf("quota default = %v\n%s", err, out)
	}
	if _, err := run("quota", "acme", "--stored", "huge"); err == nil || !strings.Contains(err.Error(), "--stored") {
		t.Fatalf("bad size: %v", err)
	}
	if _, err := run("quota", "nope"); err == nil {
		t.Fatal("unknown organization accepted")
	}
}
