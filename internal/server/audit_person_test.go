//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// assertAccessLogPerson reads the newest access_log row for (resource, resourceID)
// and requires its person_id to equal wantPersonID. The test pool is the table
// owner, so RLS on core.access_log does not apply to these reads.
func assertAccessLogPerson(t *testing.T, pool *pgxpool.Pool, resource, resourceID, wantPersonID string) {
	t.Helper()
	var got string
	err := pool.QueryRow(context.Background(),
		"SELECT COALESCE(person_id::text, '') FROM core.access_log WHERE resource = $1 AND resource_id = $2 ORDER BY created_at DESC LIMIT 1",
		resource, resourceID).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no access_log row for %s %s", resource, resourceID)
	}
	if err != nil {
		t.Fatalf("query access_log %s: %v", resource, err)
	}
	if got != wantPersonID {
		t.Fatalf("access_log %s %s: expected person_id %s, got %q", resource, resourceID, wantPersonID, got)
	}
}

// assertAuditLogPerson reads the newest audit_log row for (table, action, record)
// and requires its person_id to equal wantPersonID.
func assertAuditLogPerson(t *testing.T, pool *pgxpool.Pool, tableName, action, recordID, wantPersonID string) {
	t.Helper()
	var got string
	err := pool.QueryRow(context.Background(),
		"SELECT COALESCE(person_id::text, '') FROM core.audit_log WHERE table_name = $1 AND action = $2 AND record_id = $3 ORDER BY changed_at DESC LIMIT 1",
		tableName, action, recordID).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no audit_log %s row for %s", action, recordID)
	}
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if got != wantPersonID {
		t.Fatalf("audit_log %s %s %s: expected person_id %s, got %q", tableName, action, recordID, wantPersonID, got)
	}
}

// seedAuditPersonAdmission seeds department + payer (TestAuditTrail_GuarantorUsesSessionCompany
// pattern) and registers an admission through POST /admissions, which also creates
// its pendaftaran queue row. Returns the admission and queue ids; when no queue
// row exists, one is inserted directly (000019 NOT NULL columns).
func seedAuditPersonAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, token, companyID, merchantID, personID, label string) (admissionID, queueID string) {
	t.Helper()
	var departmentID, payerID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, $3, $3) RETURNING id::text",
		companyID, merchantID, "APSD-"+label).Scan(&departmentID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, $3, 'self_pay') RETURNING id::text",
		companyID, merchantID, "APSP-"+label).Scan(&payerID); err != nil {
		t.Fatalf("seed payer: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"person_id":        personID,
		"department_id":    departmentID,
		"primary_payer_id": payerID,
		"guarantor_name":   "PT Audit " + label,
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/admissions", token, companyID, merchantID, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create admission expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := pool.QueryRow(ctx,
		"SELECT id::text FROM operations.admission WHERE person_id = $1 AND company_id = $2 ORDER BY created_at DESC LIMIT 1",
		personID, companyID).Scan(&admissionID); err != nil {
		t.Fatalf("resolve admission: %v", err)
	}

	err := pool.QueryRow(ctx,
		"SELECT id::text FROM operations.queue WHERE admission_id = $1 ORDER BY created_at DESC LIMIT 1",
		admissionID).Scan(&queueID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := pool.QueryRow(ctx,
			"INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number) VALUES ($1, $2, 'pendaftaran', $3, $4, $5, $6) RETURNING id::text",
			companyID, merchantID, departmentID, personID, admissionID, "APSQ-"+label+"-001").Scan(&queueID); err != nil {
			t.Fatalf("seed queue: %v", err)
		}
		return admissionID, queueID
	}
	if err != nil {
		t.Fatalf("resolve queue: %v", err)
	}
	return admissionID, queueID
}

// TestAuditPerson_AccessFillsPersonFromResource — spec §3: GET /persons/:id,
// /persons/:id/families, /persons/:id/allergies, /admissions/:id and /queue/:id
// each leave one access_log row whose person_id is the patient, filled by the
// trg_access_log_person trigger from the resource.
func TestAuditPerson_AccessFillsPersonFromResource(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.person.access", "081234570101")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit Person Access", "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Audit Person Access")

	admissionID, queueID := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "access")

	for _, path := range []string{
		"/api/v1/persons/" + personID,
		"/api/v1/persons/" + personID + "/families",
		"/api/v1/persons/" + personID + "/allergies",
		"/api/v1/admissions/" + admissionID,
		"/api/v1/queue/" + queueID,
	} {
		rec := accountRequest(router, http.MethodGet, path, token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s expected 200, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}

	assertAccessLogPerson(t, pool, "person", personID, personID)
	assertAccessLogPerson(t, pool, "person_family", personID, personID)
	assertAccessLogPerson(t, pool, "patient_allergy", personID, personID)
	assertAccessLogPerson(t, pool, "admission", admissionID, personID)
	assertAccessLogPerson(t, pool, "queue", queueID, personID)
}

