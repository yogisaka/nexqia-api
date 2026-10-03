//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// schedUUID builds a deterministic UUID (8-4-4-4-12) unique per (prefix, n) so
// tests in this file can share a database without ID collisions. prefix must
// be an ASCII hex character ('1'..'9').
func schedUUID(prefix byte, n int) string {
	return fmt.Sprintf("%c0c0000a-0000-4000-8000-%012d", prefix, n)
}

// scheduleTestEnv holds the seeded chain shared by TestScheduleCalendar_*.
type scheduleTestEnv struct {
	pool          *pgxpool.Pool
	router        *gin.Engine
	companyID     string
	merchantID    string
	physicianID   string
	departmentID  string
	practiceRoom  string
	practiceRoom2 string
	wardRoom      string
	payerBPJS     string
	payerPrivate  string
	patientID     string
	token         string
	noPermToken   string
	visitCounter  int
}

// newScheduleTestEnv seeds company/merchant/users/roles/physician/department/
// payers (owner pool, bypassing RLS) and locations (tenant tx, RLS applies).
// Every identifier is derived from prefix so tests don't collide.
func newScheduleTestEnv(t *testing.T, ctx context.Context, prefix byte, tag, sipValidUntil string) *scheduleTestEnv {
	t.Helper()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	e := &scheduleTestEnv{
		pool:          pool,
		router:        router,
		companyID:     schedUUID(prefix, 1),
		merchantID:    schedUUID(prefix, 2),
		physicianID:   schedUUID(prefix, 8),
		departmentID:  schedUUID(prefix, 9),
		practiceRoom:  schedUUID(prefix, 10),
		practiceRoom2: schedUUID(prefix, 15),
		wardRoom:      schedUUID(prefix, 11),
		payerBPJS:     schedUUID(prefix, 12),
		payerPrivate:  schedUUID(prefix, 13),
		patientID:     schedUUID(prefix, 14),
	}
	roleID := schedUUID(prefix, 3)
	noPermRoleID := schedUUID(prefix, 4)
	userID := schedUUID(prefix, 5)
	noPermUserID := schedUUID(prefix, 6)
	personID := schedUUID(prefix, 7)

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed failed (%s): %v", sql, err)
		}
	}
	mustExec("INSERT INTO core.company (id, code, name) VALUES ($1, $2, $3)", e.companyID, tag+"CO", "Schedule Co "+tag)
	mustExec("INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, $3, $4)", e.merchantID, e.companyID, "M"+tag, "Klinik "+tag)
	// Pin the timezone so admission local dates are deterministic.
	mustExec("UPDATE core.merchant SET timezone = 'Asia/Jakarta' WHERE id = $1", e.merchantID)
	mustExec("INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, $3)", roleID, e.companyID, tag+" Admin")
	mustExec("INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, $3)", noPermRoleID, e.companyID, tag+" Viewer")
	mustExec("INSERT INTO core.permission (code, description, module) VALUES ($1, 'test', 'operations') ON CONFLICT (code) DO NOTHING", server.PermScheduleManage)
	mustExec("INSERT INTO core.role_permission (role_id, permission_id) SELECT $1, id FROM core.permission WHERE code = $2", roleID, server.PermScheduleManage)
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	mustExec("INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)", userID, e.companyID, tag+".admin", passwordHash)
	mustExec("INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)", noPermUserID, e.companyID, tag+".viewer", passwordHash)
	mustExec("INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", userID, e.merchantID, roleID)
	mustExec("INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", noPermUserID, e.merchantID, noPermRoleID)
	mustExec("INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, $3, 'male')", personID, e.companyID, "Dokter "+tag)
	sip := "NULL"
	if sipValidUntil != "" {
		sip = fmt.Sprintf("'%s'", sipValidUntil)
	}
	mustExec("INSERT INTO core.physician (id, company_id, merchant_id, person_id, sip_number, sip_valid_until) VALUES ($1, $2, $3, $4, $5, "+sip+"::date)",
		e.physicianID, e.companyID, e.merchantID, personID, "SIP-"+tag)
	mustExec("INSERT INTO core.department (id, company_id, merchant_id, code, name) VALUES ($1, $2, $3, $4, $5)", e.departmentID, e.companyID, e.merchantID, tag+"-DEPT", "Poli "+tag)
	mustExec("INSERT INTO core.payer (id, company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, $4, $5, 'bpjs')", e.payerBPJS, e.companyID, e.merchantID, tag+"-BPJS", "BPJS "+tag)
	mustExec("INSERT INTO core.payer (id, company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, $4, $5, 'self_pay')", e.payerPrivate, e.companyID, e.merchantID, tag+"-PRIV", "Umum "+tag)
	mustExec("INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, 'Pasien '||$3::text, 'female')", e.patientID, e.companyID, tag)

	// Locations need the merchant GUC (RLS + tenant-consistency trigger).
	tx := locationTx(t, ctx, pool, e.companyID, e.merchantID)
	for _, loc := range []struct{ id, kind, fn, name string }{
		{e.practiceRoom, "room", "{practice}", "Ruang Praktik " + tag},
		{e.practiceRoom2, "room", "{practice}", "Ruang Praktik 2 " + tag},
		{e.wardRoom, "room", "{ward_room}", "Bangsal " + tag},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO core.location (id, company_id, merchant_id, kind, code, name, functions)
			VALUES ($1, $2, $3, $4, $5, $6, $7::text[])`,
			loc.id, e.companyID, e.merchantID, loc.kind, tag+"-"+loc.id[len(loc.id)-4:], loc.name, loc.fn); err != nil {
			t.Fatalf("seed location: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}

	e.token = e.jwt(t, userID, tag+".admin")
	e.noPermToken = e.jwt(t, noPermUserID, tag+".viewer")
	return e
}

func (e *scheduleTestEnv) jwt(t *testing.T, userID, name string) string {
	t.Helper()
	token, err := auth.GenerateToken(testJWTSecret, userID, e.companyID, e.merchantID, "", name, "sched-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return token
}

// do performs one authenticated request as the admin (or the given token).
func (e *scheduleTestEnv) do(t *testing.T, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Company-ID", e.companyID)
	req.Header.Set("X-Merchant-ID", e.merchantID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// createSchedule POSTs a pattern and returns the created id.
func (e *scheduleTestEnv) createSchedule(t *testing.T, body map[string]any) (string, *httptest.ResponseRecorder) {
	t.Helper()
	body["merchant_id"] = e.merchantID
	body["physician_id"] = e.physicianID
	body["department_id"] = e.departmentID
	rec := e.do(t, http.MethodPost, "/api/v1/physician-schedules", body, e.token)
	if rec.Code != http.StatusCreated {
		return "", rec
	}
	var got struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode create response: %v (%s)", err, rec.Body.String())
	}
	return got.Data.ID, rec
}

// calendarItems GETs the calendar and decodes the data array.
func (e *scheduleTestEnv) calendarItems(t *testing.T, from, to string) []map[string]any {
	t.Helper()
	rec := e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/merchants/%s/schedule/calendar?from=%s&to=%s", e.merchantID, from, to), nil, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("calendar: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode calendar: %v", err)
	}
	return got.Data
}

func findSession(items []map[string]any, date string) map[string]any {
	for _, it := range items {
		if it["date"] == date {
			return it
		}
	}
	return nil
}

// seedAdmission inserts one outpatient admission linked to the schedule.
func (e *scheduleTestEnv) seedAdmission(t *testing.T, ctx context.Context, visitNo, payerID, scheduleID, at string) {
	t.Helper()
	tx := locationTx(t, ctx, e.pool, e.companyID, e.merchantID)
	defer tx.Rollback(ctx)
	var payer any
	if payerID != "" {
		payer = payerID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operations.admission
		(company_id, merchant_id, visit_no, person_id, admission_type, department_id, primary_payer_id, schedule_id, admission_at)
		VALUES ($1, $2, $3, $4, 'outpatient', $5, $6, $7, $8)`,
		e.companyID, e.merchantID, visitNo, e.patientID, e.departmentID, payer, scheduleID, at); err != nil {
		t.Fatalf("seed admission %s: %v", visitNo, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit admission: %v", err)
	}
}

