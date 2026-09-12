//go:build integration

package cache_test

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/yogisaka/nexqia-api/internal/cache"
	"github.com/yogisaka/nexqia-api/internal/config"
)

func TestNewRedisClient_ConnectsAndPings(t *testing.T) {
	ctx := context.Background()

	container, err := tcredis.Run(ctx, "redis:8-alpine")
	if err != nil {
		t.Fatalf("failed to start redis container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get host: %v", err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("failed to get port: %v", err)
	}

	client := cache.NewRedisClient(config.Config{RedisHost: host, RedisPort: port.Port()})
	defer client.Close()

	if err := client.Ping(ctx).Err(); err != nil {
		t.Errorf("expected successful ping, got error: %v", err)
	}
	_ = testcontainers.SkipIfProviderIsNotHealthy
}
