//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestLogin_TOTPPendingDoesNotTouchLastLogin — last_login_at changes only
// after the TOTP gate passes, like the platform login (spec
// 2026-10-06-request-tx-finalization §5).
func TestLogin_TOTPPendingDoesNotTouchLastLogin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, userID, merchantID := setupTenantMFAUser(t, ctx, pool, router, "lastlogin.owner", "081234090021")
	enableRequireTOTP(t, ctx, pool, merchantID)
	secret, _ := enrollTenantMFA(t, ctx, pool, router, companyID, "lastlogin.owner")

	// Fixed past marker: any later touch is visible regardless of clock resolution.
	marker := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, "UPDATE core.app_user SET last_login_at = $1 WHERE id = $2", marker, userID); err != nil {
		t.Fatalf("set last_login_at marker: %v", err)
	}
	lastLogin := func() time.Time {
		t.Helper()
		var ts time.Time
		if err := pool.QueryRow(ctx, "SELECT last_login_at FROM core.app_user WHERE id = $1", userID).Scan(&ts); err != nil {
			t.Fatalf("read last_login_at: %v", err)
		}
		return ts
	}

	rec := tenantLogin(t, router, companyID, "lastlogin.owner", "Passw0rd123!", "")
	if rec.Code != http.StatusOK || decodePlatformData(t, rec)["totp_required"] != true {
		t.Fatalf("password-only login expected 200 totp_required, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastLogin(); !got.Equal(marker) {
		t.Fatalf("last_login_at = %v after totp_required, want unchanged %v", got, marker)
	}

	rec = tenantLogin(t, router, companyID, "lastlogin.owner", "Passw0rd123!", "not-a-code")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong TOTP code expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastLogin(); !got.Equal(marker) {
		t.Fatalf("last_login_at = %v after wrong code, want unchanged %v", got, marker)
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	rec = tenantLogin(t, router, companyID, "lastlogin.owner", "Passw0rd123!", code)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with valid code expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastLogin(); !got.After(marker) {
		t.Fatalf("last_login_at = %v after successful login, want later than %v", got, marker)
	}
}
