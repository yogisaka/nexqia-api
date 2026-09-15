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

	Argon2MemoryKiB    uint32
	Argon2Iterations   uint32
	Argon2Parallelism  uint8

	PasswordHashMaxConcurrent  int
	PasswordHashQueueTimeoutMS int
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
