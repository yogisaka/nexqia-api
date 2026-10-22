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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// withPersonEnv holds the seeded identifiers for one test.
type withPersonEnv struct {
	router     http.Handler
	pool       *pgxpool.Pool
	ctx        context.Context
	companyID  string
	merchantID string
	token      string
}

// seedWithPersonEnv seeds company/merchant/role (+ core.user.manage when
// withPermission)/app_user/user_merchant_role following the pattern of
// user_conflict_test.go:29-73 — WITHOUT a person row (the endpoint under test
// creates it). ids must contain company/merchant/admin/role unique UUIDs.
func seedWithPersonEnv(t *testing.T, ids map[string]string, withPermission bool) *withPersonEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	// The endpoint encrypts/hashes NIK via pgcrypto with config keys — reuse
	// the same test keys as person_merge_test.go:41-42 (testConfig leaves them
	// empty, which pgcrypto rejects).
	cfg := testConfig()
	cfg.NIKEncryptionKey = "test-nik-key"
	cfg.NIKSearchKey = "test-search-key"
	router := server.NewRouter(pool, redisClient, cfg)

	companyID := ids["company"]
	merchantID := ids["merchant"]
	adminID := ids["admin"]
	roleID := ids["role"]

	if _, err := pool.Exec(ctx, "INSERT INTO core.company (id, code, name) VALUES ($1, $2, 'With-Person Co')", companyID, ids["code"]); err != nil {
		t.Fatalf("failed to seed company: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.merchant (id, company_id, code, name) VALUES ($1, $2, $3, 'With-Person Merchant')", merchantID, companyID, ids["mcode"]); err != nil {
		t.Fatalf("failed to seed merchant: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'With-Person Admin')", roleID, companyID); err != nil {
		t.Fatalf("failed to seed role: %v", err)
	}
	if withPermission {
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
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, true)", adminID, companyID, ids["admin-username"], passwordHash); err != nil {
		t.Fatalf("failed to seed app_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", adminID, merchantID, roleID); err != nil {
		t.Fatalf("failed to seed user_merchant_role: %v", err)
	}
	token, err := auth.GenerateToken(testJWTSecret, adminID, companyID, merchantID, "", ids["admin-username"], "with-person-device", time.Hour)
	if err != nil {
		t.Fatalf("failed to generate test token: %v", err)
	}
	return &withPersonEnv{router: router, pool: pool, ctx: ctx, companyID: companyID, merchantID: merchantID, token: token}
}

// postWithPerson posts a payload to POST /api/v1/users/with-person with the
// env's token and company/merchant headers.
func (env *withPersonEnv) postWithPerson(t *testing.T, payload map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/with-person", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("X-Company-ID", env.companyID)
	req.Header.Set("X-Merchant-ID", env.merchantID)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func withPersonPayload(companyID, fullName, username string) map[string]string {
	return map[string]string{
		"company_id": companyID,
		"full_name":  fullName,
		"nik":        "3201010101010001",
		"birth_date": "1990-05-10",
		"gender":     "male",
		"phone":      "08123456789",
		"email":      "budi@rsxia.id",
		"address":    "Jl. Melati 12, Bandung",
		"username":   username,
		"password":   "Passw0rd123!",
	}
}

// TestCreateAppUserWithPerson_Success — full payload → 201, PersonID set,
// PasswordHash blank, person has NULL MRN, non-null nik_search_hash, zero
// person_consent rows, and the app_user is linked to the created person.
func TestCreateAppUserWithPerson_Success(t *testing.T) {
	env := seedWithPersonEnv(t, map[string]string{
		"company":  "a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1",
		"merchant": "a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a2",
		"admin":    "a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a3",
		"role":     "a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a4",
		"code":     "wp-co-1", "mcode": "wp-m-1", "admin-username": "wp.admin1",
	}, true)

	rec := env.postWithPerson(t, withPersonPayload(env.companyID, "Dr. Budi Santoso", "budi.santoso"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			PersonID     string `json:"PersonID"`
			Username     string `json:"Username"`
			PasswordHash string `json:"PasswordHash"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.PersonID == "" {
		t.Fatalf("expected non-empty data.PersonID, got %s", rec.Body.String())
	}
	if resp.Data.Username != "budi.santoso" {
		t.Fatalf("expected Username budi.santoso, got %q", resp.Data.Username)
	}
	if resp.Data.PasswordHash != "" {
		t.Fatalf("expected empty PasswordHash in response, got %q", resp.Data.PasswordHash)
	}

	var mrn, nikHash *string
	if err := env.pool.QueryRow(env.ctx,
		"SELECT medical_record_no, nik_search_hash FROM core.person WHERE id = $1", resp.Data.PersonID,
	).Scan(&mrn, &nikHash); err != nil {
		t.Fatalf("query person: %v", err)
	}
	if mrn != nil {
		t.Fatalf("expected medical_record_no NULL, got %q", *mrn)
	}
	if nikHash == nil {
		t.Fatalf("expected nik_search_hash NOT NULL")
	}
	var consents int
	if err := env.pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM core.person_consent WHERE person_id = $1", resp.Data.PersonID,
	).Scan(&consents); err != nil {
		t.Fatalf("query person_consent: %v", err)
	}
	if consents != 0 {
		t.Fatalf("expected 0 person_consent rows, got %d", consents)
	}
	var linkedPersonID string
	if err := env.pool.QueryRow(env.ctx,
		"SELECT person_id FROM core.app_user WHERE company_id = $1 AND username = 'budi.santoso'", env.companyID,
	).Scan(&linkedPersonID); err != nil {
		t.Fatalf("query app_user: %v", err)
	}
	if linkedPersonID != resp.Data.PersonID {
		t.Fatalf("app_user person_id %s != created person %s", linkedPersonID, resp.Data.PersonID)
	}
}

// TestCreateAppUserWithPerson_ConflictRollbackAtomic — duplicate username must
// return 409 and the person written in step 9 must be rolled back with the
// aborted transaction (COUNT(core.person WHERE full_name = second name) = 0).
func TestCreateAppUserWithPerson_ConflictRollbackAtomic(t *testing.T) {
	env := seedWithPersonEnv(t, map[string]string{
		"company":  "b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b1",
		"merchant": "b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2",
		"admin":    "b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b3",
		"role":     "b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b4",
		"code":     "wp-co-2", "mcode": "wp-m-2", "admin-username": "wp.admin2",
	}, true)

	if rec := env.postWithPerson(t, withPersonPayload(env.companyID, "Person Pertama", "dup.user")); rec.Code != http.StatusCreated {
		t.Fatalf("first create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	// Same username, different full_name (and a different NIK so the username
	// constraint is the only one violated).
	payload := withPersonPayload(env.companyID, "Person Kedua", "dup.user")
	payload["nik"] = "3201010101010002"
	rec2 := env.postWithPerson(t, payload)
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
	var persons int
	if err := env.pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM core.person WHERE full_name = 'Person Kedua'",
	).Scan(&persons); err != nil {
		t.Fatalf("count persons: %v", err)
	}
	if persons != 0 {
		t.Fatalf("expected 0 person rows for rolled-back person, got %d", persons)
	}
}

// TestCreateAppUserWithPerson_NIKConflict — same NIK in a second payload must
// yield 409 "nik already registered" and neither the second person nor the
// second app_user may survive.
func TestCreateAppUserWithPerson_NIKConflict(t *testing.T) {
	env := seedWithPersonEnv(t, map[string]string{
		"company":  "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c1",
		"merchant": "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c2",
		"admin":    "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3",
		"role":     "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c4",
		"code":     "wp-co-3", "mcode": "wp-m-3", "admin-username": "wp.admin3",
	}, true)

	if rec := env.postWithPerson(t, withPersonPayload(env.companyID, "NIK Person Satu", "nik.user1")); rec.Code != http.StatusCreated {
		t.Fatalf("first create expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	payload := withPersonPayload(env.companyID, "NIK Person Dua", "nik.user2")
	rec2 := env.postWithPerson(t, payload)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate NIK expected 409, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "nik already registered" {
		t.Fatalf("expected NIK conflict message, got %q", resp.Error)
	}
	var persons int
	if err := env.pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM core.person WHERE full_name = 'NIK Person Dua'",
	).Scan(&persons); err != nil {
		t.Fatalf("count persons: %v", err)
	}
	if persons != 0 {
		t.Fatalf("expected 0 person rows for rolled-back NIK conflict, got %d", persons)
	}
	var users int
	if err := env.pool.QueryRow(env.ctx,
		"SELECT COUNT(*) FROM core.app_user WHERE username = 'nik.user2'",
	).Scan(&users); err != nil {
		t.Fatalf("count app_users: %v", err)
	}
	if users != 0 {
		t.Fatalf("expected 0 app_user rows for rolled-back NIK conflict, got %d", users)
	}
}

// TestCreateAppUserWithPerson_Validation — per-field validation failures and
// the birth_date format error (spec §Desain API steps 5–6).
func TestCreateAppUserWithPerson_Validation(t *testing.T) {
	env := seedWithPersonEnv(t, map[string]string{
		"company":  "d4d4d4d4-d4d4-d4d4-d4d4-d4d4d4d4d4d1",
		"merchant": "d4d4d4d4-d4d4-d4d4-d4d4-d4d4d4d4d4d2",
		"admin":    "d4d4d4d4-d4d4-d4d4-d4d4-d4d4d4d4d4d3",
		"role":     "d4d4d4d4-d4d4-d4d4-d4d4-d4d4d4d4d4d4",
		"code":     "wp-co-4", "mcode": "wp-m-4", "admin-username": "wp.admin4",
	}, true)

	base := func() map[string]string {
		p := withPersonPayload(env.companyID, "Valid Person", "valid.user")
		delete(p, "password")
		p["password"] = "Passw0rd123!"
		return p
	}

	// gender "x" → validation_failed, fields.gender.
	p := base()
	p["gender"] = "x"
	rec := env.postWithPerson(t, p)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid gender expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "validation_failed" || resp.Fields["gender"] != "Jenis kelamin tidak valid" {
		t.Fatalf("expected gender validation_failed, got %s", rec.Body.String())
	}

	// NIK "123" → fields.nik.
	p = base()
	p["nik"] = "123"
	rec = env.postWithPerson(t, p)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid nik expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	resp = struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "validation_failed" || resp.Fields["nik"] != "NIK harus 16 digit angka" {
		t.Fatalf("expected nik validation_failed, got %s", rec.Body.String())
	}

	// birth_date wrong format → 400 invalid birth_date.
	p = base()
	p["birth_date"] = "1990/05/10"
	rec = env.postWithPerson(t, p)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid birth_date expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var bdResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bdResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if bdResp.Error != "invalid birth_date, expected YYYY-MM-DD" {
		t.Fatalf("expected birth_date format error, got %q", bdResp.Error)
	}

	// 7-character password → 400 (binding min=8).
	p = base()
	p["password"] = "Passw0r"
	if rec := env.postWithPerson(t, p); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Empty full_name → 400 (binding required).
	p = base()
	p["full_name"] = ""
	if rec := env.postWithPerson(t, p); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty full_name expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateAppUserWithPerson_Forbidden — a company_id other than the token's
// company → 403; a role without core.user.manage → 403 (RequirePermission shape).
func TestCreateAppUserWithPerson_Forbidden(t *testing.T) {
	env := seedWithPersonEnv(t, map[string]string{
		"company":  "e5e5e5e5-e5e5-e5e5-e5e5-e5e5e5e5e5e1",
		"merchant": "e5e5e5e5-e5e5-e5e5-e5e5-e5e5e5e5e5e2",
		"admin":    "e5e5e5e5-e5e5-e5e5-e5e5-e5e5e5e5e5e3",
		"role":     "e5e5e5e5-e5e5-e5e5-e5e5-e5e5e5e5e5e4",
		"code":     "wp-co-5", "mcode": "wp-m-5", "admin-username": "wp.admin5",
	}, true)

	// Other-company UUID → 403 with the exact message.
	otherCompany := "f6f6f6f6-f6f6-f6f6-f6f6-f6f6f6f6f6f6"
	rec := env.postWithPerson(t, withPersonPayload(otherCompany, "Cross Tenant", "cross.user"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-company expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "cannot create user outside your own company" {
		t.Fatalf("expected cross-company message, got %q", resp.Error)
	}

	// Second env whose role lacks core.user.manage.
	env2 := seedWithPersonEnv(t, map[string]string{
		"company":  "a7a7a7a7-a7a7-a7a7-a7a7-a7a7a7a7a7a1",
		"merchant": "a7a7a7a7-a7a7-a7a7-a7a7-a7a7a7a7a7a2",
		"admin":    "a7a7a7a7-a7a7-a7a7-a7a7-a7a7a7a7a7a3",
		"role":     "a7a7a7a7-a7a7-a7a7-a7a7-a7a7a7a7a7a4",
		"code":     "wp-co-6", "mcode": "wp-m-6", "admin-username": "wp.admin6",
	}, false)
	rec2 := env2.postWithPerson(t, withPersonPayload(env2.companyID, "No Perm Person", "no.perm.user"))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("missing permission expected 403, got %d: %s", rec2.Code, rec2.Body.String())
	}
}
