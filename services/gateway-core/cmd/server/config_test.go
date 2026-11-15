package main

import (
	"strings"
	"testing"
)

// 回归 (sfind-5cabf64c): ADMIN_JWT_SECRET 不得回落到内置默认 — 空/过短/
// 公开 dev 字面量均拒绝启动; 字面量仅 ALLOW_DEV_ADMIN_SECRET=true 时放行.
func TestLoadAppConfig_AdminJWTSecret(t *testing.T) {
	base := func() {
		t.Setenv("DATABASE_URL", "postgres://x")
		t.Setenv("CHAIN_CONFIG_JSON", `{"factory":"0x0000000000000000000000000000000000000001","implementation":"0x0000000000000000000000000000000000000002","chains":{"31337":{"chain_id":31337,"rpc_urls":["http://x"]}}}`)
	}
	valid := strings.Repeat("a", 48)

	t.Run("unset -> refuse", func(t *testing.T) {
		base()
		if _, err := loadAppConfig(); err == nil || !strings.Contains(err.Error(), "ADMIN_JWT_SECRET is required") {
			t.Fatalf("want required error, got %v", err)
		}
	})
	t.Run("short -> refuse", func(t *testing.T) {
		base()
		t.Setenv("ADMIN_JWT_SECRET", "too-short")
		if _, err := loadAppConfig(); err == nil || !strings.Contains(err.Error(), "32 bytes") {
			t.Fatalf("want length error, got %v", err)
		}
	})
	t.Run("public dev literal -> refuse", func(t *testing.T) {
		base()
		t.Setenv("ADMIN_JWT_SECRET", devAdminJWTSecret)
		if _, err := loadAppConfig(); err == nil || !strings.Contains(err.Error(), "public dev value") {
			t.Fatalf("want dev-literal error, got %v", err)
		}
	})
	t.Run("dev literal + explicit opt-in -> accept", func(t *testing.T) {
		base()
		t.Setenv("ADMIN_JWT_SECRET", devAdminJWTSecret)
		t.Setenv("ALLOW_DEV_ADMIN_SECRET", "true")
		if _, err := loadAppConfig(); err != nil {
			t.Fatalf("dev opt-in should pass, got %v", err)
		}
	})
	t.Run("real secret -> accept", func(t *testing.T) {
		base()
		t.Setenv("ADMIN_JWT_SECRET", valid)
		cfg, err := loadAppConfig()
		if err != nil {
			t.Fatalf("valid secret rejected: %v", err)
		}
		if string(cfg.AdminJWTKey) != valid {
			t.Fatal("AdminJWTKey mismatch")
		}
	})
}
