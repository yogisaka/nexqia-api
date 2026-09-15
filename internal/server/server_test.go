//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/server"
)

const testJWTSecret = "test-secret"

func testConfig() config.Config {
	return config.Config{
		JWTSecret:                    testJWTSecret,
		RateLimitLoginMaxAttempts:    5,
		RateLimitLoginWindowSeconds:  900,
		RateLimitAPITokensPerMinute:  100,
		RateLimitAPIBurst:            20,
		Argon2MemoryKiB:              19456,
		Argon2Iterations:             2,
		Argon2Parallelism:            1,
		PasswordHashMaxConcurrent:    4,
		PasswordHashQueueTimeoutMS:   2000,
		AccessTokenTTLMinutes:        15,
		RefreshTokenIdleTimeoutHours: 24,
		RefreshTokenAbsoluteTTLDays:  30,
		CookieSecure:                 true,
	}
}

func newTestRedisClient(t *testing.T, ctx context.Context) *redis.Client {
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

func TestTenantMiddleware_IsTransactionScopedAcrossRequests(t *testing.T) {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:18",
		postgres.WithDatabase("nexqia_test"),
		postgres.WithUsername("nexqia"),
		postgres.WithPassword("nexqia"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	defer pool.Close()

	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, "99999999-9999-9999-9999-999999999999", "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "tester", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate test token: %v", err)
	}

	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req1.Header.Set("X-Company-ID", "11111111-1111-1111-1111-111111111111")
	req1.Header.Set("X-Merchant-ID", "22222222-2222-2222-2222-222222222222")
	req1.Header.Set("Authorization", "Bearer "+token)
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("request 1: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var body1 struct {
		Data struct {
			CompanyID  string `json:"company_id"`
			MerchantID string `json:"merchant_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w1.Body.Bytes(), &body1); err != nil {
		t.Fatalf("failed to decode response 1: %v", err)
	}
	if body1.Data.CompanyID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("request 1: expected echoed company_id, got %q", body1.Data.CompanyID)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req2.Header.Set("X-Company-ID", "33333333-3333-3333-3333-333333333333")
	req2.Header.Set("X-Merchant-ID", "44444444-4444-4444-4444-444444444444")
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("request 2: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var body2 struct {
		Data struct {
			CompanyID  string `json:"company_id"`
			MerchantID string `json:"merchant_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("failed to decode response 2: %v", err)
	}
	if body2.Data.CompanyID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("request 2: tenant context leaked from request 1, got %q", body2.Data.CompanyID)
	}
}

func TestTenantMiddleware_RejectsMissingTenantHeaders(t *testing.T) {
	ctx := context.Background()
	pool := &pgxpool.Pool{} // never dialed — middleware must reject before touching the pool
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing tenant headers, got %d", w.Code)
	}
}

func TestRateLimitAPIMiddleware_FailsClosedWhenRedisUnreachable(t *testing.T) {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:18",
		postgres.WithDatabase("nexqia_test"),
		postgres.WithUsername("nexqia"),
		postgres.WithPassword("nexqia"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	defer pool.Close()

	// Unreachable Redis address on purpose — simulates Redis being down.
	unreachableRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer unreachableRedis.Close()

	router := server.NewRouter(pool, unreachableRedis, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, "99999999-9999-9999-9999-999999999999", "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "tester", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate test token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("X-Company-ID", "11111111-1111-1111-1111-111111111111")
	req.Header.Set("X-Merchant-ID", "22222222-2222-2222-2222-222222222222")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when Redis is unreachable (fail-closed), got %d: %s", w.Code, w.Body.String())
	}
}
