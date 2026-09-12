//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/yogisaka/nexqia-api/internal/server"
)

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

	router := server.NewRouter(pool)

	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req1.Header.Set("X-Company-ID", "11111111-1111-1111-1111-111111111111")
	req1.Header.Set("X-Merchant-ID", "22222222-2222-2222-2222-222222222222")
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
	pool := &pgxpool.Pool{} // never dialed — middleware must reject before touching the pool
	router := server.NewRouter(pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing tenant headers, got %d", w.Code)
	}
}
