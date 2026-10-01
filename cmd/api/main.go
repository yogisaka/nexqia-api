// cmd/api/main.go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/yogisaka/nexqia-api/internal/cache"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/mail"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// @title NEXQIA API
// @version 1.0
// @description Backend service for the NEXQIA Healthcare Operating Platform. All routes are under /api/v1. Response envelope: {"data": ..., "meta": {...}}.
// @BasePath /api/v1
func main() {
	_ = godotenv.Load() // dev: baca .env kalau ada; produksi pakai env asli dari compose/systemd, file gak ada = no-op

	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.AppRuntimeDSN())
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	redisClient := cache.NewRedisClient(cfg)
	defer func() { _ = redisClient.Close() }()

	// Outbox email worker (internal/mail) lives in the API process and stops with it.
	slog.Info("email worker started", "driver", cfg.MailDriver)
	go mail.RunWorker(ctx, pool, mail.NewSender(cfg), cfg)

	router := server.NewRouter(pool, redisClient, cfg)
	srv := &http.Server{Addr: ":" + cfg.HTTPPort, Handler: router}
	// The signal context replaces Go's default exit on SIGINT/SIGTERM, so the
	// server has to be shut down explicitly once it fires.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown", "error", err)
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}
