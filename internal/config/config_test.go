// internal/config/config_test.go
package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoad_UsesEnvOverridesAndDefaults(t *testing.T) {
	_ = os.Setenv("DB_HOST", "db.internal")
	_ = os.Setenv("APP_RUNTIME_PASSWORD", "secret123")
	_ = os.Setenv("REDIS_PASSWORD", "redispw")
	_ = os.Setenv("JWT_SECRET", "test-secret")
	_ = os.Setenv("MFA_SECRET_ENCRYPTION_KEY", "test-mfa-key")
	_ = os.Setenv("NIK_SEARCH_KEY", "test-nik-search-key")
	_ = os.Setenv("NIK_ENCRYPTION_KEY", "test-nik-key")
	defer func() { _ = os.Unsetenv("DB_HOST") }()
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()
	defer func() { _ = os.Unsetenv("REDIS_PASSWORD") }()
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()
	defer func() { _ = os.Unsetenv("MFA_SECRET_ENCRYPTION_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_SEARCH_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_ENCRYPTION_KEY") }()

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
	_ = os.Setenv("MFA_SECRET_ENCRYPTION_KEY", "test-mfa-key")
	_ = os.Setenv("NIK_SEARCH_KEY", "test-nik-search-key")
	_ = os.Setenv("NIK_ENCRYPTION_KEY", "test-nik-key")
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()
	defer func() { _ = os.Unsetenv("REDIS_PASSWORD") }()
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()
	defer func() { _ = os.Unsetenv("MFA_SECRET_ENCRYPTION_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_SEARCH_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_ENCRYPTION_KEY") }()

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
	_ = os.Setenv("MFA_SECRET_ENCRYPTION_KEY", "test-mfa-key")
	_ = os.Setenv("NIK_SEARCH_KEY", "test-nik-search-key")
	_ = os.Setenv("NIK_ENCRYPTION_KEY", "test-nik-key")
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()
	defer func() { _ = os.Unsetenv("REDIS_PASSWORD") }()
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()
	defer func() { _ = os.Unsetenv("MFA_SECRET_ENCRYPTION_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_SEARCH_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_ENCRYPTION_KEY") }()

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

func TestLoad_ReadsSessionConfig(t *testing.T) {
	_ = os.Setenv("APP_RUNTIME_PASSWORD", "secret123")
	_ = os.Setenv("REDIS_PASSWORD", "redispw")
	_ = os.Setenv("JWT_SECRET", "test-secret")
	_ = os.Setenv("MFA_SECRET_ENCRYPTION_KEY", "test-mfa-key")
	_ = os.Setenv("NIK_SEARCH_KEY", "test-nik-search-key")
	_ = os.Setenv("NIK_ENCRYPTION_KEY", "test-nik-key")
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()
	defer func() { _ = os.Unsetenv("REDIS_PASSWORD") }()
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()
	defer func() { _ = os.Unsetenv("MFA_SECRET_ENCRYPTION_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_SEARCH_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_ENCRYPTION_KEY") }()

	cfg := Load()

	if cfg.AccessTokenTTLMinutes != 15 {
		t.Errorf("expected default AccessTokenTTLMinutes 15, got %d", cfg.AccessTokenTTLMinutes)
	}
	if cfg.RefreshTokenIdleTimeoutHours != 24 {
		t.Errorf("expected default RefreshTokenIdleTimeoutHours 24, got %d", cfg.RefreshTokenIdleTimeoutHours)
	}
	if cfg.RefreshTokenAbsoluteTTLDays != 30 {
		t.Errorf("expected default RefreshTokenAbsoluteTTLDays 30, got %d", cfg.RefreshTokenAbsoluteTTLDays)
	}
	if !cfg.CookieSecure {
		t.Error("expected default CookieSecure true")
	}
	if cfg.MFASecretEncryptionKey != "test-mfa-key" {
		t.Errorf("expected MFASecretEncryptionKey from env, got %q", cfg.MFASecretEncryptionKey)
	}
}

func TestLoad_ReadsAppLockConfig(t *testing.T) {
	_ = os.Setenv("APP_RUNTIME_PASSWORD", "secret123")
	_ = os.Setenv("REDIS_PASSWORD", "redispw")
	_ = os.Setenv("JWT_SECRET", "test-secret")
	_ = os.Setenv("MFA_SECRET_ENCRYPTION_KEY", "test-mfa-key")
	_ = os.Setenv("NIK_SEARCH_KEY", "test-nik-search-key")
	_ = os.Setenv("NIK_ENCRYPTION_KEY", "test-nik-key")
	defer func() { _ = os.Unsetenv("APP_RUNTIME_PASSWORD") }()
	defer func() { _ = os.Unsetenv("REDIS_PASSWORD") }()
	defer func() { _ = os.Unsetenv("JWT_SECRET") }()
	defer func() { _ = os.Unsetenv("MFA_SECRET_ENCRYPTION_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_SEARCH_KEY") }()
	defer func() { _ = os.Unsetenv("NIK_ENCRYPTION_KEY") }()

	cfg := Load()

	if cfg.AppLockDefaultIdleMinutes != 5 {
		t.Errorf("expected default AppLockDefaultIdleMinutes 5, got %d", cfg.AppLockDefaultIdleMinutes)
	}
	if cfg.AppLockMinIdleMinutes != 1 {
		t.Errorf("expected default AppLockMinIdleMinutes 1, got %d", cfg.AppLockMinIdleMinutes)
	}
	if cfg.AppLockMaxIdleMinutes != 30 {
		t.Errorf("expected default AppLockMaxIdleMinutes 30, got %d", cfg.AppLockMaxIdleMinutes)
	}
	if cfg.AppLockMaxPinAttempts != 5 {
		t.Errorf("expected default AppLockMaxPinAttempts 5, got %d", cfg.AppLockMaxPinAttempts)
	}
	if cfg.AppLockAttemptWindowMinutes != 15 {
		t.Errorf("expected default AppLockAttemptWindowMinutes 15, got %d", cfg.AppLockAttemptWindowMinutes)
	}
}

// TestValidateAppLockIdle — the idle-window fence (spec
// 2026-09-29-merchant-security-settings §3): a valid range passes; min below 1
// or above max, and a default outside [min,max], each fail naming their env.
func TestValidateAppLockIdle(t *testing.T) {
	if err := validateAppLockIdle(1, 30, 5); err != nil {
		t.Errorf("expected valid range 1..30 with default 5 to pass, got %v", err)
	}
	if err := validateAppLockIdle(1, 1, 1); err != nil {
		t.Errorf("expected min == max == default to pass, got %v", err)
	}
	err := validateAppLockIdle(0, 30, 5)
	if err == nil || !strings.Contains(err.Error(), "APP_LOCK_MIN_IDLE_MINUTES") {
		t.Errorf("expected min 0 to fail naming APP_LOCK_MIN_IDLE_MINUTES, got %v", err)
	}
	err = validateAppLockIdle(31, 30, 5)
	if err == nil || !strings.Contains(err.Error(), "APP_LOCK_MIN_IDLE_MINUTES") {
		t.Errorf("expected min > max to fail naming APP_LOCK_MIN_IDLE_MINUTES, got %v", err)
	}
	err = validateAppLockIdle(1, 30, 0)
	if err == nil || !strings.Contains(err.Error(), "APP_LOCK_DEFAULT_IDLE_MINUTES") {
		t.Errorf("expected default below range to fail naming APP_LOCK_DEFAULT_IDLE_MINUTES, got %v", err)
	}
	err = validateAppLockIdle(1, 30, 31)
	if err == nil || !strings.Contains(err.Error(), "APP_LOCK_DEFAULT_IDLE_MINUTES") {
		t.Errorf("expected default above range to fail naming APP_LOCK_DEFAULT_IDLE_MINUTES, got %v", err)
	}
}

func TestGetEnvBool_OverridesDefaultAndPanicsOnInvalid(t *testing.T) {
	_ = os.Setenv("COOKIE_SECURE", "false")
	defer func() { _ = os.Unsetenv("COOKIE_SECURE") }()
	if got := getEnvBool("COOKIE_SECURE", true); got != false {
		t.Errorf("expected override false, got %v", got)
	}
	_ = os.Unsetenv("COOKIE_SECURE")

	_ = os.Setenv("COOKIE_SECURE", "not-a-bool")
	defer func() { _ = os.Unsetenv("COOKIE_SECURE") }()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on invalid boolean env var, got none")
		}
	}()
	getEnvBool("COOKIE_SECURE", true)
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