// TestScheduleCalendar_ProjectionAndOverride — a projected session appears
// with pattern values, a PUT override replaces it with a stored row
// (projected=false), and DELETE restores the projection.
func TestScheduleCalendar_ProjectionAndOverride(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '1', "CALP", "")

	scheduleID, rec := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "quota_jkn": 4, "effective_from": "2026-10-05",
		"room_id": e.practiceRoom,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create schedule: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	items := e.calendarItems(t, "2026-10-05", "2026-10-11")
	s := findSession(items, "2026-10-05")
	if s == nil {
		t.Fatalf("projected session for 2026-10-05 missing: %v", items)
	}
	if s["projected"] != true || s["start_time"] != "08:00" || s["quota_jkn"] != float64(4) {
		t.Fatalf("projected session wrong: %v", s)
	}
	if s["status"] != "available" {
		t.Fatalf("status = %v, want available", s["status"])
	}
	room, _ := s["room"].(map[string]any)
	if room == nil || room["id"] != e.practiceRoom || room["path"] != "Ruang Praktik CALP" {
		t.Fatalf("room = %v, want practice room with path", s["room"])
	}
	phys, _ := s["physician"].(map[string]any)
	if phys == nil || phys["sip_number"] != "SIP-CALP" {
		t.Fatalf("physician = %v, want sip_number", s["physician"])
	}
	if findSession(items, "2026-10-06") != nil {
		t.Fatalf("Tuesday must not project a Monday pattern")
	}

	// Override the session for that date.
	rec = e.do(t, http.MethodPut, fmt.Sprintf("/api/v1/schedule/sessions/%s/2026-10-05", scheduleID),
		map[string]any{"start_time": "09:00", "end_time": "15:00", "quota_jkn": 6, "notes": "ruang ganti"}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("override session: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	items = e.calendarItems(t, "2026-10-05", "2026-10-11")
	s = findSession(items, "2026-10-05")
	if s["projected"] != false || s["start_time"] != "09:00" || s["quota_jkn"] != float64(6) || s["notes"] != "ruang ganti" {
		t.Fatalf("stored override wrong: %v", s)
	}

	// Delete the override → back to the projection.
	rec = e.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/schedule/sessions/%s/2026-10-05", scheduleID), nil, e.token)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete override: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	items = e.calendarItems(t, "2026-10-05", "2026-10-11")
	s = findSession(items, "2026-10-05")
	if s["projected"] != true || s["start_time"] != "08:00" {
		t.Fatalf("projection not restored: %v", s)
	}
}

// TestScheduleCalendar_RegisteredPerPayer — admissions count separately for
// JKN (payer_type 'bpjs' primary payer) and other payers.
func TestScheduleCalendar_RegisteredPerPayer(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '2', "CALR", "")

	scheduleID, _ := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "quota_jkn": 5, "effective_from": "2026-10-05",
		"room_id": e.practiceRoom,
	})

	// 1 JKN (bpjs primary payer) + 1 non-JKN on 2026-10-05 (08:30 Jakarta =
	// 01:30 UTC).
	e.seedAdmission(t, ctx, "CALR-0001", e.payerBPJS, scheduleID, "2026-10-05T01:30:00+00:00")
	e.seedAdmission(t, ctx, "CALR-0002", e.payerPrivate, scheduleID, "2026-10-05T02:30:00+00:00")

	items := e.calendarItems(t, "2026-10-05", "2026-10-11")
	s := findSession(items, "2026-10-05")
	if s["registered_jkn"] != float64(1) || s["registered_other"] != float64(1) {
		t.Fatalf("registered = jkn %v other %v, want 1/1", s["registered_jkn"], s["registered_other"])
	}
}

