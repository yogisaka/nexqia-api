// internal/config/config.go
package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
)

type Config struct {
	DBHost             string
	DBPort             string
	DBName             string
	AppRuntimeUser     string
	AppRuntimePassword string
	HTTPPort           string
	RedisHost          string
	RedisPort          string
	RedisPassword      string
	JWTSecret          string

	RateLimitLoginMaxAttempts   int
	RateLimitLoginWindowSeconds int
	RateLimitAPITokensPerMinute int
	RateLimitAPIBurst           int

	Argon2MemoryKiB   uint32
	Argon2Iterations  uint32
	Argon2Parallelism uint8

	PasswordHashMaxConcurrent  int
	PasswordHashQueueTimeoutMS int

	AccessTokenTTLMinutes        int
	RefreshTokenIdleTimeoutHours int
	RefreshTokenAbsoluteTTLDays  int
	CookieSecure                 bool

	// MFASecretEncryptionKey is base64-encoded, decodes to 32 raw bytes (AES-256-GCM
	// key for core.app_user.mfa_secret) — deliberately a dedicated key, separate from
	// any future field-encryption key. See docs/design/specs/2026-09-15-totp-2fa-design.md §6.
	MFASecretEncryptionKey string

	// NIKEncryptionKey is the pgcrypto pgp_sym_encrypt/pgp_sym_decrypt symmetric key
	// for core.person.nik (env NIK_ENCRYPTION_KEY, see .env.example) — must be identical
	// across every environment that needs to decrypt the same data; rotating it makes
	// existing NIK values undecryptable.
	NIKEncryptionKey string

	// AppLockDefaultIdleMinutes/AppLockMaxPinAttempts/AppLockAttemptWindowMinutes —
	// PIN-unlock tunables, see docs/design/specs/2026-09-15-pin-unlock-design.md §10.
	// AppLockDefaultIdleMinutes is only a fallback: a merchant's own idle_minutes
	// (core.feature_flag "auth.pin_lock" flag_value) always wins when set.
	AppLockDefaultIdleMinutes   int
	AppLockMaxPinAttempts       int
	AppLockAttemptWindowMinutes int
}

func Load() Config {
	return Config{
		DBHost:             getEnv("DB_HOST", "localhost"),
		DBPort:             getEnv("DB_PORT", "5432"),
		DBName:             getEnv("DB_NAME", "nexqia"),
		AppRuntimeUser:     getEnv("APP_RUNTIME_USER", "app_runtime"),
		AppRuntimePassword: mustGetEnv("APP_RUNTIME_PASSWORD"),
		HTTPPort:           getEnv("HTTP_PORT", "8080"),
		RedisHost:          getEnv("REDIS_HOST", "localhost"),
		RedisPort:          getEnv("REDIS_PORT", "6379"),
		RedisPassword:      mustGetEnv("REDIS_PASSWORD"),
		JWTSecret:          mustGetEnv("JWT_SECRET"),

		RateLimitLoginMaxAttempts:   getEnvInt("RATE_LIMIT_LOGIN_MAX_ATTEMPTS", 5),
		RateLimitLoginWindowSeconds: getEnvInt("RATE_LIMIT_LOGIN_WINDOW_SECONDS", 900),
		RateLimitAPITokensPerMinute: getEnvInt("RATE_LIMIT_API_TOKENS_PER_MINUTE", 100),
		RateLimitAPIBurst:           getEnvInt("RATE_LIMIT_API_BURST", 20),

		Argon2MemoryKiB:   uint32(getEnvInt("ARGON2_MEMORY_KIB", 19456)),
		Argon2Iterations:  uint32(getEnvInt("ARGON2_ITERATIONS", 2)),
		Argon2Parallelism: uint8(getEnvInt("ARGON2_PARALLELISM", 1)),

		PasswordHashMaxConcurrent:  getEnvInt("PASSWORD_HASH_MAX_CONCURRENT", runtime.NumCPU()),
		PasswordHashQueueTimeoutMS: getEnvInt("PASSWORD_HASH_QUEUE_TIMEOUT_MS", 2000),

		AccessTokenTTLMinutes:        getEnvInt("ACCESS_TOKEN_TTL_MINUTES", 15),
		RefreshTokenIdleTimeoutHours: getEnvInt("REFRESH_TOKEN_IDLE_TIMEOUT_HOURS", 24),
		RefreshTokenAbsoluteTTLDays:  getEnvInt("REFRESH_TOKEN_ABSOLUTE_TTL_DAYS", 30),
		CookieSecure:                 getEnvBool("COOKIE_SECURE", true),

		MFASecretEncryptionKey: mustGetEnv("MFA_SECRET_ENCRYPTION_KEY"),
		NIKEncryptionKey:       mustGetEnv("NIK_ENCRYPTION_KEY"),

		AppLockDefaultIdleMinutes:   getEnvInt("APP_LOCK_DEFAULT_IDLE_MINUTES", 5),
		AppLockMaxPinAttempts:       getEnvInt("APP_LOCK_MAX_PIN_ATTEMPTS", 5),
		AppLockAttemptWindowMinutes: getEnvInt("APP_LOCK_ATTEMPT_WINDOW_MINUTES", 15),
	}
}

func (c Config) AppRuntimeDSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s", c.AppRuntimeUser, c.AppRuntimePassword, c.DBHost, c.DBPort, c.DBName)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required environment variable %s is not set", key))
	}
	return v
}

func getEnvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		panic(fmt.Sprintf("environment variable %s must be a boolean, got %q", key, v))
	}
	return b
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		panic(fmt.Sprintf("environment variable %s must be an integer, got %q", key, v))
	}
	return n
}
