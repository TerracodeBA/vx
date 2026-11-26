package main

import (
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/server"
)

func TestOrgDeleteGraceFromEnv(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"":    0, // the default, 30 days
		"30d": 30 * 24 * time.Hour,
		"2w":  14 * 24 * time.Hour,
		"72h": 72 * time.Hour,
		"0":   -1, // no grace period: purged at the next run
		"0d":  -1,
	} {
		var cfg server.Config
		if err := applyOps(&cfg, func(k string) string {
			if k == "VX_ORG_DELETE_GRACE" {
				return v
			}
			return ""
		}); err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if cfg.OrgDeletion.Grace != want {
			t.Errorf("VX_ORG_DELETE_GRACE=%q: grace %v, want %v", v, cfg.OrgDeletion.Grace, want)
		}
	}
	for _, v := range []string{"soon", "-3d", "-1h"} {
		var cfg server.Config
		if err := applyOps(&cfg, func(k string) string {
			if k == "VX_ORG_DELETE_GRACE" {
				return v
			}
			return ""
		}); err == nil {
			t.Errorf("VX_ORG_DELETE_GRACE=%q was accepted", v)
		}
	}
}
