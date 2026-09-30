// internal/config/config.go
package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	// tzdata embeds the IANA zone database so AUDIT_TIMEZONE resolves even in
	// containers without /usr/share/zoneinfo.
	_ "time/tzdata"
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

	RateLimitLoginMaxAttempts      int
	RateLimitLoginWindowSeconds    int
	RateLimitRegisterMaxAttempts   int
	RateLimitRegisterWindowSeconds int
	RateLimitAPITokensPerMinute    int
	RateLimitAPIBurst              int

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

	// NIKSearchKey is the HMAC-SHA256 key for core.person.nik_search_hash (env
	// NIK_SEARCH_KEY, see .env.example) — deliberately separate from
	// NIK_ENCRYPTION_KEY so the two can be rotated independently; used only for
	// exact-match search, never stored or logged as plaintext.
	NIKSearchKey string

	// AppLockDefaultIdleMinutes/AppLockMaxPinAttempts/AppLockAttemptWindowMinutes —
	// PIN-unlock tunables, see docs/design/specs/2026-09-15-pin-unlock-design.md §10.
	// AppLockDefaultIdleMinutes is only a fallback: a merchant's own idle_minutes
	// (core.feature_flag "auth.pin_lock" flag_value) always wins when set.
	AppLockDefaultIdleMinutes   int
	AppLockMaxPinAttempts       int
	AppLockAttemptWindowMinutes int

	// AppLockMinIdleMinutes/AppLockMaxIdleMinutes — hard fence (env
	// APP_LOCK_MIN_IDLE_MINUTES/APP_LOCK_MAX_IDLE_MINUTES) both a merchant's
	// idle_minutes (PUT /settings/security) and AppLockDefaultIdleMinutes must
	// fall inside; validated fail-fast in Load(), see
	// docs/design/specs/2026-09-29-merchant-security-settings-design.md §3.
	AppLockMinIdleMinutes int
	AppLockMaxIdleMinutes int

	// PlatformMFAGraceDays/TenantMFAGraceDays — grace period (days) after a user
	// becomes MFA-mandatory before login is hard-blocked, see
	// docs/design/specs/2026-09-25-mfa-grace-enforcement-design.md §6.
	PlatformMFAGraceDays int
	TenantMFAGraceDays   int

	// TrustedProxies — IP/CIDR addresses gin may trust as X-Forwarded-For sources
	// (router.SetTrustedProxies). Gin's default trusts every proxy, so a client
	// could spoof c.ClientIP() (rate-limit bypass, untrustworthy access_log IPs).
	// Default = the private ranges: deployment topology is nginx on the host →
	// nginx container → API on the Docker network (API port is never published),
	// so X-Forwarded-For only ever arrives from those addresses. Env
	// TRUSTED_PROXIES (comma-separated); the single value "none" trusts nobody.
	TrustedProxies []string

	// AuditExportMaxRows caps one audit CSV export (env AUDIT_EXPORT_MAX_ROWS);
	// a filtered result wider than this is refused with 400 so a broad filter
	// cannot stream the whole log table. See
	// docs/design/specs/2026-09-29-audit-log-ui-design.md §4.5.
	AuditExportMaxRows int

	// AuditLocation is the timezone audit CSV timestamps render in (env
	// AUDIT_TIMEZONE, IANA name — default Asia/Jakarta).
	AuditLocation *time.Location

	// AuditWorkHourStart/AuditWorkHourEnd (env AUDIT_WORK_HOUR_START/END) —
	// working-hours fence feeding the after_hours review flag: access rows
	// before start or at/after end (local hour in AuditLocation) count as
	// after-hours. See docs/design/specs/2026-09-29-access-review.md §6.
	AuditWorkHourStart int
	AuditWorkHourEnd   int

	// AuditReviewDeniedThreshold (env AUDIT_REVIEW_DENIED_THRESHOLD) — the
	// number of 403s at which a user gets the denied review flag.
	AuditReviewDeniedThreshold int

	// AuditReviewVolumeMultiplier (env AUDIT_REVIEW_VOLUME_MULTIPLIER) — a
	// user is volume-flagged when their views exceed this multiplier × the
	// period's median views (only meaningful when the median > 0).
	AuditReviewVolumeMultiplier float64
}

