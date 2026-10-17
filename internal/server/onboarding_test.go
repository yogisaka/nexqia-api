//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// wantOnboardingKeys is the exact response order the FE checklist depends on.
var wantOnboardingKeys = []string{"departments", "locations", "physicians_schedules", "staff_roles", "clinical_config", "payers"}

type onboardingStatusItem struct {
	Key  string `json:"key"`
	Done bool   `json:"done"`
}

type onboardingStatusResponse struct {
	Data []onboardingStatusItem `json:"data"`
}

// TestOnboardingStatus_AllFalse checks a freshly seeded merchant: 200, exactly
// six items in contract order, everything done=false.
func TestOnboardingStatus_AllFalse(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "onboarding.allfalse", "+62811000001")
	token := merchantScopedToken(t, ctx, pool, userID, companyID, merchantID, "onboarding.allfalse")

	rec := accountRequest(router, "GET", "/api/v1/onboarding-status", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /onboarding-status: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp onboardingStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Data) != 6 {
		t.Fatalf("expected 6 items, got %d: %s", len(resp.Data), rec.Body.String())
	}
	for i, item := range resp.Data {
		if item.Key != wantOnboardingKeys[i] {
			t.Fatalf("item %d: expected key %q, got %q", i, wantOnboardingKeys[i], item.Key)
		}
		if item.Done {
			t.Fatalf("item %q: expected done=false on fresh merchant", item.Key)
		}
	}
}

// TestOnboardingStatus_DepartmentDone seeds one department and expects only the
// departments flag to flip to true.
func TestOnboardingStatus_DepartmentDone(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "onboarding.deptdone", "+62811000002")
	token := merchantScopedToken(t, ctx, pool, userID, companyID, merchantID, "onboarding.deptdone")

	// core.department: company_id, merchant_id, code unique, name, specialty NULL, created_by.
	companyUUID := mustParseUUID(t, companyID)
	merchantUUID := mustParseUUID(t, merchantID)
	userUUID := mustParseUUID(t, userID)
	if _, err := sqlcgen.New(pool).CreateDepartment(ctx, sqlcgen.CreateDepartmentParams{
		CompanyID: companyUUID, MerchantID: merchantUUID, Code: "ONB1", Name: "Poli Tes",
		CreatedBy: userUUID,
	}); err != nil {
		t.Fatalf("seed department: %v", err)
	}

	rec := accountRequest(router, "GET", "/api/v1/onboarding-status", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /onboarding-status: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp onboardingStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	done := map[string]bool{}
	for _, item := range resp.Data {
		done[item.Key] = item.Done
	}
	if !done["departments"] {
		t.Fatalf("expected departments=true, got %s", rec.Body.String())
	}
	for _, key := range []string{"locations", "physicians_schedules", "staff_roles", "clinical_config", "payers"} {
		if done[key] {
			t.Fatalf("expected %s=false, got %s", key, rec.Body.String())
		}
	}
}

// TestOnboardingStatus_RequiresPermission: a user whose role lacks
// core.merchant.manage gets 403 (pattern: clinical_config_test.go).
func TestOnboardingStatus_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID := "c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1"
	merchantID := "c2c2c2c2-c2c2-c2c2-c2c2-c2c2c2c2c2c2"
	userID := "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3"
	roleID := "c4c4c4c4-c4c4-c4c4-c4c4-c4c4c4c4c4c4"

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed failed (%s): %v", sql, err)
		}
	}
	mustExec("INSERT INTO core.company (id, code, name) VALUES ($1, 'CONB01', 'Onboarding Co')", companyID)
	mustExec("INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'MONB01', 'Klinik Onboarding')", merchantID, companyID)
	mustExec("INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Perm Role')", roleID, companyID)
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	mustExec("INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'onboarding.noperm', $3, true)", userID, companyID, passwordHash)
	mustExec("INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID)

	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "", "onboarding.noperm", "onboarding-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	rec := accountRequest(router, "GET", "/api/v1/onboarding-status", token, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /onboarding-status without permission: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func mustParseUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

// merchantScopedToken reissues a token carrying the merchant ("mid") claim:
// seedAccountOwner returns the registration token, which has no mid, and
// AuthMerchantID reads it from claims only.
func merchantScopedToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID, companyID, merchantID, username string) string {
	t.Helper()
	var roleID string
	if err := pool.QueryRow(ctx, "SELECT role_id::text FROM core.user_merchant_role WHERE user_id = $1::uuid AND merchant_id = $2::uuid", userID, merchantID).Scan(&roleID); err != nil {
		t.Fatalf("resolve owner role: %v", err)
	}
	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, roleID, username, "onboarding-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return token
}