// TestScheduleCalendar_StatusModes — combined mode: full at slot_quota,
// near_full at the threshold; split mode: JKN pool vs non-JKN pool.
func TestScheduleCalendar_StatusModes(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '3', "CALS", "")

	scheduleID, _ := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "quota_jkn": 0, "effective_from": "2026-10-05",
		"room_id": e.practiceRoom,
	})

	// Defaults when no flags exist: combined, 80, 30.
	rec := e.do(t, http.MethodGet, fmt.Sprintf("/api/v1/merchants/%s/schedule/settings", e.merchantID), nil, e.token)
	var settings struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get settings: %d %s", rec.Code, rec.Body.String())
	}
	if settings.Data["quota_mode"] != "combined" || settings.Data["near_full_threshold"] != float64(80) || settings.Data["contract_warning_days"] != float64(30) {
		t.Fatalf("default settings = %v", settings.Data)
	}
	rec = e.do(t, http.MethodPut, fmt.Sprintf("/api/v1/merchants/%s/schedule/settings", e.merchantID),
		map[string]any{"quota_mode": "combined", "near_full_threshold": 80, "contract_warning_days": 30}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings: %d %s", rec.Code, rec.Body.String())
	}

	// Combined: 9/10 = 90% >= 80% → near_full; 10/10 → full.
	for i := 1; i <= 9; i++ {
		payer := e.payerPrivate
		if i%2 == 0 {
			payer = e.payerBPJS
		}
		e.seedAdmission(t, ctx, fmt.Sprintf("CALS-%04d", i), payer, scheduleID, "2026-10-05T01:00:00+00:00")
	}
	s := findSession(e.calendarItems(t, "2026-10-05", "2026-10-11"), "2026-10-05")
	if s["status"] != "near_full" {
		t.Fatalf("combined 9/10 status = %v, want near_full", s["status"])
	}
	e.seedAdmission(t, ctx, "CALS-0010", e.payerPrivate, scheduleID, "2026-10-05T01:30:00+00:00")
	s = findSession(e.calendarItems(t, "2026-10-05", "2026-10-11"), "2026-10-05")
	if s["status"] != "full" {
		t.Fatalf("combined 10/10 status = %v, want full", s["status"])
	}

	// Split mode: second schedule on Tuesday, quota_jkn 5 + slot_quota 5.
	// 5/5 JKN + 0/5 others → near_full (JKN pool full); +5 others → full.
	splitID, _ := e.createSchedule(t, map[string]any{
		"day_of_week": 2, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 5, "quota_jkn": 5, "effective_from": "2026-10-06",
		"room_id": e.practiceRoom,
	})
	rec = e.do(t, http.MethodPut, fmt.Sprintf("/api/v1/merchants/%s/schedule/settings", e.merchantID),
		map[string]any{"quota_mode": "split", "near_full_threshold": 80, "contract_warning_days": 30}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings split: %d %s", rec.Code, rec.Body.String())
	}
	for i := 1; i <= 5; i++ {
		e.seedAdmission(t, ctx, fmt.Sprintf("CALS-J%03d", i), e.payerBPJS, splitID, "2026-10-06T01:00:00+00:00")
	}
	s = findSession(e.calendarItems(t, "2026-10-05", "2026-10-11"), "2026-10-06")
	if s["status"] != "near_full" {
		t.Fatalf("split 5/5 JKN + 0 others status = %v, want near_full", s["status"])
	}
	for i := 1; i <= 5; i++ {
		e.seedAdmission(t, ctx, fmt.Sprintf("CALS-O%03d", i), e.payerPrivate, splitID, "2026-10-06T02:00:00+00:00")
	}
	s = findSession(e.calendarItems(t, "2026-10-05", "2026-10-11"), "2026-10-06")
	if s["status"] != "full" {
		t.Fatalf("split 5/5 + 5/5 status = %v, want full", s["status"])
	}
}

