//go:build integration

package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// clientIPRequest builds the request exactly like accountRequest (same four
// headers) but lets the test set RemoteAddr and X-Forwarded-For BEFORE
// ServeHTTP — accountRequest calls ServeHTTP internally, so it cannot set
// RemoteAddr (spec 2026-09-29-small-security-fixes §1).
func clientIPRequest(router *gin.Engine, path, token, companyID, merchantID, remoteAddr, forwardedFor string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Merchant-ID", merchantID)
	req.RemoteAddr = remoteAddr
	req.Header.Set("X-Forwarded-For", forwardedFor)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// viewLogIP returns the ip_address of the single person/view access_log row
// for the company (the test pool connects as the table owner, so RLS on
// core.access_log does not apply to this read).
func viewLogIP(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID string) string {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM core.access_log WHERE company_id = $1 AND resource = 'person' AND action = 'view'",
		companyID).Scan(&count); err != nil {
		t.Fatalf("count person/view rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 person/view row, got %d", count)
	}
	var ip string
	if err := pool.QueryRow(ctx,
		"SELECT COALESCE(host(ip_address), '') FROM core.access_log WHERE company_id = $1 AND resource = 'person' AND action = 'view'",
		companyID).Scan(&ip); err != nil {
		t.Fatalf("read access_log ip_address: %v", err)
	}
	return ip
}

// TestClientIP_SpoofedHeaderFromPublicRemoteIgnored — a request from a public
// (untrusted) RemoteAddr carrying a fake X-Forwarded-For records the real
// remote address in access_log, not the spoofed header.
func TestClientIP_SpoofedHeaderFromPublicRemoteIgnored(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.ipspray", "081234570141")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Client IP Spoof Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Client IP Spoof Person")

	rec := clientIPRequest(router, "/api/v1/persons/"+personID, token, companyID, merchantID, "198.51.100.7:4321", "1.2.3.4")
	if rec.Code != http.StatusOK {
		t.Fatalf("get person expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if ip := viewLogIP(t, ctx, pool, companyID); ip != "198.51.100.7" {
		t.Fatalf("expected access_log ip_address 198.51.100.7 (spoofed header ignored), got %q", ip)
	}
}

// TestClientIP_TrustedProxyForwardsClient — a request from a trusted proxy
// (private range) has its X-Forwarded-For client address recorded in
// access_log.
func TestClientIP_TrustedProxyForwardsClient(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.ipproxy", "081234570151")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Client IP Proxy Person", "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Client IP Proxy Person")

	rec := clientIPRequest(router, "/api/v1/persons/"+personID, token, companyID, merchantID, "172.18.0.5:4321", "203.0.113.9")
	if rec.Code != http.StatusOK {
		t.Fatalf("get person expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if ip := viewLogIP(t, ctx, pool, companyID); ip != "203.0.113.9" {
		t.Fatalf("expected access_log ip_address 203.0.113.9 (trusted proxy forwarded), got %q", ip)
	}
}