// TestAuditPerson_ChangeFillsPerson — the trg_audit_row recorder stores the
// patient behind the change: a PATCH person records core.person, an admission
// with guarantor_name records operations.admission and
// operations.admission_guarantor, all with person_id = pasien.
func TestAuditPerson_ChangeFillsPerson(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.person.change", "081234570111")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit Person Change", "female")
	personID, mrn := latestPerson(t, ctx, pool, companyID, "Audit Person Change")

	rec := accountRequest(router, http.MethodPatch, "/api/v1/persons/"+personID, token, companyID, merchantID,
		patchPersonBody("Audit Person Change", "female", mrn, "081234570112"))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch person expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	admissionID, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "change")
	var guarantorID string
	if err := pool.QueryRow(ctx,
		"SELECT id::text FROM operations.admission_guarantor WHERE admission_id = $1", admissionID).Scan(&guarantorID); err != nil {
		t.Fatalf("resolve guarantor: %v", err)
	}

	assertAuditLogPerson(t, pool, "core.person", "update", personID, personID)
	assertAuditLogPerson(t, pool, "operations.admission", "insert", admissionID, personID)
	assertAuditLogPerson(t, pool, "operations.admission_guarantor", "insert", guarantorID, personID)
}

// TestAuditPerson_ExplicitPersonNotOverwritten — when the app passes person_id
// itself, the access-log trigger must keep that value instead of overwriting it
// from the resource lookup.
func TestAuditPerson_ExplicitPersonNotOverwritten(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.person.explicit", "081234570121")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit Person Explicit", "male")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Audit Person Explicit")
	admissionID, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "explicit")

	otherPersonID := "89898989-8989-8989-8989-898989898989"
	if _, err := pool.Exec(ctx,
		"INSERT INTO core.access_log (company_id, actor_id, resource, resource_id, person_id, action) VALUES ($1, $2, 'admission', $3, $4, 'view')",
		companyID, userID, admissionID, otherPersonID); err != nil {
		t.Fatalf("insert explicit access_log: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM core.access_log WHERE resource = 'admission' AND resource_id = $1", admissionID).Scan(&n); err != nil {
		t.Fatalf("count access_log rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 access_log row, got %d", n)
	}
	assertAccessLogPerson(t, pool, "admission", admissionID, otherPersonID)
}

// TestAuditPerson_AccessTriggerUnderAppRuntime — the access-log trigger runs as
// the caller (not SECURITY DEFINER): under app_runtime with the right merchant
// GUC the admission lookup fills person_id; with a foreign merchant GUC the
// session cannot see the admission row and person_id stays NULL.
func TestAuditPerson_AccessTriggerUnderAppRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, userID, token, _ := seedAccountOwner(t, ctx, pool, router, "audit.person.runtime", "081234570131")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Audit Person Runtime", "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Audit Person Runtime")
	admissionID, _ := seedAuditPersonAdmission(t, ctx, pool, router, token, companyID, merchantID, personID, "runtime")

	// Transaction 1: app_runtime with company + merchant → person_id filled.
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
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchantID); err != nil {
		t.Fatalf("set merchant GUC: %v", err)
	}
	var filled string
	if err := tx.QueryRow(ctx,
		"INSERT INTO core.access_log (company_id, merchant_id, actor_id, resource, resource_id, action) VALUES ($1, $2, $3, 'admission', $4, 'view') RETURNING COALESCE(person_id::text, '')",
		companyID, merchantID, userID, admissionID).Scan(&filled); err != nil {
		t.Fatalf("insert access_log as app_runtime: %v", err)
	}
	if filled != personID {
		t.Fatalf("expected person_id %s under app_runtime, got %q", personID, filled)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx 1: %v", err)
	}

	// Transaction 2: app_runtime with a foreign merchant GUC → the admission row
	// is invisible to the session, the trigger leaves person_id NULL.
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
	if _, err := tx2.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", "99999999-9999-9999-9999-999999999999"); err != nil {
		t.Fatalf("set foreign merchant GUC: %v", err)
	}
	var empty string
	if err := tx2.QueryRow(ctx,
		"INSERT INTO core.access_log (company_id, actor_id, resource, resource_id, action) VALUES ($1, $2, 'admission', $3, 'view') RETURNING COALESCE(person_id::text, '')",
		companyID, userID, admissionID).Scan(&empty); err != nil {
		t.Fatalf("insert access_log as app_runtime with foreign merchant: %v", err)
	}
	if empty != "" {
		t.Fatalf("expected person_id NULL with a foreign merchant GUC, got %q", empty)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx 2: %v", err)
	}
}
