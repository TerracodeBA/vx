package main

import (
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/server"
	"github.com/TerracodeBA/vx/internal/service"
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
