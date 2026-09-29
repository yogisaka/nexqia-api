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

// TestCreateAppUser_Conflict — duplicate username on POST /api/v1/users must
// return 409 with a specific message ("username sudah dipakai di perusahaan
// ini"), not a generic 500 (cleanup plan 2026-09-25, task 1). Seeding follows
// TestRequireCompanyLevelPermission_ExistingMerchantScopedRoleStillWorks in
// register_test.go.
func TestCreateAppUser_Conflict(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID := "b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1"
	merchantID := "c2c2c2c2-c2c2-c2c2-c2c2-c2c2c2c2c2c2"
	adminID := "d3d3d3d3-d3d3-d3d3-d3d3-d3d3d3d3d3d3"
	roleID := "e4e4e4e4-e4e4-e4e4-e4e4-e4e4e4e4e4e4"
	personID := "f5f5f5f5-f5f5-f5f5-f5f5-f5f5f5f5f5f5"

	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ($1, 'conf-co', 'Conflict Co')", companyID); err != nil {
		t.Fatalf("failed to seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'conf-merchant', 'Conflict Merchant')", merchantID, companyID); err != nil {
		t.Fatalf("failed to seed merchant: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Conflict Admin')", roleID, companyID); err != nil {
		t.Fatalf("failed to seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.permission (id, code, description, module) VALUES ('90909090-9090-9090-9090-909090909091', 'core.user.manage', 'test', 'core') ON CONFLICT (code) DO NOTHING"); err != nil {
		t.Fatalf("failed to seed permission: %v", err)
	}
	var permID string
	if err := pool.QueryRow(ctx, "SELECT id FROM core.permission WHERE code = 'core.user.manage'").Scan(&permID); err != nil {
		t.Fatalf("failed to fetch permission id: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, permID); err != nil {
		t.Fatalf("failed to seed role_permission: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'conf.admin', $3, true)", adminID, companyID, passwordHash); err != nil {
		t.Fatalf("failed to seed app_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", adminID, merchantID, roleID); err != nil {
		t.Fatalf("failed to seed user_merchant_role: %v", err)
	}
	// One shared person row — person_id is a plain FK (NOT unique) on
	// core.app_user, so both create calls may reuse it.
	if _, err := pool.Exec(ctx, "INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, 'Conflict Person', 'male')", personID, companyID); err != nil {
		t.Fatalf("failed to seed person: %v", err)
	}

	token, err := auth.GenerateToken(testJWTSecret, adminID, companyID, merchantID, "conf.admin", "conf-device", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate test token: %v", err)
	}

	postUser := func(username, email string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{
			"company_id": companyID,
			"person_id":  personID,
			"username":   username,
			"email":      email,
			"password":   "Passw0rd123!",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Company-ID", companyID)
		req.Header.Set("X-Merchant-ID", merchantID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := postUser("conflict.user", "conflict.user@example.com"); rec.Code != http.StatusCreated {
		t.Fatalf("first create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Same username, DIFFERENT email — so the only violated constraint is the
	// (company_id, username) one and the username branch is what's asserted.
	rec2 := postUser("conflict.user", "other.person@example.com")
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate username expected 409, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "username sudah dipakai di perusahaan ini" {
		t.Fatalf("expected username conflict message, got %q", resp.Error)
	}
}
