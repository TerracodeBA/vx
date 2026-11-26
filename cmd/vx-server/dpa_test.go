package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/server"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// TestAgreementConfig: the data processing agreement step is off unless
// VX_REQUIRE_DPA turns it on; on, it needs a valid VX_DPA_DIR, and the
// server refuses to start without a version in effect.
func TestAgreementConfig(t *testing.T) {
	read := func(env map[string]string) (server.Config, error) {
		var cfg server.Config
		err := applyOps(&cfg, func(k string) string { return env[k] })
		return cfg, err
	}
	// Off by default: even a broken directory is not read.
	cfg, err := read(map[string]string{"VX_DPA_DIR": "/nonexistent"})
	if err != nil || cfg.Agreement.Required || len(cfg.Agreement.Versions) != 0 {
		t.Fatalf("off: %+v, %v", cfg.Agreement, err)
	}
	if _, err := read(map[string]string{"VX_REQUIRE_DPA": "1"}); err == nil || !strings.Contains(err.Error(), "VX_DPA_DIR is not set") {
		t.Errorf("on without a directory: %v", err)
	}
	if _, err := read(map[string]string{"VX_REQUIRE_DPA": "maybe"}); err == nil || !strings.Contains(err.Error(), "VX_REQUIRE_DPA") {
		t.Errorf("VX_REQUIRE_DPA=maybe: %v", err)
	}
	dir := t.TempDir()
	if _, err := read(map[string]string{"VX_REQUIRE_DPA": "1", "VX_DPA_DIR": dir}); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("on with an empty directory: %v", err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("v1.md", "Test agreement, version 1\n")
	write("manifest.json", `{"versions":[{"id":"v1","file":"v1.md","published":"2099-01-01","effective":"2099-01-01"}]}`)
	cfg, err = read(map[string]string{"VX_REQUIRE_DPA": "true", "VX_DPA_DIR": dir})
	if err != nil || !cfg.Agreement.Required || len(cfg.Agreement.Versions) != 1 {
		t.Fatalf("on: %+v, %v", cfg.Agreement, err)
	}
	// Valid, but nothing in effect yet: the server refuses to start.
	cfg.DatabaseURL, cfg.DataDir = storetest.NewDatabase(t), t.TempDir()
	if srv, err := server.New(context.Background(), cfg); err == nil {
		srv.Close()
		t.Fatal("the server started without a version in effect")
	} else if !strings.Contains(err.Error(), "no version of the data processing agreement is in effect yet") {
		t.Errorf("start: %v", err)
	}
	write("manifest.json", `{"versions":[{"id":"v1","file":"v1.md","published":"2026-01-01","effective":"2026-01-01"}]}`)
	if cfg.Agreement, err = agreementSettings(func(k string) string {
		return map[string]string{"VX_REQUIRE_DPA": "1", "VX_DPA_DIR": dir}[k]
	}); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("start with a version in effect: %v", err)
	}
	srv.Close()
}
