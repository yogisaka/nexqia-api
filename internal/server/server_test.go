//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
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
		RateLimitLoginWindowSeconds:     900,
		RateLimitRegisterMaxAttempts:    5,
		RateLimitRegisterWindowSeconds:  900,
		RateLimitAPITokensPerMinute:     100,
		RateLimitAPIBurst:            20,
		Argon2MemoryKiB:              19456,
		Argon2Iterations:             2,
		Argon2Parallelism:            1,
		PasswordHashMaxConcurrent:    4,
		PasswordHashQueueTimeoutMS:   2000,
		MFASecretEncryptionKey:       "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=",
		AccessTokenTTLMinutes:        15,
		RefreshTokenIdleTimeoutHours: 24,
		RefreshTokenAbsoluteTTLDays:  30,
		CookieSecure:                 true,
		AppLockDefaultIdleMinutes:    5,
		AppLockMaxPinAttempts:        3,
		AppLockAttemptWindowMinutes:  15,
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

// newTestPostgresPool starts a fully migrated Postgres container (the prebuilt
// nexqia-api-postgres-test image, same one internal/db/migrations_test.go uses) and
// returns a pool connected to it. Tests that only need RLS/tenant-context plumbing
// (no real tables) can ignore the schema; pin-unlock tests rely on it directly.
func newTestPostgresPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	container, err := postgres.Run(ctx, "nexqia-api-postgres-test:latest",
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
	m, err := migrate.New("file://../../migrations", dsn)
	if err != nil {
		t.Fatalf("failed to init migrate: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("failed to apply migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// setupPinLockTestUser seeds a company, merchant, role, and one app_user (with the
// given pin, or none) directly via SQL, and sets the auth.pin_lock feature flag for
// that merchant. Returns the IDs the tests need.
func setupPinLockTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hasher *auth.PasswordHasher, pin string, flagEnabled bool, idleMinutes int) (companyID, merchantID, userID string) {
	t.Helper()
	companyID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	merchantID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	userID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	roleID := "dddddddd-dddd-dddd-dddd-dddddddddddd"

	_, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name, max_concurrent_sessions) VALUES ($1, 'test-co', 'Test Co', 10)", companyID)
	if err != nil {
		t.Fatalf("failed to seed company: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'test-merchant', 'Test Merchant')", merchantID, companyID)
	if err != nil {
		t.Fatalf("failed to seed merchant: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Test Role')", roleID, companyID)
	if err != nil {
		t.Fatalf("failed to seed role: %v", err)
	}
	passwordHash, err := hasher.Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	var pinHash *string
	if pin != "" {
		h, err := hasher.Hash(ctx, pin)
		if err != nil {
			t.Fatalf("failed to hash pin: %v", err)
		}
		pinHash = &h
	}
	_, err = pool.Exec(ctx,
		"INSERT INTO core.app_user (id, company_id, username, password_hash, pin_hash, is_active) VALUES ($1, $2, 'tester', $3, $4, true)",
		userID, companyID, passwordHash, pinHash,
	)
	if err != nil {
		t.Fatalf("failed to seed app_user: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID)
	if err != nil {
		t.Fatalf("failed to seed user_merchant_role: %v", err)
	}
	flagValue := []byte(fmt.Sprintf(`{"enabled": %t, "idle_minutes": %d}`, flagEnabled, idleMinutes))
	_, err = pool.Exec(ctx,
		"INSERT INTO core.feature_flag (merchant_id, flag_key, flag_value) VALUES ($1, 'auth.pin_lock', $2)",
		merchantID, flagValue,
	)
	if err != nil {
		t.Fatalf("failed to seed feature_flag: %v", err)
	}
	return companyID, merchantID, userID
}

func testHasher() *auth.PasswordHasher {
	return auth.NewPasswordHasher(4, 2*time.Second, auth.Argon2Params{MemoryKiB: 19456, Iterations: 2, Parallelism: 1})
}

func TestTenantMiddleware_IsTransactionScopedAcrossRequests(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, "99999999-9999-9999-9999-999999999999", "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "tester", "device-1", time.Hour)
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
	pool := newTestPostgresPool(t, ctx)

	// Unreachable Redis address on purpose — simulates Redis being down.
	unreachableRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer unreachableRedis.Close()

	router := server.NewRouter(pool, unreachableRedis, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, "99999999-9999-9999-9999-999999999999", "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "tester", "device-1", time.Hour)
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

func TestAppLockMiddleware_FlagDisabled_PassesThroughWithoutLockCheck(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	_, merchantID, userID := setupPinLockTestUser(t, ctx, pool, testHasher(), "123456", false, 5)

	router := server.NewRouter(pool, redisClient, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, userID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", merchantID, "tester", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("X-Company-ID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	req.Header.Set("X-Merchant-ID", merchantID)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with pin_lock flag disabled, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAppLockMiddleware_FlagEnabledNoUnlock_Returns423(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	_, merchantID, userID := setupPinLockTestUser(t, ctx, pool, testHasher(), "123456", true, 5)

	router := server.NewRouter(pool, redisClient, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, userID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", merchantID, "tester", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("X-Company-ID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	req.Header.Set("X-Merchant-ID", merchantID)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusLocked {
		t.Errorf("expected 423 when locked and no applock key set, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPinVerify_CorrectPin_UnlocksSubsequentRequests(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	_, merchantID, userID := setupPinLockTestUser(t, ctx, pool, testHasher(), "123456", true, 5)

	router := server.NewRouter(pool, redisClient, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, userID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", merchantID, "tester", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	verifyReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pin/verify", strings.NewReader(`{"pin":"123456"}`))
	verifyReq.Header.Set("Content-Type", "application/json")
	verifyReq.Header.Set("X-Company-ID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	verifyReq.Header.Set("X-Merchant-ID", merchantID)
	verifyReq.Header.Set("Authorization", "Bearer "+token)
	wVerify := httptest.NewRecorder()
	router.ServeHTTP(wVerify, verifyReq)
	if wVerify.Code != http.StatusOK {
		t.Fatalf("expected 200 from pin/verify with correct pin, got %d: %s", wVerify.Code, wVerify.Body.String())
	}

	pingReq := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	pingReq.Header.Set("X-Company-ID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	pingReq.Header.Set("X-Merchant-ID", merchantID)
	pingReq.Header.Set("Authorization", "Bearer "+token)
	wPing := httptest.NewRecorder()
	router.ServeHTTP(wPing, pingReq)
	if wPing.Code != http.StatusOK {
		t.Errorf("expected 200 after successful pin verify, got %d: %s", wPing.Code, wPing.Body.String())
	}
}

func TestPinVerify_TooManyWrongAttempts_RevokesSessionAndReturns403(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	_, merchantID, userID := setupPinLockTestUser(t, ctx, pool, testHasher(), "123456", true, 5)

	router := server.NewRouter(pool, redisClient, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, userID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", merchantID, "tester", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	// testConfig() sets AppLockMaxPinAttempts to 3 — the 3rd wrong attempt must revoke.
	var lastCode int
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/pin/verify", strings.NewReader(`{"pin":"000000"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Company-ID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
		req.Header.Set("X-Merchant-ID", merchantID)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		lastCode = w.Code
	}
	if lastCode != http.StatusForbidden {
		t.Errorf("expected 403 on reaching APP_LOCK_MAX_PIN_ATTEMPTS, got %d", lastCode)
	}
}

func TestRedisUnreachable_AppLockMiddleware_FailsClosedWith503(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	_, merchantID, userID := setupPinLockTestUser(t, ctx, pool, testHasher(), "123456", true, 5)

	unreachableRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer unreachableRedis.Close()
	router := server.NewRouter(pool, unreachableRedis, testConfig())
	token, err := auth.GenerateToken(testJWTSecret, userID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", merchantID, "tester", "device-1", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("X-Company-ID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	req.Header.Set("X-Merchant-ID", merchantID)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when Redis is unreachable and pin_lock is enabled, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCompanyLookup_ReturnsIDAndName_ForActiveSixDigitCode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	_, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '123456', 'Test Co')")
	if err != nil {
		t.Fatalf("failed to seed company: %v", err)
	}
	router := server.NewRouter(pool, redisClient, testConfig())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/companies/lookup?code=123456", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.Data.Name != "Test Co" {
		t.Errorf("expected name %q, got %q", "Test Co", body.Data.Name)
	}
}

func TestCompanyLookup_ReturnsNotFound_ForUnknownCode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/companies/lookup?code=000000", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Note: newTestPostgresPool connects as role "nexqia" (table owner, same role
// migrations run as) — this bypasses RLS regardless of the SECURITY DEFINER
// function, so this test does not catch a regression to a plain (non-DEFINER)
// query the way the production app_runtime role would. Same blind spot as
// every other integration test in this file.
func TestCompanyLookup_RejectsMalformedCode(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/companies/lookup?code=12", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
