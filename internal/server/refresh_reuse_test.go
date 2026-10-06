//go:build integration

package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestTenantRefresh_ReuseRevokesDeviceChain — presenting an already-rotated
// refresh token revokes the whole device chain, and that revocation must
// survive the 401 (spec 2026-10-06-request-tx-finalization §4 #2).
func TestTenantRefresh_ReuseRevokesDeviceChain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, userID, _, oldCookie := seedAccountOwner(t, ctx, pool, router, "refreuse.owner", "081234090011")

	refresh := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
		req.Header.Set("X-Company-ID", companyID)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := refresh(oldCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("first refresh expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	newCookie := cookieNamed(t, rec, "refresh_token")

	if rec := refresh(oldCookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reusing the rotated token expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	// The successor died with the device chain: the revocation was committed.
	if rec := refresh(newCookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("successor token after reuse expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	var active int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.refresh_token WHERE user_id = $1 AND revoked_at IS NULL", userID).Scan(&active); err != nil {
		t.Fatalf("count active refresh tokens: %v", err)
	}
	if active != 0 {
		t.Fatalf("active refresh tokens = %d after reuse detection, want 0", active)
	}
}
