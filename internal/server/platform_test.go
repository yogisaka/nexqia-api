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
