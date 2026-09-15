//go:build integration

package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/yogisaka/nexqia-api/internal/ratelimit"
)

func newTestClient(t *testing.T) *redis.Client {
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
	client := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	t.Cleanup(func() { client.Close() })
	return client
}

func TestAllowTokenBucket_AllowsBurstThenBlocksThenRefills(t *testing.T) {
	client := newTestClient(t)
	limiter := ratelimit.NewLimiter(client)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		result, err := limiter.AllowTokenBucket(ctx, "test:bucket", 3, 60)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result.Allowed {
			t.Fatalf("expected request %d to be allowed (within burst capacity)", i)
		}
	}

	result, err := limiter.AllowTokenBucket(ctx, "test:bucket", 3, 60)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected 4th request to be blocked, burst capacity exhausted")
	}
	if result.RetryAfter <= 0 {
		t.Error("expected positive RetryAfter when blocked")
	}

	// refillPerMinute=60 == 1 token/second; sleep past one refill interval and
	// confirm the bucket actually allows again (this is the "refills" half of
	// the test name).
	time.Sleep(1100 * time.Millisecond)
	result, err = limiter.AllowTokenBucket(ctx, "test:bucket", 3, 60)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected request to be allowed after refill interval elapsed")
	}
}

func TestAllowSlidingWindow_BlocksAfterMaxAttemptsWithinWindow(t *testing.T) {
	client := newTestClient(t)
	limiter := ratelimit.NewLimiter(client)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		result, err := limiter.AllowSlidingWindow(ctx, "test:login:alice", 5, 900)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result.Allowed {
			t.Fatalf("expected attempt %d to be allowed (within max attempts)", i)
		}
	}

	result, err := limiter.AllowSlidingWindow(ctx, "test:login:alice", 5, 900)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected 6th attempt to be blocked, max attempts reached")
	}
}

func TestAllowTokenBucket_ConcurrentRequestsNeverExceedCapacity(t *testing.T) {
	client := newTestClient(t)
	limiter := ratelimit.NewLimiter(client)
	ctx := context.Background()

	const capacity = 10
	const attempts = 50
	allowedCount := 0
	results := make(chan bool, attempts)

	for i := 0; i < attempts; i++ {
		go func() {
			result, err := limiter.AllowTokenBucket(ctx, "test:concurrent", capacity, 0)
			if err != nil {
				results <- false
				return
			}
			results <- result.Allowed
		}()
	}

	for i := 0; i < attempts; i++ {
		if <-results {
			allowedCount++
		}
	}

	if allowedCount != capacity {
		t.Errorf("expected exactly %d allowed under concurrent load (atomic script), got %d", capacity, allowedCount)
	}
	_ = testcontainers.SkipIfProviderIsNotHealthy
}
