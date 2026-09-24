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

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
	"github.com/yogisaka/nexqia-api/internal/session"
)

func TestPlatformAdminLogin_Success(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	adminID := "40404040-4040-4040-4040-404040404040"
	roleID := "50505050-5050-5050-5050-505050505050"
	permID := "60606060-6060-6060-6060-606060606060"

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.company.view', 'test', 'platform')", permID); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, 'Test Super Admin', true)", roleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, permID); err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (id, username, email, full_name, password_hash, is_active) VALUES ($1, 'testadmin', 'testadmin@nexqia.internal', 'Test Admin', $2, true)", adminID, passwordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)", adminID, roleID); err != nil {
		t.Fatalf("seed admin_user_role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": "testadmin", "password": "correct-horse"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "platform-test-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.Token == "" {
		t.Fatalf("expected non-empty token, got %+v", resp.Data)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatalf("expected a Set-Cookie header for the refresh token")
	}
}

func TestPlatformAdminLogin_WrongPassword(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (username, email, full_name, password_hash, is_active) VALUES ('wrongpwtest', 'wrongpwtest@nexqia.internal', 'Test', $1, true)", passwordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": "wrongpwtest", "password": "not-the-password"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "platform-test-device-2")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListPlatformCompanies_NoToken(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/platform/companies", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListPlatformCompanies_ReturnsAcrossTenants(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	adminID := "41414141-4141-4141-4141-414141414141"
	roleID := "51515151-5151-5151-5151-515151515151"
	permID := "61616161-6161-6161-6161-616161616161"
	companyID1 := "71717171-7171-7171-7171-717171717171"
	companyID2 := "72727272-7272-7272-7272-727272727272"

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.company.view', 'test', 'platform') ON CONFLICT (id) DO NOTHING", permID); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, 'Cross-Tenant Test Admin', true)", roleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, permID); err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (id, username, email, full_name, password_hash, is_active) VALUES ($1, 'crosstenanttest', 'crosstenanttest@nexqia.internal', 'Test', $2, true)", adminID, passwordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)", adminID, roleID); err != nil {
		t.Fatalf("seed admin_user_role: %v", err)
	}
	// 2 DIFFERENT tenant companies — this is what proves the RLS-bypass
	// function actually works (spec §1/§8): a plain SELECT under RLS would
	// only ever return 1 row (whichever matches app.current_company_id, which
	// isn't even set on this route at all).
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ($1, 'xtenant1', 'Cross Tenant Co 1')", companyID1); err != nil {
		t.Fatalf("seed company 1: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ($1, 'xtenant2', 'Cross Tenant Co 2')", companyID2); err != nil {
		t.Fatalf("seed company 2: %v", err)
	}

	token, err := auth.GeneratePlatformAdminToken(testJWTSecret, adminID, "crosstenanttest", "platform-test-device-3", time.Hour)
	if err != nil {
		t.Fatalf("generate platform admin token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/platform/companies", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			ID string `json:"ID"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	foundBoth := map[string]bool{companyID1: false, companyID2: false}
	for _, c := range resp.Data {
		if _, ok := foundBoth[c.ID]; ok {
			foundBoth[c.ID] = true
		}
	}
	for id, found := range foundBoth {
		if !found {
			t.Fatalf("expected company %s in cross-tenant list, got %+v", id, resp.Data)
		}
	}
}

func TestPlatformRoutes_RejectOrdinaryTenantToken(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// An ordinary tenant access token (auth.GenerateToken, purpose "access") —
	// must NOT work against platform routes, regression against any future
	// change that accidentally merges the two auth systems.
	tenantToken, err := auth.GenerateToken(testJWTSecret, "80808080-8080-8080-8080-808080808080", "90909090-9090-9090-9090-909090909090", "a0a0a0a0-a0a0-a0a0-a0a0-a0a0a0a0a0a0", "sometenantuser", "tenant-device", time.Hour)
	if err != nil {
		t.Fatalf("generate tenant token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/platform/companies", nil)
	req.Header.Set("Authorization", "Bearer "+tenantToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (tenant token rejected by platform auth), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlatformAdminRefresh_ReuseDetectionRevokesDeviceChain(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (username, email, full_name, password_hash, is_active) VALUES ('refreshreusetest', 'refreshreusetest@nexqia.internal', 'Test', $1, true)", passwordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}

	loginBody, _ := json.Marshal(map[string]string{"username": "refreshreusetest", "password": "correct-horse"})
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/platform/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("X-Device-Id", "reuse-test-device")
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", loginRec.Code, loginRec.Body.String())
	}
	var refreshCookie *http.Cookie
	for _, ck := range loginRec.Result().Cookies() {
		if ck.Name == "platform_refresh_token" {
			refreshCookie = ck
		}
	}
	if refreshCookie == nil {
		t.Fatalf("expected platform_refresh_token cookie from login")
	}

	// First refresh: succeeds, rotates the token.
	refreshReq1 := httptest.NewRequest(http.MethodPost, "/api/v1/platform/refresh", nil)
	refreshReq1.AddCookie(refreshCookie)
	refreshRec1 := httptest.NewRecorder()
	router.ServeHTTP(refreshRec1, refreshReq1)
	if refreshRec1.Code != http.StatusOK {
		t.Fatalf("first refresh expected 200, got %d: %s", refreshRec1.Code, refreshRec1.Body.String())
	}

	// Reuse the OLD (now-revoked) cookie — must fail AND revoke the successor too.
	refreshReq2 := httptest.NewRequest(http.MethodPost, "/api/v1/platform/refresh", nil)
	refreshReq2.AddCookie(refreshCookie)
	refreshRec2 := httptest.NewRecorder()
	router.ServeHTTP(refreshRec2, refreshReq2)
	if refreshRec2.Code != http.StatusUnauthorized {
		t.Fatalf("reused refresh token expected 401, got %d: %s", refreshRec2.Code, refreshRec2.Body.String())
	}

	// The successor from refresh #1 must ALSO now be revoked (device-chain
	// revocation) — extract it and confirm it no longer works either.
	var successorCookie *http.Cookie
	for _, ck := range refreshRec1.Result().Cookies() {
		if ck.Name == "platform_refresh_token" {
			successorCookie = ck
		}
	}
	if successorCookie == nil {
		t.Fatalf("expected platform_refresh_token cookie from first refresh")
	}
	refreshReq3 := httptest.NewRequest(http.MethodPost, "/api/v1/platform/refresh", nil)
	refreshReq3.AddCookie(successorCookie)
	refreshRec3 := httptest.NewRecorder()
	router.ServeHTTP(refreshRec3, refreshReq3)
	if refreshRec3.Code != http.StatusUnauthorized {
		t.Fatalf("successor token expected 401 after device-chain revocation, got %d: %s", refreshRec3.Code, refreshRec3.Body.String())
	}
}

func TestLoginHandler_SuspendedCompanyRejected(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID := "20202020-2020-2020-2020-202020202020"
	userID := "21212121-2121-2121-2121-212121212121"

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name, is_active) VALUES ($1, 'suspco', 'Suspended Co', false)", companyID); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'suspuser', $3, true)", userID, companyID, passwordHash); err != nil {
		t.Fatalf("seed app_user: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": "suspuser", "password": "correct-horse"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Device-Id", "suspend-test-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginHandler_ActiveCompanyStillWorks(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID := "22222222-2222-2222-2222-222222222222"
	merchantID := "23232323-2323-2323-2323-232323232323"
	roleID := "24242424-2424-2424-2424-242424242424"
	userID := "25252525-2525-2525-2525-252525252525"

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name, is_active) VALUES ($1, 'activeco', 'Active Co', true)", companyID); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'activem', 'Active Merchant')", merchantID, companyID); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Active Test Role')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'activeuser', $3, true)", userID, companyID, passwordHash); err != nil {
		t.Fatalf("seed app_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"username": "activeuser", "password": "correct-horse"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Device-Id", "active-test-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (regression: active company must still be able to log in), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSuspendPlatformCompany_RevokesActiveSessions(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	router := server.NewRouter(pool, redisClient, cfg)

	// Platform admin fixture (same shape as 0d-1's TestListPlatformCompanies_ReturnsAcrossTenants).
	adminID := "30303030-3030-3030-3030-303030303030"
	adminRoleID := "31313131-3131-3131-3131-313131313131"
	permViewID := "32323232-3232-3232-3232-323232323232"
	permManageID := "33333333-3333-3333-3333-333333333333"

	adminPasswordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.company.view', 'test', 'platform') ON CONFLICT (id) DO NOTHING", permViewID); err != nil {
		t.Fatalf("seed permission view: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.company.manage', 'test', 'platform') ON CONFLICT (id) DO NOTHING", permManageID); err != nil {
		t.Fatalf("seed permission manage: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, 'Suspend Test Admin', true)", adminRoleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role_permission (role_id, permission_id) VALUES ($1, $2), ($1, $3)", adminRoleID, permViewID, permManageID); err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (id, username, email, full_name, password_hash, is_active) VALUES ($1, 'suspendadmin', 'suspendadmin@nexqia.internal', 'Test', $2, true)", adminID, adminPasswordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)", adminID, adminRoleID); err != nil {
		t.Fatalf("seed admin_user_role: %v", err)
	}

	// Tenant fixture with a real, live refresh-token session.
	companyID := "34343434-3434-3434-3434-343434343434"
	merchantID := "35353535-3535-3535-3535-353535353535"
	tenantRoleID := "36363636-3636-3636-3636-363636363636"
	tenantUserID := "37373737-3737-3737-3737-373737373737"

	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name, is_active) VALUES ($1, 'tokill', 'Company To Suspend', true)", companyID); err != nil {
		t.Fatalf("seed tenant company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'tokillm', 'Merchant')", merchantID, companyID); err != nil {
		t.Fatalf("seed tenant merchant: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Tenant Role')", tenantRoleID, companyID); err != nil {
		t.Fatalf("seed tenant role: %v", err)
	}
	tenantPasswordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash tenant password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'tokilluser', $3, true)", tenantUserID, companyID, tenantPasswordHash); err != nil {
		t.Fatalf("seed tenant app_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", tenantUserID, merchantID, tenantRoleID); err != nil {
		t.Fatalf("seed tenant user_merchant_role: %v", err)
	}

	var companyUUID, merchantUUID, userUUID pgtype.UUID
	if err := companyUUID.Scan(companyID); err != nil {
		t.Fatalf("parse companyID: %v", err)
	}
	if err := merchantUUID.Scan(merchantID); err != nil {
		t.Fatalf("parse merchantID: %v", err)
	}
	if err := userUUID.Scan(tenantUserID); err != nil {
		t.Fatalf("parse tenantUserID: %v", err)
	}
	q := sqlcgen.New(pool)
	issued, err := session.Issue(ctx, q, session.Config{
		JWTSecret:      cfg.JWTSecret,
		AccessTokenTTL: time.Duration(cfg.AccessTokenTTLMinutes) * time.Minute,
		IdleTimeout:    time.Duration(cfg.RefreshTokenIdleTimeoutHours) * time.Hour,
		AbsoluteTTL:    time.Duration(cfg.RefreshTokenAbsoluteTTLDays) * 24 * time.Hour,
	}, session.IssueParams{
		UserID: userUUID, CompanyID: companyUUID, MerchantID: merchantUUID,
		Username: "tokilluser", DeviceID: "suspend-victim-device", DeviceLabel: "test",
		IP: netip.MustParseAddr("127.0.0.1"),
	})
	if err != nil {
		t.Fatalf("issue tenant session: %v", err)
	}

	// Log in as platform admin.
	adminLoginBody, _ := json.Marshal(map[string]string{"username": "suspendadmin", "password": "correct-horse"})
	adminLoginReq := httptest.NewRequest(http.MethodPost, "/api/v1/platform/login", bytes.NewReader(adminLoginBody))
	adminLoginReq.Header.Set("Content-Type", "application/json")
	adminLoginReq.Header.Set("X-Device-Id", "suspend-admin-device")
	adminLoginRec := httptest.NewRecorder()
	router.ServeHTTP(adminLoginRec, adminLoginReq)
	if adminLoginRec.Code != http.StatusOK {
		t.Fatalf("admin login expected 200, got %d: %s", adminLoginRec.Code, adminLoginRec.Body.String())
	}
	var adminLoginResp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(adminLoginRec.Body.Bytes(), &adminLoginResp); err != nil {
		t.Fatalf("decode admin login response: %v", err)
	}

	// Suspend the tenant company.
	suspendReq := httptest.NewRequest(http.MethodPost, "/api/v1/platform/companies/"+companyID+"/suspend", nil)
	suspendReq.Header.Set("Authorization", "Bearer "+adminLoginResp.Data.Token)
	suspendRec := httptest.NewRecorder()
	router.ServeHTTP(suspendRec, suspendReq)
	if suspendRec.Code != http.StatusOK {
		t.Fatalf("suspend expected 200, got %d: %s", suspendRec.Code, suspendRec.Body.String())
	}

	// The tenant's already-issued refresh token must now be rejected.
	refreshReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	refreshReq.AddCookie(&http.Cookie{Name: "refresh_token", Value: issued.RefreshToken})
	refreshReq.Header.Set("X-Company-ID", companyID)
	refreshRec := httptest.NewRecorder()
	router.ServeHTTP(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (session force-revoked by suspend), got %d: %s", refreshRec.Code, refreshRec.Body.String())
	}
}

func TestSuspendPlatformCompany_NotFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	adminID := "38383838-3838-3838-3838-383838383838"
	roleID := "39393939-3939-3939-3939-393939393939"
	permID := "40404040-4040-4040-4040-404040404040"

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.company.manage', 'test', 'platform') ON CONFLICT (id) DO NOTHING", permID); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, 'NotFound Test Admin', true)", roleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, permID); err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (id, username, email, full_name, password_hash, is_active) VALUES ($1, 'notfoundadmin', 'notfoundadmin@nexqia.internal', 'Test', $2, true)", adminID, passwordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)", adminID, roleID); err != nil {
		t.Fatalf("seed admin_user_role: %v", err)
	}

	token, err := auth.GeneratePlatformAdminToken(testJWTSecret, adminID, "notfoundadmin", "notfound-device", time.Hour)
	if err != nil {
		t.Fatalf("generate platform admin token: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/companies/00000000-0000-0000-0000-000000000000/suspend", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent company id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSuspendPlatformCompany_ActivateRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// Platform admin with platform.company.manage (same shape as TestSuspendPlatformCompany_NotFound).
	adminID := "42424242-4242-4242-4242-424242424242"
	adminRoleID := "43434343-4343-4343-4343-434343434343"
	permManageID := "44444444-4444-4444-4444-444444444444"

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.company.manage', 'test', 'platform') ON CONFLICT (id) DO NOTHING", permManageID); err != nil {
		t.Fatalf("seed permission manage: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, 'Activate Test Admin', true)", adminRoleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role_permission (role_id, permission_id) VALUES ($1, $2)", adminRoleID, permManageID); err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (id, username, email, full_name, password_hash, is_active) VALUES ($1, 'activateadmin', 'activateadmin@nexqia.internal', 'Test', $2, true)", adminID, passwordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)", adminID, adminRoleID); err != nil {
		t.Fatalf("seed admin_user_role: %v", err)
	}

	// Tenant fixture — mirror TestLoginHandler_ActiveCompanyStillWorks: a 200
	// login needs company + merchant + role + user_merchant_role, not just the
	// company row (the suspended-rejection path bails before merchant lookup).
	companyID := "45454545-4545-4545-4545-454545454545"
	merchantID := "46464646-4646-4646-4646-464646464646"
	tenantRoleID := "47474747-4747-4747-4747-474747474747"
	tenantUserID := "48484848-4848-4848-4848-484848484848"

	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name, is_active) VALUES ($1, 'activateco', 'Activate Co', true)", companyID); err != nil {
		t.Fatalf("seed tenant company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'activatem', 'Merchant')", merchantID, companyID); err != nil {
		t.Fatalf("seed tenant merchant: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Tenant Role')", tenantRoleID, companyID); err != nil {
		t.Fatalf("seed tenant role: %v", err)
	}
	tenantPasswordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash tenant password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'activateuser', $3, true)", tenantUserID, companyID, tenantPasswordHash); err != nil {
		t.Fatalf("seed tenant app_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", tenantUserID, merchantID, tenantRoleID); err != nil {
		t.Fatalf("seed tenant user_merchant_role: %v", err)
	}

	tenantLoginCode := func() int {
		body, _ := json.Marshal(map[string]string{"username": "activateuser", "password": "correct-horse"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Company-ID", companyID)
		req.Header.Set("X-Device-Id", "activate-roundtrip-device")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	adminToken, err := auth.GeneratePlatformAdminToken(testJWTSecret, adminID, "activateadmin", "activate-admin-device", time.Hour)
	if err != nil {
		t.Fatalf("generate platform admin token: %v", err)
	}

	if code := tenantLoginCode(); code != http.StatusOK {
		t.Fatalf("pre-suspend login expected 200, got %d", code)
	}

	suspendReq := httptest.NewRequest(http.MethodPost, "/api/v1/platform/companies/"+companyID+"/suspend", nil)
	suspendReq.Header.Set("Authorization", "Bearer "+adminToken)
	suspendRec := httptest.NewRecorder()
	router.ServeHTTP(suspendRec, suspendReq)
	if suspendRec.Code != http.StatusOK {
		t.Fatalf("suspend expected 200, got %d: %s", suspendRec.Code, suspendRec.Body.String())
	}

	if code := tenantLoginCode(); code != http.StatusForbidden {
		t.Fatalf("suspended login expected 403, got %d", code)
	}

	activateReq := httptest.NewRequest(http.MethodPost, "/api/v1/platform/companies/"+companyID+"/activate", nil)
	activateReq.Header.Set("Authorization", "Bearer "+adminToken)
	activateRec := httptest.NewRecorder()
	router.ServeHTTP(activateRec, activateReq)
	if activateRec.Code != http.StatusOK {
		t.Fatalf("activate expected 200, got %d: %s", activateRec.Code, activateRec.Body.String())
	}

	if code := tenantLoginCode(); code != http.StatusOK {
		t.Fatalf("post-activate login expected 200 (round-trip), got %d", code)
	}
}

func TestSuspendPlatformCompany_ForbiddenWithoutPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// Platform admin with ONLY platform.company.view — no platform.company.manage.
	adminID := "4a4a4a4a-4a4a-4a4a-4a4a-4a4a4a4a4a4a"
	adminRoleID := "4b4b4b4b-4b4b-4b4b-4b4b-4b4b4b4b4b4b"
	permViewID := "4c4c4c4c-4c4c-4c4c-4c4c-4c4c4c4c4c4c"

	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.company.view', 'test', 'platform') ON CONFLICT (id) DO NOTHING", permViewID); err != nil {
		t.Fatalf("seed permission view: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, 'View Only Test Admin', true)", adminRoleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role_permission (role_id, permission_id) VALUES ($1, $2)", adminRoleID, permViewID); err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (id, username, email, full_name, password_hash, is_active) VALUES ($1, 'nopermadmin', 'nopermadmin@nexqia.internal', 'Test', $2, true)", adminID, passwordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)", adminID, adminRoleID); err != nil {
		t.Fatalf("seed admin_user_role: %v", err)
	}

	// Real company target — proves the 403 comes from the permission check,
	// not the 404 nonexistent-company path.
	companyID := "4d4d4d4d-4d4d-4d4d-4d4d-4d4d4d4d4d4d"
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name, is_active) VALUES ($1, 'nopermco', 'No Perm Co', true)", companyID); err != nil {
		t.Fatalf("seed company: %v", err)
	}

	token, err := auth.GeneratePlatformAdminToken(testJWTSecret, adminID, "nopermadmin", "noperm-device", time.Hour)
	if err != nil {
		t.Fatalf("generate platform admin token: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/companies/"+companyID+"/suspend", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (missing platform.company.manage), got %d: %s", rec.Code, rec.Body.String())
	}
}
