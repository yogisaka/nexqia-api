// cmd/api/main.go
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/yogisaka/nexqia-api/internal/cache"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/server"
)

func main() {
	_ = godotenv.Load() // dev: baca .env kalau ada; produksi pakai env asli dari compose/systemd, file gak ada = no-op

	cfg := config.Load()

	pool, err := pgxpool.New(context.Background(), cfg.AppRuntimeDSN())
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	redisClient := cache.NewRedisClient(cfg)
	defer func() { _ = redisClient.Close() }()

	router := server.NewRouter(pool)
	if err := router.Run(":" + cfg.HTTPPort); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}
