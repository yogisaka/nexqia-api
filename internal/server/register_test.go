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

	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "regr.admin", "regr-device", time.Hour)
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
