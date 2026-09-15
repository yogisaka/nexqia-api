// internal/config/config_test.go
package config

import (
	"os"
	"testing"
)

func TestLoad_UsesEnvOverridesAndDefaults(t *testing.T) {
	_ = os.Setenv("DB_HOST", "db.internal")
	_ = os.Setenv("APP_RUNTIME_PASSWORD", "secret123")
	_ = os.Setenv("REDIS_PASSWORD", "redispw")
	_ = os.Setenv("JWT_SECRET", "test-secret")
	defer func() { _ = os.Unsetenv("DB_HOST") }()
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()
	defer func() { _ = os.Unsetenv("REDIS_PASSWORD") }()
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()

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
	_ = os.Setenv("REDIS_PASSWORD", "redispw")
	_ = os.Setenv("JWT_SECRET", "test-secret")
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()
	defer func() { _ = os.Unsetenv("REDIS_PASSWORD") }()
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()

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

func TestLoad_ReadsRateLimitAndHashingConfig(t *testing.T) {
	_ = os.Setenv("APP_RUNTIME_PASSWORD", "secret123")
	_ = os.Setenv("REDIS_PASSWORD", "redispw")
	_ = os.Setenv("JWT_SECRET", "test-secret")
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()
	defer func() { _ = os.Unsetenv("REDIS_PASSWORD") }()
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()

	cfg := Load()

	if cfg.RateLimitLoginMaxAttempts != 5 {
		t.Errorf("expected default RateLimitLoginMaxAttempts 5, got %d", cfg.RateLimitLoginMaxAttempts)
	}
	if cfg.RateLimitLoginWindowSeconds != 900 {
		t.Errorf("expected default RateLimitLoginWindowSeconds 900, got %d", cfg.RateLimitLoginWindowSeconds)
	}
	if cfg.RateLimitAPITokensPerMinute != 100 {
		t.Errorf("expected default RateLimitAPITokensPerMinute 100, got %d", cfg.RateLimitAPITokensPerMinute)
	}
	if cfg.RateLimitAPIBurst != 20 {
		t.Errorf("expected default RateLimitAPIBurst 20, got %d", cfg.RateLimitAPIBurst)
	}
	if cfg.Argon2MemoryKiB != 19456 {
		t.Errorf("expected default Argon2MemoryKiB 19456, got %d", cfg.Argon2MemoryKiB)
	}
	if cfg.Argon2Iterations != 2 {
		t.Errorf("expected default Argon2Iterations 2, got %d", cfg.Argon2Iterations)
	}
	if cfg.Argon2Parallelism != 1 {
		t.Errorf("expected default Argon2Parallelism 1, got %d", cfg.Argon2Parallelism)
	}
	if cfg.PasswordHashQueueTimeoutMS != 2000 {
		t.Errorf("expected default PasswordHashQueueTimeoutMS 2000, got %d", cfg.PasswordHashQueueTimeoutMS)
	}
}

func TestGetEnvInt_OverridesDefaultAndPanicsOnInvalid(t *testing.T) {
	_ = os.Setenv("RATE_LIMIT_API_BURST", "42")
	defer func() { _ = os.Unsetenv("RATE_LIMIT_API_BURST") }()
	if got := getEnvInt("RATE_LIMIT_API_BURST", 20); got != 42 {
		t.Errorf("expected override 42, got %d", got)
	}

	_ = os.Setenv("RATE_LIMIT_API_BURST", "not-a-number")
	defer func() { _ = os.Unsetenv("RATE_LIMIT_API_BURST") }()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on invalid integer env var, got none")
		}
	}()
	getEnvInt("RATE_LIMIT_API_BURST", 20)
}
