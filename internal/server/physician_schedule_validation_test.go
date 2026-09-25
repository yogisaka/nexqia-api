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

// TestPhysicianSchedule_RejectsInvertedRanges covers the schedule validation
// contract (2026-09-25): create rejects end_time not strictly after start_time
// and effective_to before effective_from with 400 before any write, and
// accepts a well-formed schedule with 201.
func TestPhysicianSchedule_RejectsInvertedRanges(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID := "d1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1"
	merchantID := "d2c2c2c2-c2c2-c2c2-c2c2-c2c2c2c2c2c2"
	userID := "d3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3"
	roleID := "d4c4c4c4-c4c4-c4c4-c4c4-c4c4c4c4c4c4"
	personID := "d5c5c5c5-c5c5-c5c5-c5c5-c5c5c5c5c5c5"
	physicianID := "d6c6c6c6-c6c6-c6c6-c6c6-c6c6c6c6c6c6"
	departmentID := "d7c7c7c7-c7c7-c7c7-c7c7-c7c7c7c7c7c7"

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed failed (%s): %v", sql, err)
		}
	}
	mustExec("INSERT INTO core.company (id, code, name) VALUES ($1, 'SCHD01', 'Schedule Co')", companyID)
	mustExec("INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, 'MSCHD01', 'Klinik Schedule')", merchantID, companyID)
	mustExec("INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'Schedule Admin')", roleID, companyID)
	mustExec("INSERT INTO core.permission (code, description, module) VALUES ($1, 'test', 'operations') ON CONFLICT (code) DO NOTHING", server.PermScheduleManage)
	mustExec("INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = $2", roleID, server.PermScheduleManage)
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	mustExec("INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'sched.admin', $3, true)", userID, companyID, passwordHash)
	mustExec("INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, merchantID, roleID)
	mustExec("INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, 'Schedule Doctor', 'male')", personID, companyID)
	mustExec("INSERT INTO core.physician (id, company_id, merchant_id, person_id) VALUES ($1, $2, $3, $4)", physicianID, companyID, merchantID, personID)
	mustExec("INSERT INTO core.department (id, company_id, merchant_id, code, name) VALUES ($1, $2, $3, 'DSCHD01', 'Poli Schedule')", departmentID, companyID, merchantID)

	token, err := auth.GenerateToken(testJWTSecret, userID, companyID, merchantID, "sched.admin", "sched-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	post := func(body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/physician-schedules", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Company-ID", companyID)
		req.Header.Set("X-Merchant-ID", merchantID)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	expectError := func(rec *httptest.ResponseRecorder, wantStatus int, wantMsg string) {
		t.Helper()
		if rec.Code != wantStatus {
			t.Fatalf("expected %d, got %d: %s", wantStatus, rec.Code, rec.Body.String())
		}
		var got struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
		}
		if got.Error != wantMsg {
			t.Fatalf("expected error %q, got %q", wantMsg, got.Error)
		}
	}

	base := map[string]any{
		"merchant_id":    merchantID,
		"physician_id":   physicianID,
		"department_id":  departmentID,
		"day_of_week":    1,
		"slot_quota":     12,
		"effective_from": "2026-10-01",
	}

	// end_time before start_time.
	inverted := cloneJSONMap(base)
	inverted["start_time"] = "14:00"
	inverted["end_time"] = "08:00"
	expectError(post(inverted), http.StatusBadRequest, "end_time must be after start_time")

	// end_time equal to start_time ("strictly after").
	equal := cloneJSONMap(base)
	equal["start_time"] = "08:00"
	equal["end_time"] = "08:00"
	expectError(post(equal), http.StatusBadRequest, "end_time must be after start_time")

	// effective_to before effective_from.
	badPeriod := cloneJSONMap(base)
	badPeriod["start_time"] = "08:00"
	badPeriod["end_time"] = "14:00"
	badPeriod["effective_to"] = "2026-09-01"
	expectError(post(badPeriod), http.StatusBadRequest, "effective_to must be on or after effective_from")

	// Valid schedule.
	valid := cloneJSONMap(base)
	valid["start_time"] = "08:00"
	valid["end_time"] = "14:00"
	rec := post(valid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid schedule: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// cloneJSONMap returns a shallow copy so per-case keys don't leak between bodies.
func cloneJSONMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}