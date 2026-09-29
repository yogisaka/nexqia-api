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

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// auditQuerier is satisfied by both *pgxpool.Pool (owner role, bypasses RLS) and
// pgx.Tx (used inside TestAuditTrail_TriggerUnderAppRuntimeRole where the
// transaction runs as app_runtime).
type auditQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type auditLogRow struct {
	Action          string
	Fields          []string
	ChangedBy       string // "" = NULL
	PlatformAdminID string // "" = NULL
}

// auditRowsForRecord reads the audit_log rows for one record id. The test pool
// connects as the table owner (bootstrap superuser), so RLS on core.audit_log
// does not apply to these reads.
func auditRowsForRecord(t *testing.T, q auditQuerier, recordID string) []auditLogRow {
	t.Helper()
	rows, err := q.Query(context.Background(), `
		SELECT action, changed_fields, COALESCE(changed_by::text, ''), COALESCE(platform_admin_id::text, '')
		FROM core.audit_log
		WHERE record_id = $1
		ORDER BY changed_at`, recordID)
	if err != nil {
		t.Fatalf("query core.audit_log: %v", err)
	}
	defer rows.Close()
	out := []auditLogRow{}
	for rows.Next() {
		var r auditLogRow
		if err := rows.Scan(&r.Action, &r.Fields, &r.ChangedBy, &r.PlatformAdminID); err != nil {
			t.Fatalf("scan audit_log row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit_log rows: %v", err)
	}
	return out
}

// createPersonViaAPI creates a person through the real POST /persons endpoint
// (person_id is resolved from the DB afterwards — the response shape of
// pgtype.UUID fields is not this test's concern).
func createPersonViaAPI(t *testing.T, router *gin.Engine, token, companyID, merchantID, fullName, gender string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"full_name": fullName, "gender": gender})
	rec := accountRequest(router, http.MethodPost, "/api/v1/persons", token, companyID, merchantID, body)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create person expected 2xx, got %d: %s", rec.Code, rec.Body.String())
	}
	return rec
}

// latestPerson resolves the person just created for (companyID, fullName),
// returning its id and current medical_record_no — POST /persons auto-generates
// an MRN when absent, and PATCH /persons/:id is a full-form update that must
// echo it back or the MRN clearing gets recorded as a change.
func latestPerson(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, fullName string) (id, mrn string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		"SELECT id::text, COALESCE(medical_record_no, '') FROM core.person WHERE company_id = $1 AND full_name = $2 ORDER BY created_at DESC LIMIT 1",
		companyID, fullName).Scan(&id, &mrn); err != nil {
		t.Fatalf("resolve person: %v", err)
	}
	return id, mrn
}

func patchPersonBody(fullName, gender, mrn, phone string) []byte {
	b, _ := json.Marshal(map[string]string{"full_name": fullName, "gender": gender, "medical_record_no": mrn, "phone": phone})
	return b
}

// TestAuditTrail_PersonUpdateRecordsFieldNames — spec §3: a PATCH that changes
// only the phone records exactly one update row for core.person with
// changed_fields='{phone}', attributed to the caller; migration 000046 dropped
// old_value/new_value so no patient values are ever stored.
func TestAuditTrail_PersonUpdateRecordsFieldNames(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.owner", "081234570001")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit Person One", "female")
	personID, mrn := latestPerson(t, ctx, pool, companyID, "Audit Person One")

	rec := accountRequest(router, http.MethodPatch, "/api/v1/persons/"+personID, token, companyID, merchantID,
		patchPersonBody("Audit Person One", "female", mrn, "081234570002"))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch person expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rows := auditRowsForRecord(t, pool, personID)
	updates := 0
	for _, r := range rows {
		if r.Action != "update" {
			continue
		}
		updates++
		if len(r.Fields) != 1 || r.Fields[0] != "phone" {
			t.Fatalf("expected changed_fields = {phone}, got %v", r.Fields)
		}
		if r.ChangedBy != userID {
			t.Fatalf("expected changed_by %s, got %q", userID, r.ChangedBy)
		}
		if r.PlatformAdminID != "" {
			t.Fatalf("expected platform_admin_id NULL for an ordinary session, got %q", r.PlatformAdminID)
		}
	}
	if updates != 1 {
		t.Fatalf("expected exactly 1 update row, got %d", updates)
	}

	var legacyColumns int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'core' AND table_name = 'audit_log' AND column_name IN ('old_value','new_value')").Scan(&legacyColumns); err != nil {
		t.Fatalf("information_schema.columns check: %v", err)
	}
	if legacyColumns != 0 {
		t.Fatalf("core.audit_log must not have old_value/new_value columns, found %d", legacyColumns)
	}
}

