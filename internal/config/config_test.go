// internal/config/config_test.go
package config

import (
	"os"
	"testing"
)

func TestLoad_UsesEnvOverridesAndDefaults(t *testing.T) {
	_ = os.Setenv("DB_HOST", "db.internal")
	_ = os.Setenv("APP_RUNTIME_PASSWORD", "secret123")
	defer func() { _ = os.Unsetenv("DB_HOST") }()
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()

	cfg := Load()

	if cfg.DBHost != "db.internal" {
		t.Errorf("expected DBHost from env, got %q", cfg.DBHost)
	}
	if cfg.DBPort != "5432" {
		t.Errorf("expected default DBPort 5432, got %q", cfg.DBPort)
	}
	if cfg.AppRuntimeUser != "app_runtime" {
		t.Errorf("expected default AppRuntimeUser app_runtime, got %q", cfg.AppRuntimeUser)
	}
	if cfg.AppRuntimePassword != "secret123" {
		t.Errorf("expected AppRuntimePassword from env, got %q", cfg.AppRuntimePassword)
	}
}

func TestLoad_ReadsRedisConfig(t *testing.T) {
	_ = os.Setenv("APP_RUNTIME_PASSWORD", "secret123")
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()

	cfg := Load()

	if cfg.RedisHost != "localhost" {
		t.Errorf("expected default RedisHost localhost, got %q", cfg.RedisHost)
	}
	if cfg.RedisPort != "6379" {
		t.Errorf("expected default RedisPort 6379, got %q", cfg.RedisPort)
	}
}

func TestAppRuntimeDSN_FormatsPostgresURL(t *testing.T) {
	cfg := Config{
		DBHost:             "localhost",
		DBPort:             "5432",
		DBName:             "nexqia",
		AppRuntimeUser:     "app_runtime",
		AppRuntimePassword: "pw",
	}
	got := cfg.AppRuntimeDSN()
	want := "postgres://app_runtime:pw@localhost:5432/nexqia"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
