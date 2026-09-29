// internal/server/merchant_test.go
//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
	"github.com/yogisaka/nexqia-api/internal/session"
)

func TestCreateMerchantHandler_ActivatesNewMerchantForMerchantLessCaller(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	router := server.NewRouter(pool, redisClient, cfg)

	companyID := "40404040-4040-4040-4040-404040404040"
	userID := "50505050-5050-5050-5050-505050505050"
	roleID := "60606060-6060-6060-6060-606060606060"
	permissionID := "70707070-7070-7070-7070-707070707070"

	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ($1, 'merch-co', 'Merchant Test Co')", companyID); err != nil {
		t.Fatalf("failed to seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name, is_system) VALUES ($1, $2, 'Owner', true)", roleID, companyID); err != nil {
		t.Fatalf("failed to seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.permission (id, code, description, module) VALUES ($1, 'core.merchant.manage', 'test', 'core') ON CONFLICT DO NOTHING", permissionID); err != nil {
		t.Fatalf("failed to seed permission: %v", err)
	}
	// Migration 000043 already ships core.merchant.manage, so the insert above
	// may be skipped on UNIQUE(code) — resolve the real id for role_permission.
	if err := pool.QueryRow(ctx, "SELECT id FROM core.permission WHERE code = 'core.merchant.manage'").Scan(&permissionID); err != nil {
		t.Fatalf("failed to fetch core.merchant.manage permission id: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, permissionID); err != nil {
		t.Fatalf("failed to seed role_permission: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'merch.owner', $3, true)", userID, companyID, passwordHash); err != nil {
		t.Fatalf("failed to seed app_user: %v", err)
	}
	// The point of this test: user_company_role ONLY, zero user_merchant_role rows.
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_company_role (user_id, company_id, role_id) VALUES ($1, $2, $3)", userID, companyID, roleID); err != nil {
		t.Fatalf("failed to seed user_company_role: %v", err)
	}

	var userUUID, companyUUID pgtype.UUID
	if err := userUUID.Scan(userID); err != nil {
		t.Fatalf("failed to parse userID: %v", err)
	}
	if err := companyUUID.Scan(companyID); err != nil {
		t.Fatalf("failed to parse companyID: %v", err)
	}
	q := sqlcgen.New(pool)
	issued, err := session.Issue(ctx, q, session.Config{
		JWTSecret:      cfg.JWTSecret,
		AccessTokenTTL: time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute,
		IdleTimeout:    time.Duration(cfg.RefreshTokenIdleTimeoutHours) * time.Hour,
		AbsoluteTTL:    time.Duration(cfg.RefreshTokenAbsoluteTTLDays) * 24 * time.Hour,
	}, session.IssueParams{
		UserID: userUUID, CompanyID: companyUUID, MerchantID: pgtype.UUID{}, // no merchant yet
		Username: "merch.owner", DeviceID: "merch-device", DeviceLabel: "Merch Device",
		IP: netip.MustParseAddr("127.0.0.1"),
	})
	if err != nil {
		t.Fatalf("failed to issue test session: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"company_id": companyID, "name": "Klinik Pertama", "timezone": "Asia/Jakarta",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchants", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+issued.AccessToken)
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: issued.RefreshToken})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Merchant struct {
				ID string `json:"ID"`
			} `json:"merchant"`
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.Token == "" || resp.Data.Token == issued.AccessToken {
		t.Fatalf("expected a NEW, different token in response, got %q", resp.Data.Token)
	}
	if resp.Data.Merchant.ID == "" {
		t.Fatalf("expected merchant.ID in response, got empty")
	}
}

// TestCreateMerchantHandler_EnablesPinLock — spec §3: every new merchant gets an
// auth.pin_lock feature flag with {"enabled":true} at creation time.
func TestCreateMerchantHandler_EnablesPinLock(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, token, ownerCookie := registerTenantMFAOwner(t, router, "pinlock.owner", "081234520001")
	merchantID := createTenantMerchant(t, ctx, pool, router, token, ownerCookie, companyID, "Klinik PinLock")

	var enabled string
	if err := pool.QueryRow(ctx, "SELECT flag_value->>'enabled' FROM core.feature_flag WHERE merchant_id = $1 AND flag_key = 'auth.pin_lock'", merchantID).Scan(&enabled); err != nil {
		t.Fatalf("query auth.pin_lock flag for new merchant: %v", err)
	}
	if enabled != "true" {
		t.Fatalf("expected auth.pin_lock enabled = true for new merchant, got %q", enabled)
	}
}

// TestCreateMerchantHandler_PinNudgeOnNextLogin — spec §3: after the flag exists,
// a user without a PIN logging into that merchant gets pin_nudge in the response.
func TestCreateMerchantHandler_PinNudgeOnNextLogin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, token, ownerCookie := registerTenantMFAOwner(t, router, "pinnudge.owner", "081234520002")
	merchantID := createTenantMerchant(t, ctx, pool, router, token, ownerCookie, companyID, "Klinik PinNudge")
	_ = merchantID

	// Logout revokes the register auto-login session so login below doesn't hit
	// checkDeviceLimit.
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutReq.Header.Set("X-Company-ID", companyID)
	logoutReq.AddCookie(ownerCookie)
	logoutRec := httptest.NewRecorder()
	router.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout expected 200, got %d: %s", logoutRec.Code, logoutRec.Body.String())
	}

	// Exactly one merchant → login selects it directly and issues the session.
	loginRec := tenantLogin(t, router, companyID, "pinnudge.owner", "Passw0rd123!", "")
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", loginRec.Code, loginRec.Body.String())
	}
	data := decodePlatformData(t, loginRec)
	if data["requires_merchant_selection"] == true {
		t.Fatalf("expected single-merchant login, got selection payload %+v", data)
	}
	if nudge, ok := data["pin_nudge"].(bool); !ok || !nudge {
		t.Fatalf("expected pin_nudge: true for PIN-less user on pin-locked merchant, got %+v", data)
	}
}