// TestAuditTrail_NoOpUpdateNotRecorded — a PATCH with identical values must not
// produce a new update row (the trigger skips touch-only updates).
func TestAuditTrail_NoOpUpdateNotRecorded(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.noop", "081234570011")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit Person Two", "male")
	personID, mrn := latestPerson(t, ctx, pool, companyID, "Audit Person Two")

	// First PATCH actually changes the phone — this IS recorded.
	rec := accountRequest(router, http.MethodPatch, "/api/v1/persons/"+personID, token, companyID, merchantID,
		patchPersonBody("Audit Person Two", "male", mrn, "081234570012"))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch person expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if updates := countAuditUpdates(t, pool, personID); updates != 1 {
		t.Fatalf("expected 1 update row after the real change, got %d", updates)
	}

	// Identical PATCH — no new row.
	rec = accountRequest(router, http.MethodPatch, "/api/v1/persons/"+personID, token, companyID, merchantID,
		patchPersonBody("Audit Person Two", "male", mrn, "081234570012"))
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op patch expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if updates := countAuditUpdates(t, pool, personID); updates != 1 {
		t.Fatalf("no-op PATCH must not record a new update row, got %d", updates)
	}
}

func countAuditUpdates(t *testing.T, pool *pgxpool.Pool, recordID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM core.audit_log WHERE record_id = $1 AND action = 'update'", recordID).Scan(&n); err != nil {
		t.Fatalf("count update rows: %v", err)
	}
	return n
}

// TestAuditTrail_ImpersonationRecordsAdmin — the same PATCH performed through an
// impersonation token attributes the change to the platform admin
// (app.platform_admin_id set transaction-scoped by AuthMiddleware).
func TestAuditTrail_ImpersonationRecordsAdmin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.imp", "081234570021")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit Person Three", "female")
	personID, mrn := latestPerson(t, ctx, pool, companyID, "Audit Person Three")

	impToken := impersonationToken(t, tokenToUserID(t, pool, token), companyID, merchantID)
	rec := accountRequest(router, http.MethodPatch, "/api/v1/persons/"+personID, impToken, companyID, merchantID,
		patchPersonBody("Audit Person Three", "female", mrn, "081234570023"))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch person under impersonation expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rows := auditRowsForRecord(t, pool, personID)
	updates := 0
	for _, r := range rows {
		if r.Action != "update" {
			continue
		}
		updates++
		if r.PlatformAdminID != "00000000-0000-0000-0000-000000000001" {
			t.Fatalf("expected platform_admin_id = impersonating admin, got %q", r.PlatformAdminID)
		}
	}
	if updates == 0 {
		t.Fatalf("expected at least one update row after the impersonated PATCH")
	}
}

// tokenToUserID resolves the user id behind an access token (used to mint an
// impersonation token AS that user).
func tokenToUserID(t *testing.T, pool *pgxpool.Pool, token string) string {
	t.Helper()
	claims, err := auth.ParseToken(testJWTSecret, token)
	if err != nil {
		t.Fatalf("parse seeded token: %v", err)
	}
	return claims.UserID
}

