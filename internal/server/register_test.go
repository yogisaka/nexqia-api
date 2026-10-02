//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

func registerPayload(username, email, phone string) []byte {
	b, _ := json.Marshal(map[string]string{
		"company_name":          "Test Register Co",
		"full_name":             "Test Owner",
		"username":              username,
		"email":                 email,
		"phone":                 phone,
		"password":              "Passw0rd123!",
		"password_confirmation": "Passw0rd123!",
	})
	return b
}

func TestRegisterHandler_Success(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload("test.owner", "test.owner@example.com", "081234500099")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "integration-test")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Token       string `json:"token"`
			CompanyID   string `json:"company_id"`
			CompanyCode string `json:"company_code"`
			UserID      string `json:"user_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.Token == "" || resp.Data.CompanyID == "" || resp.Data.CompanyCode == "" || resp.Data.UserID == "" {
		t.Fatalf("expected non-empty token/company_id/company_code/user_id, got %+v", resp.Data)
	}
}

func TestRegisterHandler_DuplicateEmail(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload("dup.owner1", "dup.owner@example.com", "081234500098")))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Device-Id", "integration-test-1")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first register expected 201, got %d: %s", rec1.Code, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload("dup.owner2", "dup.owner@example.com", "081234500097")))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Device-Id", "integration-test-2")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second register (dup email) expected 409, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestLoginHandler_OwnerWithNoMerchantCanLogin — regression test for the bug
// ketemu live (2026-09-24): an Owner who self-registered but never created
// their first merchant used to get 403 "user has no merchant assignment" on
// every subsequent login (register's own auto-login worked fine — LoginHandler
// was never updated to match). Register once, then log in separately (simulates
// logout+login) with correct credentials — must succeed with a merchant-less
// session, not be rejected.
func TestLoginHandler_OwnerWithNoMerchantCanLogin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	if _, err := pool.Exec(ctx, "INSERT INTO core.permission (id, code, description, module) VALUES ('80808080-8080-8080-8080-808080808081', 'core.merchant.manage', 'test', 'core') ON CONFLICT DO NOTHING"); err != nil {
		t.Fatalf("failed to seed core.merchant.manage permission: %v", err)
	}

	const deviceID = "integration-test-device"

	regReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload("nomerchant.owner", "nomerchant.owner@example.com", "081234500096")))
	regReq.Header.Set("Content-Type", "application/json")
	regReq.Header.Set("X-Device-Id", deviceID)
	regRec := httptest.NewRecorder()
	router.ServeHTTP(regRec, regReq)
	if regRec.Code != http.StatusCreated {
		t.Fatalf("register expected 201, got %d: %s", regRec.Code, regRec.Body.String())
	}
	var regResp struct {
		Data struct {
			CompanyID string `json:"company_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(regRec.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}

	// Logout (simulates the user closing/leaving the session before trying to
	// log back in) — revokes the auto-login session from register so the
	// subsequent login doesn't hit checkDeviceLimit, which is a separate
	// concern from the bug this test targets.
	var refreshCookie *http.Cookie
	for _, ck := range regRec.Result().Cookies() {
		if ck.Name == "refresh_token" {
			refreshCookie = ck
		}
	}
	if refreshCookie == nil {
		t.Fatalf("expected refresh_token cookie from register response")
	}
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutReq.Header.Set("X-Company-ID", regResp.Data.CompanyID)
	logoutReq.AddCookie(refreshCookie)
	logoutRec := httptest.NewRecorder()
	router.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout expected 200, got %d: %s", logoutRec.Code, logoutRec.Body.String())
	}

	loginBody, _ := json.Marshal(map[string]string{"username": "nomerchant.owner", "password": "Passw0rd123!"})
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("X-Company-ID", regResp.Data.CompanyID)
	loginReq.Header.Set("X-Device-Id", deviceID)
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusOK {
		t.Fatalf("login for merchant-less owner expected 200, got %d: %s", loginRec.Code, loginRec.Body.String())
	}
	var loginResp struct {
		Data struct {
			Token      string `json:"token"`
			MerchantID string `json:"merchant_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(loginRec.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if loginResp.Data.Token == "" {
		t.Fatalf("expected non-empty token, got %+v", loginResp.Data)
	}
	if loginResp.Data.MerchantID != "" {
		t.Fatalf("expected empty merchant_id for merchant-less owner, got %q", loginResp.Data.MerchantID)
	}

	// Real user journey continues here: create the first merchant using the
	// LOGIN session (not register's) — this is the exact chain reported live
	// 2026-09-24 ("failed to activate new merchant").
	var loginRefreshCookie *http.Cookie
	for _, ck := range loginRec.Result().Cookies() {
		if ck.Name == "refresh_token" {
			loginRefreshCookie = ck
		}
	}
	if loginRefreshCookie == nil {
		t.Fatalf("expected refresh_token cookie from login response")
	}
	createBody, _ := json.Marshal(map[string]string{
		"company_id": regResp.Data.CompanyID, "name": "Klinik Pertama", "timezone": "Asia/Jakarta",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/merchants", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+loginResp.Data.Token)
	createReq.Header.Set("X-Company-ID", regResp.Data.CompanyID)
	createReq.AddCookie(loginRefreshCookie)
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create first merchant after login expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
}

// TestRegisterThenCreateFirstMerchant_EndToEnd — exercises the exact real user
// journey (register → create first merchant) through actual HTTP endpoints
// only, no manual DB/session seeding shortcuts (unlike merchant_test.go's
// TestCreateMerchantHandler_ActivatesNewMerchantForMerchantLessCaller, which
// seeds session.Issue directly against the pool and so never exercised
// RegisterHandler's real cookie/session-issuance path at all). Diagnostic
// test for "failed to activate new merchant" reported live 2026-09-24.
func TestRegisterThenCreateFirstMerchant_EndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// No manual permission seed here on purpose: migration 000043 ships the
	// permission catalog (incl. core.merchant.manage), so this test is the
	// regression test that a fresh production-style DB (migrations only, no
	// seed) lets a newly registered Owner create their first merchant.

	regReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload("firstmerchant.owner", "firstmerchant.owner@example.com", "081234500094")))
	regReq.Header.Set("Content-Type", "application/json")
	regReq.Header.Set("X-Device-Id", "integration-test-e2e")
	regRec := httptest.NewRecorder()
	router.ServeHTTP(regRec, regReq)
	if regRec.Code != http.StatusCreated {
		t.Fatalf("register expected 201, got %d: %s", regRec.Code, regRec.Body.String())
	}
	var regResp struct {
		Data struct {
			CompanyID string `json:"company_id"`
			Token     string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(regRec.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	var refreshCookie *http.Cookie
	for _, ck := range regRec.Result().Cookies() {
		if ck.Name == "refresh_token" {
			refreshCookie = ck
		}
	}
	if refreshCookie == nil {
		t.Fatalf("expected refresh_token cookie in register response")
	}

	body, _ := json.Marshal(map[string]string{
		"company_id": regResp.Data.CompanyID, "name": "Klinik Pertama", "timezone": "Asia/Jakarta",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/merchants", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+regResp.Data.Token)
	createReq.Header.Set("X-Company-ID", regResp.Data.CompanyID)
	createReq.AddCookie(refreshCookie)
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)

	if createRec.Code != http.StatusCreated {
		t.Fatalf("create first merchant expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
}

// TestRefreshCookiePath_CoversMerchantsRoute — regression test for the bug
// ketemu live (2026-09-24): refreshCookiePath was "/api/v1/auth", but
// CreateMerchantHandler (0b, spec §3.5) reads this same cookie and is mounted
// at /api/v1/merchants — a SIBLING path, not a sub-path — so a real browser
// (which enforces cookie Path scoping) never sent the cookie there at all,
// every create-merchant call failed with "missing refresh token". This test
// asserts on the Set-Cookie Path ATTRIBUTE directly — httptest's client
// doesn't enforce Path scoping the way a browser does, so a naive "call
// CreateMerchantHandler with the cookie manually attached" test would pass
// even with the old broken Path and never catch this class of bug.
func TestRefreshCookiePath_CoversMerchantsRoute(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload("cookiepath.owner", "cookiepath.owner@example.com", "081234500095")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "integration-test-cookiepath")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var refreshCookie *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "refresh_token" {
			refreshCookie = ck
		}
	}
	if refreshCookie == nil {
		t.Fatalf("expected refresh_token cookie in register response")
	}
	// Must be broad enough to cover /api/v1/merchants (a sibling of /api/v1/auth,
	// not a sub-path of it) — "/api/v1/auth" alone is NOT sufficient.
	if refreshCookie.Path != "/api/v1" {
		t.Fatalf("expected refresh_token cookie Path to be /api/v1 (covers /api/v1/merchants), got %q", refreshCookie.Path)
	}
}

func TestRequireCompanyLevelPermission_ExistingMerchantScopedRoleStillWorks(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	merchantID := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	userID := "10101010-1010-1010-1010-101010101010"
	roleID := "20202020-2020-2020-2020-202020202020"
	permissionID := "30303030-3030-3030-3030-303030303030"

	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ($1, 'regr-co', 'Regression Co')", companyID); err != nil {
		t.Fatalf("failed to seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'regr-merchant', 'Regression Merchant')", merchantID, companyID); err != nil {
		t.Fatalf("failed to seed merchant: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Regression Admin')", roleID, companyID); err != nil {
		t.Fatalf("failed to seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.permission (id, code, description, module) VALUES ($1, 'core.company.manage.own', 'test', 'core') ON CONFLICT (code) DO NOTHING", permissionID); err != nil {
		t.Fatalf("failed to seed permission: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT id FROM core.permission WHERE code = 'core.company.manage.own'").Scan(&permissionID); err != nil {
		t.Fatalf("failed to fetch permission id: %v", err)
	}
	// Also seed core.merchant.manage — needed by Update/Delete/ListMerchantHandler
	// after they were moved to companyOnlyAuthed group (uses RequireCompanyLevelPermission
	// instead of RequirePermissionForMerchant).
	permMerchantManageID := "80808080-8080-8080-8080-808080808080"
	if _, err := pool.Exec(ctx, "INSERT INTO core.permission (id, code, description, module) VALUES ($1, 'core.merchant.manage', 'Kelola merchant', 'core') ON CONFLICT (code) DO NOTHING", permMerchantManageID); err != nil {
		t.Fatalf("failed to seed permission: %v", err)
	}
	var actualMerchantPermID string
	if err := pool.QueryRow(ctx, "SELECT id FROM core.permission WHERE code = 'core.merchant.manage'").Scan(&actualMerchantPermID); err != nil {
		t.Fatalf("failed to fetch permission id: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, permissionID); err != nil {
		t.Fatalf("failed to seed role_permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, actualMerchantPermID); err != nil {
		t.Fatalf("failed to seed role_permission: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'regr.admin', $3, true)", userID, companyID, passwordHash); err != nil {
		t.Fatalf("failed to seed app_user: %v", err)
	}
	// The regression case: ONLY user_merchant_role, deliberately NO user_company_role row.
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("failed to seed user_merchant_role: %v", err)
	}

	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "", "regr.admin", "regr-device", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate test token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/companies/"+companyID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", companyID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (regression: merchant-scoped-only role should still reach GetCompanyHandler via companyOnlyAuthed), got %d: %s", rec.Code, rec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/companies/"+companyID+"/merchants", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listReq.Header.Set("X-Company-ID", companyID)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from ListMerchantsHandler, got %d: %s", listRec.Code, listRec.Body.String())
	}

	updateBody, _ := json.Marshal(map[string]any{
		"name": "Regression Merchant Renamed", "timezone": "Asia/Jakarta", "is_active": true,
	})
	updateReq := httptest.NewRequest(http.MethodPatch, "/api/v1/merchants/"+merchantID, bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", "Bearer "+token)
	updateReq.Header.Set("X-Company-ID", companyID)
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	router.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from UpdateMerchantHandler, got %d: %s", updateRec.Code, rec.Body.String())
	}
}
