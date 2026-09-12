// internal/config/config.go
package config

import (
	"fmt"
	"os"
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