// TestAuditTrail_SelfRegistrationStillWorks — self-registration has no user
// session, so the owner person's insert row must exist with changed_by NULL.
func TestAuditTrail_SelfRegistrationStillWorks(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload("audit.reg", "audit.reg@example.com", "081234570031")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "audit-trail-test-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			UserID string `json:"user_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}

	var personID string
	if err := pool.QueryRow(ctx, "SELECT person_id::text FROM core.app_user WHERE id = $1", resp.Data.UserID).Scan(&personID); err != nil {
		t.Fatalf("resolve owner person: %v", err)
	}
	rows := auditRowsForRecord(t, pool, personID)
	if len(rows) != 1 || rows[0].Action != "insert" {
		t.Fatalf("expected exactly 1 insert row for the owner person, got %+v", rows)
	}
	if rows[0].ChangedBy != "" {
		t.Fatalf("expected changed_by NULL for self-registration, got %q", rows[0].ChangedBy)
	}
}

// TestAuditTrail_TriggerUnderAppRuntimeRole — the test pool is the table owner
// and bypasses RLS, so prove the SECURITY DEFINER trigger + RLS explicitly:
// as app_runtime the insert/update are recorded, a user-less session records
// changed_by NULL, and a cross-tenant access_log insert is rejected.
func TestAuditTrail_TriggerUnderAppRuntimeRole(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "audit.role", "081234570041")

	// Transaction 1: app_runtime with company + user → insert + update recorded.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_user_id', $1, true)", userID); err != nil {
		t.Fatalf("set user GUC: %v", err)
	}
	var personID string
	if err := tx.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, 'Role Person One', 'male') RETURNING id::text",
		companyID).Scan(&personID); err != nil {
		t.Fatalf("insert person as app_runtime: %v", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE core.person SET phone = '081234570042' WHERE id = $1", personID); err != nil {
		t.Fatalf("update person as app_runtime: %v", err)
	}
	rows := auditRowsForRecord(t, tx, personID)
	if len(rows) != 2 {
		t.Fatalf("expected insert + update audit rows as app_runtime, got %+v", rows)
	}
	for _, r := range rows {
		if r.ChangedBy != userID {
			t.Fatalf("expected changed_by = %s on %s row, got %q", userID, r.Action, r.ChangedBy)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx 1: %v", err)
	}

	// Transaction 2: app_runtime with ONLY the company set (no user) → insert
	// still succeeds (SECURITY DEFINER) and the row has changed_by NULL.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx 2: %v", err)
	}
	defer tx2.Rollback(ctx)
	if _, err := tx2.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx2.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	var noUserPersonID string
	if err := tx2.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, 'Role Person No User', 'female') RETURNING id::text",
		companyID).Scan(&noUserPersonID); err != nil {
		t.Fatalf("user-less insert as app_runtime: %v", err)
	}
	noUserRows := auditRowsForRecord(t, tx2, noUserPersonID)
	if len(noUserRows) != 1 || noUserRows[0].Action != "insert" {
		t.Fatalf("expected 1 insert row for the user-less session, got %+v", noUserRows)
	}
	if noUserRows[0].ChangedBy != "" {
		t.Fatalf("expected changed_by NULL without a user session, got %q", noUserRows[0].ChangedBy)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx 2: %v", err)
	}

	// Transaction 3: app_runtime inserting an access_log row for ANOTHER company
	// must be rejected (RLS policy, app_runtime is NOBYPASSRLS).
	tx3, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx 3: %v", err)
	}
	defer tx3.Rollback(ctx)
	if _, err := tx3.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx3.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	otherCompanyID := "71717171-7171-7171-7171-717171717171"
	_, err = tx3.Exec(ctx,
		"INSERT INTO core.access_log (company_id, actor_id, resource, action) VALUES ($1, $2, 'person', 'view')",
		otherCompanyID, userID)
	if err == nil {
		t.Fatalf("cross-tenant access_log insert as app_runtime must fail, got no error")
	}
	if err := tx3.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx 3: %v", err)
	}
}

