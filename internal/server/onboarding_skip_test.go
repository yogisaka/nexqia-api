//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

type onboardingSkipResponse struct {
	Data struct {
		Skipped           bool    `json:"skipped"`
		SkippedAt         *string `json:"skipped_at"`
		CanManageMerchant bool    `json:"can_manage_merchant"`
	} `json:"data"`
}

// TestOnboardingSkipOwnerMerchantLess: a freshly registered owner (merchant
// already created, token still has no mid — the seedAccountOwner token is the
// registration token). GET starts skipped=false/null, POST flips to
// skipped=true with a filled RFC3339 timestamp, GET re-read persists it.
func TestOnboardingSkipOwnerMerchantLess(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	// 4th return = registration token, no mid claim (onboarding_test.go:158).
	companyID, _, _, token, _ := seedAccountOwner(t, ctx, pool, router, "onbskip.owner", "+62811000201")

	rec := accountRequest(router, "GET", "/api/v1/onboarding-skip", token, companyID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /onboarding-skip: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp onboardingSkipResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if resp.Data.Skipped || resp.Data.SkippedAt != nil || !resp.Data.CanManageMerchant {
		t.Fatalf("GET before skip: expected {skipped:false, skipped_at:null, can_manage_merchant:true}, got %s", rec.Body.String())
	}

	postRec := accountRequest(router, "POST", "/api/v1/onboarding-skip", token, companyID, "", nil)
	if postRec.Code != http.StatusOK {
		t.Fatalf("POST /onboarding-skip: expected 200, got %d: %s", postRec.Code, postRec.Body.String())
	}
	var postResp onboardingSkipResponse
	if err := json.Unmarshal(postRec.Body.Bytes(), &postResp); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if !postResp.Data.Skipped || postResp.Data.SkippedAt == nil || !postResp.Data.CanManageMerchant {
		t.Fatalf("POST skip: expected {skipped:true, skipped_at:filled, can_manage_merchant:true}, got %s", postRec.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, *postResp.Data.SkippedAt); err != nil {
		t.Fatalf("skipped_at not RFC3339: %v (%s)", err, *postResp.Data.SkippedAt)
	}

	reRec := accountRequest(router, "GET", "/api/v1/onboarding-skip", token, companyID, "", nil)
	if reRec.Code != http.StatusOK {
		t.Fatalf("GET /onboarding-skip re-read: expected 200, got %d: %s", reRec.Code, reRec.Body.String())
	}
	var reResp onboardingSkipResponse
	if err := json.Unmarshal(reRec.Body.Bytes(), &reResp); err != nil {
		t.Fatalf("decode re-GET response: %v", err)
	}
	if !reResp.Data.Skipped || reResp.Data.SkippedAt == nil {
		t.Fatalf("GET after skip: expected {skipped:true, skipped_at:filled}, got %s", reRec.Body.String())
	}
}

// TestOnboardingSkipStaffWithoutMerchantManage: a raw-SQL staff user without
// core.merchant.manage reads can_manage_merchant=false but can still skip
// their own onboarding (self endpoint, no permission required).
func TestOnboardingSkipStaffWithoutMerchantManage(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID := "d1d1d1d1-d1d1-d1d1-d1d1-d1d1d1d1d1d1"
	merchantID := "d2d2d2d2-d2d2-d2d2-d2d2-d2d2d2d2d2d2"
	userID := "d3d3d3d3-d3d3-d3d3-d3d3-d3d3d3d3d3d3"
	roleID := "d4d4d4d4-d4d4-d4d4-d4d4-d4d4d4d4d4d4"

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed failed (%s): %v", sql, err)
		}
	}
	mustExec("INSERT INTO core.company (id, code, name) VALUES ($1, 'OSK01', 'Onboarding Skip Co')", companyID)
	mustExec("INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'MOSK01', 'Klinik Skip')", merchantID, companyID)
	mustExec("INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Skip No Perm Role')", roleID, companyID)
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	mustExec("INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'onbskip.staff', $3, true)", userID, companyID, passwordHash)
	mustExec("INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID)

	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "", "onbskip.staff", "onbskip-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	rec := accountRequest(router, "GET", "/api/v1/onboarding-skip", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /onboarding-skip: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp onboardingSkipResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if resp.Data.Skipped || resp.Data.SkippedAt != nil || resp.Data.CanManageMerchant {
		t.Fatalf("GET staff: expected {skipped:false, skipped_at:null, can_manage_merchant:false}, got %s", rec.Body.String())
	}

	postRec := accountRequest(router, "POST", "/api/v1/onboarding-skip", token, companyID, merchantID, nil)
	if postRec.Code != http.StatusOK {
		t.Fatalf("POST /onboarding-skip staff: expected 200 (self skip allowed), got %d: %s", postRec.Code, postRec.Body.String())
	}
	var postResp onboardingSkipResponse
	if err := json.Unmarshal(postRec.Body.Bytes(), &postResp); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if !postResp.Data.Skipped || postResp.Data.SkippedAt == nil || postResp.Data.CanManageMerchant {
		t.Fatalf("POST staff: expected {skipped:true, skipped_at:filled, can_manage_merchant:false}, got %s", postRec.Body.String())
	}
}

// TestOnboardingSkipUnauthorized: no Authorization header → 401.
func TestOnboardingSkipUnauthorized(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "onbskip.unauth", "+62811000202")

	rec := accountRequest(router, "GET", "/api/v1/onboarding-skip", "", companyID, "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /onboarding-skip without token: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}