// TestScheduleCalendar_PatternConflict409 — overlapping same-physician or
// same-room patterns are rejected with 409 + conflicts[].
func TestScheduleCalendar_PatternConflict409(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '4', "CALX", "")

	_, rec := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed schedule: %d %s", rec.Code, rec.Body.String())
	}

	var conflictResp struct {
		Error     string `json:"error"`
		Conflicts []struct {
			ScheduleID string `json:"schedule_id"`
			Reason     string `json:"reason"`
		} `json:"conflicts"`
	}

	// Same physician, overlapping hours → reason physician.
	_, rec = e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "10:00", "end_time": "16:00",
		"slot_quota": 5, "effective_from": "2026-10-05", "room_id": e.practiceRoom2,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("same physician overlap: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &conflictResp); err != nil || conflictResp.Error != "schedule overlaps another schedule" {
		t.Fatalf("conflict body = %s", rec.Body.String())
	}
	if len(conflictResp.Conflicts) != 1 || conflictResp.Conflicts[0].Reason != "physician" {
		t.Fatalf("conflicts = %v, want physician", conflictResp.Conflicts)
	}

	// Different physician, same room, overlapping → reason room.
	otherPerson := schedUUID('4', 20)
	e.pool.Exec(ctx, "INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, 'Dokter Lain', 'male')", otherPerson, e.companyID)
	otherPhysician := schedUUID('4', 21)
	e.pool.Exec(ctx, "INSERT INTO core.physician (id, company_id, merchant_id, person_id) VALUES ($1, $2, $3, $4)", otherPhysician, e.companyID, e.merchantID, otherPerson)
	origPhys := e.physicianID
	e.physicianID = otherPhysician
	_, rec = e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "09:00", "end_time": "10:00",
		"slot_quota": 5, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
	})
	e.physicianID = origPhys
	if rec.Code != http.StatusConflict {
		t.Fatalf("same room overlap: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &conflictResp); err != nil || len(conflictResp.Conflicts) != 1 || conflictResp.Conflicts[0].Reason != "room" {
		t.Fatalf("conflicts = %s, want room", rec.Body.String())
	}

	// Adjacent hours (14:00–16:00) don't overlap → 201.
	_, rec = e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "14:00", "end_time": "16:00",
		"slot_quota": 5, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("adjacent schedule: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestScheduleCalendar_SessionConflict409 — a session override overlapping
// another session that date is rejected, and a date off the pattern is 400.
func TestScheduleCalendar_SessionConflict409(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '5', "CALO", "")

	idA, _ := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "effective_from": "2026-10-05", "room_id": e.practiceRoom,
	})
	otherPerson := schedUUID('5', 20)
	e.pool.Exec(ctx, "INSERT INTO core.person (id, company_id, full_name, gender) VALUES ($1, $2, 'Dokter Dua', 'male')", otherPerson, e.companyID)
	otherPhysician := schedUUID('5', 21)
	e.pool.Exec(ctx, "INSERT INTO core.physician (id, company_id, merchant_id, person_id) VALUES ($1, $2, $3, $4)", otherPhysician, e.companyID, e.merchantID, otherPerson)
	origPhys := e.physicianID
	e.physicianID = otherPhysician
	idB, _ := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "14:00", "end_time": "16:00",
		"slot_quota": 5, "effective_from": "2026-10-05", "room_id": e.practiceRoom2,
	})
	e.physicianID = origPhys

	// Move B onto the same room/time window as A → 409.
	rec := e.do(t, http.MethodPut, fmt.Sprintf("/api/v1/schedule/sessions/%s/2026-10-05", idB),
		map[string]any{"room_id": e.practiceRoom, "start_time": "10:00", "end_time": "15:00"}, e.token)
	if rec.Code != http.StatusConflict {
		t.Fatalf("session override overlap: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// Override on a Tuesday for a Monday pattern → 400.
	rec = e.do(t, http.MethodPut, fmt.Sprintf("/api/v1/schedule/sessions/%s/2026-10-06", idA),
		map[string]any{"notes": "x"}, e.token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("override off-pattern date: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Non-overriding move stays fine (adjacent room, own window) → 200.
	rec = e.do(t, http.MethodPut, fmt.Sprintf("/api/v1/schedule/sessions/%s/2026-10-05", idA),
		map[string]any{"notes": "ok"}, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain override: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	_ = idA
}

// TestScheduleCalendar_NonPracticeRoom400 — room without the 'practice'
// function is rejected with 400 "room must be a practice room".
func TestScheduleCalendar_NonPracticeRoom400(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '6', "CALW", "")

	_, rec := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "effective_from": "2026-10-05", "room_id": e.wardRoom,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ward room: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error != "room must be a practice room" {
		t.Fatalf("error = %s", rec.Body.String())
	}
}

// TestScheduleCalendar_SipWarning — a pattern whose effective_to passes the
// physician's sip_valid_until carries the sip_expires_before_end warning.
func TestScheduleCalendar_SipWarning(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '7', "CALSI", "2026-12-31")

	_, rec := e.createSchedule(t, map[string]any{
		"day_of_week": 1, "start_time": "08:00", "end_time": "14:00",
		"slot_quota": 10, "effective_from": "2026-10-05", "effective_to": "2027-06-30",
		"room_id": e.practiceRoom,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode warnings: %v (%s)", err, rec.Body.String())
	}
	found := false
	for _, w := range got.Warnings {
		if w == "sip_expires_before_end" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want sip_expires_before_end", got.Warnings)
	}
}

// TestScheduleCalendar_RangeLimit400 — a range above 62 days is rejected.
func TestScheduleCalendar_RangeLimit400(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '8', "CALD", "")

	rec := e.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/merchants/%s/schedule/calendar?from=2026-01-01&to=2026-03-15", e.merchantID), nil, e.token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	// 62 days exactly (Jan 1 – Mar 3) is allowed.
	rec = e.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/merchants/%s/schedule/calendar?from=2026-01-01&to=2026-03-03", e.merchantID), nil, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("62-day range: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestScheduleCalendar_RequiresPermission — a merchant user without
// operations.schedule.manage gets 403 on the calendar.
func TestScheduleCalendar_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	e := newScheduleTestEnv(t, ctx, '9', "CALA", "")

	rec := e.do(t, http.MethodGet,
		fmt.Sprintf("/api/v1/merchants/%s/schedule/calendar?from=2026-10-05&to=2026-10-11", e.merchantID), nil, e.noPermToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}