// TestAuditTrail_PlatformCompanyCreationRecordsAdmin — POST /platform/companies
// opens its own tx, so the platform_admin_id GUC is set by the handler itself;
// the owner person's insert row must carry that admin.
func TestAuditTrail_PlatformCompanyCreationRecordsAdmin(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	adminID := "61616161-6161-6161-6161-616161616161"
	roleID := "62626262-6262-6262-6262-626262626262"
	permID := "63636363-6363-6363-6363-636363636363"

	adminPasswordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.permission (id, code, description, module) VALUES ($1, 'platform.company.manage', 'test', 'platform') ON CONFLICT (id) DO NOTHING", permID); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role (id, name, is_system) VALUES ($1, 'Audit Create Co Admin', true)", roleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.role_permission (role_id, permission_id) VALUES ($1, $2)", roleID, permID); err != nil {
		t.Fatalf("seed role_permission: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user (id, username, email, full_name, password_hash, is_active) VALUES ($1, 'auditcoadmin', 'auditcoadmin@nexqia.internal', 'Test', $2, true)", adminID, adminPasswordHash); err != nil {
		t.Fatalf("seed admin_user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO platform.admin_user_role (admin_user_id, role_id) VALUES ($1, $2)", adminID, roleID); err != nil {
		t.Fatalf("seed admin_user_role: %v", err)
	}

	adminToken, err := auth.GeneratePlatformAdminToken(testJWTSecret, adminID, "auditcoadmin", "audit-co-device", time.Hour)
	if err != nil {
		t.Fatalf("generate platform admin token: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"company_name": "Audit Onboard Co",
		"full_name":    "Platform Owner Person",
		"username":     "audit.newowner",
		"email":        "audit.newowner@example.com",
		"phone":        "081234570043",
		"password":     "Passw0rd123!",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platform/companies", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create platform company expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			UserID string `json:"user_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var personID string
	if err := pool.QueryRow(ctx, "SELECT person_id::text FROM core.app_user WHERE id = $1", resp.Data.UserID).Scan(&personID); err != nil {
		t.Fatalf("resolve owner person: %v", err)
	}
	rows := auditRowsForRecord(t, pool, personID)
	if len(rows) != 1 || rows[0].Action != "insert" {
		t.Fatalf("expected exactly 1 insert row for the owner person, got %+v", rows)
	}
	if rows[0].PlatformAdminID != adminID {
		t.Fatalf("expected platform_admin_id = %s, got %q", adminID, rows[0].PlatformAdminID)
	}
}

// TestAuditTrail_GuarantorUsesSessionCompany — operations.admission_guarantor
// has no company_id of its own; the trigger must fall back to the session GUC
// (app.current_company_id) set by TenantMiddleware for the caller.
func TestAuditTrail_GuarantorUsesSessionCompany(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.guarantor", "081234570051")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit Guarantor Person", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Audit Guarantor Person")

	departmentID := "64646464-6464-6464-6464-646464646464"
	payerID := "65656565-6565-6565-6565-656565656565"
	if _, err := pool.Exec(ctx, "INSERT INTO core.department (id, company_id, merchant_id, code, name) VALUES ($1, $2, $3, 'AUDT01', 'Poli Audit')", departmentID, companyID, merchantID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.payer (id, company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, 'AUDT-PAY', 'Audit Payer', 'self_pay')", payerID, companyID, merchantID); err != nil {
		t.Fatalf("seed payer: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"person_id":        personID,
		"department_id":    departmentID,
		"primary_payer_id": payerID,
		"guarantor_name":   "PT Audit Guarantor",
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/admissions", token, companyID, merchantID, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create admission expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var admissionID, guarantorID string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM operations.admission WHERE person_id = $1 AND company_id = $2 ORDER BY created_at DESC LIMIT 1", personID, companyID).Scan(&admissionID); err != nil {
		t.Fatalf("resolve admission: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT id::text FROM operations.admission_guarantor WHERE admission_id = $1", admissionID).Scan(&guarantorID); err != nil {
		t.Fatalf("resolve admission_guarantor: %v", err)
	}

	var rowCompany string
	var rowAction string
	if err := pool.QueryRow(ctx,
		"SELECT company_id::text, action FROM core.audit_log WHERE table_name = 'operations.admission_guarantor' AND record_id = $1",
		guarantorID).Scan(&rowCompany, &rowAction); err != nil {
		t.Fatalf("query guarantor audit row: %v", err)
	}
	if rowAction != "insert" {
		t.Fatalf("expected action insert, got %q", rowAction)
	}
	if rowCompany != companyID {
		t.Fatalf("expected guarantor audit row company %s (caller's company), got %q", companyID, rowCompany)
	}
}