func Load() Config {
	cfg := Config{
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

		RateLimitLoginMaxAttempts:      getEnvInt("RATE_LIMIT_LOGIN_MAX_ATTEMPTS", 5),
		RateLimitLoginWindowSeconds:    getEnvInt("RATE_LIMIT_LOGIN_WINDOW_SECONDS", 900),
		RateLimitRegisterMaxAttempts:   getEnvInt("RATE_LIMIT_REGISTER_MAX_ATTEMPTS", 3),
		RateLimitRegisterWindowSeconds: getEnvInt("RATE_LIMIT_REGISTER_WINDOW_SECONDS", 3600),
		RateLimitAPITokensPerMinute:    getEnvInt("RATE_LIMIT_API_TOKENS_PER_MINUTE", 100),
		RateLimitAPIBurst:              getEnvInt("RATE_LIMIT_API_BURST", 20),

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
		NIKSearchKey:           mustGetEnv("NIK_SEARCH_KEY"),

		AppLockDefaultIdleMinutes:   getEnvInt("APP_LOCK_DEFAULT_IDLE_MINUTES", 5),
		AppLockMaxPinAttempts:       getEnvInt("APP_LOCK_MAX_PIN_ATTEMPTS", 5),
		AppLockAttemptWindowMinutes: getEnvInt("APP_LOCK_ATTEMPT_WINDOW_MINUTES", 15),

		AppLockMinIdleMinutes: getEnvInt("APP_LOCK_MIN_IDLE_MINUTES", 1),
		AppLockMaxIdleMinutes: getEnvInt("APP_LOCK_MAX_IDLE_MINUTES", 30),

		PlatformMFAGraceDays: getEnvInt("PLATFORM_MFA_GRACE_DAYS", 7),
		TenantMFAGraceDays:   getEnvInt("TENANT_MFA_GRACE_DAYS", 7),

		TrustedProxies: getTrustedProxies(),

		AuditExportMaxRows: getEnvInt("AUDIT_EXPORT_MAX_ROWS", 5000),
		AuditLocation:      mustLoadLocation(getEnv("AUDIT_TIMEZONE", "Asia/Jakarta")),

		AuditWorkHourStart:          getEnvInt("AUDIT_WORK_HOUR_START", 7),
		AuditWorkHourEnd:            getEnvInt("AUDIT_WORK_HOUR_END", 21),
		AuditReviewDeniedThreshold:  getEnvInt("AUDIT_REVIEW_DENIED_THRESHOLD", 5),
		AuditReviewVolumeMultiplier: getEnvFloat64("AUDIT_REVIEW_VOLUME_MULTIPLIER", 3),
	}
	// Fail fast at startup: an idle fence the API would have to half-enforce is
	// worse than refusing to boot (spec 2026-09-29-merchant-security-settings §3).
	if err := validateAppLockIdle(cfg.AppLockMinIdleMinutes, cfg.AppLockMaxIdleMinutes, cfg.AppLockDefaultIdleMinutes); err != nil {
		panic(err)
	}
	if cfg.AuditExportMaxRows <= 0 {
		panic(fmt.Sprintf("AUDIT_EXPORT_MAX_ROWS must be > 0, got %d", cfg.AuditExportMaxRows))
	}
	if err := validateAuditReview(cfg.AuditWorkHourStart, cfg.AuditWorkHourEnd, cfg.AuditReviewDeniedThreshold, cfg.AuditReviewVolumeMultiplier); err != nil {
		panic(err)
	}
	return cfg
}

// validateAuditReview checks the access-review flag tunables (spec
// 2026-09-29-access-review §6): 0 <= start < end <= 24 working hours, a denied
// threshold of at least 1 and a strictly positive volume multiplier.
func validateAuditReview(start, end, threshold int, multiplier float64) error {
	if start < 0 || start >= end || end > 24 {
		return fmt.Errorf("AUDIT_WORK_HOUR_START/AUDIT_WORK_HOUR_END invalid: need 0 <= start < end <= 24, got %d..%d", start, end)
	}
	if threshold < 1 {
		return fmt.Errorf("AUDIT_REVIEW_DENIED_THRESHOLD must be >= 1, got %d", threshold)
	}
	if multiplier <= 0 {
		return fmt.Errorf("AUDIT_REVIEW_VOLUME_MULTIPLIER must be > 0, got %v", multiplier)
	}
	return nil
}

// mustLoadLocation parses an IANA timezone name; a bad AUDIT_TIMEZONE refuses
// to boot rather than exporting audit timestamps in the wrong zone.
func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("AUDIT_TIMEZONE %q is not a valid IANA timezone: %v", name, err))
	}
	return loc
}

// validateAppLockIdle checks the idle-window fence: min must be at least 1 and
// not exceed max, and the default must fall inside [min, max] — otherwise a
// merchant admin could save an idle_minutes value AppLockMiddleware would never
// honor consistently.
func validateAppLockIdle(min, max, def int) error {
	if min < 1 || min > max {
		return fmt.Errorf("APP_LOCK_MIN_IDLE_MINUTES/APP_LOCK_MAX_IDLE_MINUTES invalid")
	}
	if def < min || def > max {
		return fmt.Errorf("APP_LOCK_DEFAULT_IDLE_MINUTES must be between APP_LOCK_MIN_IDLE_MINUTES and APP_LOCK_MAX_IDLE_MINUTES")
	}
	return nil
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

// getEnvFloat64 parses a float env var; a non-numeric value refuses to boot
// (same fail-fast contract as getEnvInt).
func getEnvFloat64(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		panic(fmt.Sprintf("environment variable %s must be a number, got %q", key, v))
	}
	return f
}

// getTrustedProxies parses TRUSTED_PROXIES: comma-separated IP/CIDR, items
// trimmed, empties dropped. Unset/empty → the private ranges (nginx topology,
// see TrustedProxies doc comment); the single value "none" → empty slice.
func getTrustedProxies() []string {
	v := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES"))
	if v == "" {
		return []string{"127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	}
	if v == "none" {
		return []string{}
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
