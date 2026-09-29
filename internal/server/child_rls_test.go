//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// childRLSTenant holds the ids of one seeded tenant's company + merchant +
// department + payer + person + admission + queue chain (owner pool, so no RLS
// applies to the seed itself).
type childRLSTenant struct {
	companyID   string
	merchantID  string
	departmentID string
	payerID     string
	personID    string
	admissionID string
	queueID     string
}

// childRLSChangedBy is a filler uuid for NOT NULL actor columns that carry no
// foreign key (queue_status_history.changed_by, person_merge_log.merged_by).
const childRLSChangedBy = "00000000-0000-0000-0000-000000000001"

// seedChildRLSTenant seeds one full tenant chain with plain SQL as the owner
// pool — same shape as the seeds in audit_trail_test.go (department/payer) and
// setupPinLockTestUser (company/merchant). Every id is returned from the DB so
// no uuid has to be invented here.
func seedChildRLSTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label string) childRLSTenant {
	t.Helper()
	var tn childRLSTenant
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.company (code, name, max_concurrent_sessions) VALUES ($1, $2, 10) RETURNING id::text",
		"CHLD-"+label, "Child RLS Co "+label).Scan(&tn.companyID); err != nil {
		t.Fatalf("seed company %s: %v", label, err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.merchant (company_id, code, name) VALUES ($1, $2, $3) RETURNING id::text",
		tn.companyID, "child-"+strings.ToLower(label), "Child Merchant "+label).Scan(&tn.merchantID); err != nil {
		t.Fatalf("seed merchant %s: %v", label, err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, $3, $4) RETURNING id::text",
		tn.companyID, tn.merchantID, "CHLD-"+label, "Poli Child "+label).Scan(&tn.departmentID); err != nil {
		t.Fatalf("seed department %s: %v", label, err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, $4, 'self_pay') RETURNING id::text",
		tn.companyID, tn.merchantID, "CHLD-PAY-"+label, "Child Payer "+label).Scan(&tn.payerID); err != nil {
		t.Fatalf("seed payer %s: %v", label, err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, $2, 'male') RETURNING id::text",
		tn.companyID, "Child RLS Person "+label).Scan(&tn.personID); err != nil {
		t.Fatalf("seed person %s: %v", label, err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, primary_payer_id) VALUES ($1, $2, $3, $4, 'outpatient', $5, $6) RETURNING id::text",
		tn.companyID, tn.merchantID, "CHLD-"+label+"-1", tn.personID, tn.departmentID, tn.payerID).Scan(&tn.admissionID); err != nil {
		t.Fatalf("seed admission %s: %v", label, err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number) VALUES ($1, $2, 'pendaftaran', $3, $4, $5, $6) RETURNING id::text",
		tn.companyID, tn.merchantID, tn.departmentID, tn.personID, tn.admissionID, "CHLD-"+label+"-001").Scan(&tn.queueID); err != nil {
		t.Fatalf("seed queue %s: %v", label, err)
	}
	return tn
}

// childRLSAppRuntimeTx opens a transaction running as app_runtime with the
// given tenant's company/merchant GUCs — the same pattern as
// TestAuditTrail_TriggerUnderAppRuntimeRole (the owner pool bypasses RLS, so
// RLS behaviour is only observable through this role switch).
func childRLSAppRuntimeTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID string) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchantID); err != nil {
		t.Fatalf("set merchant GUC: %v", err)
	}
	return tx
}

// TestChildRLS_FillFromParent — the BEFORE trigger fills company_id/merchant_id
// from the parent row on every insert, overwriting anything the caller sent.
func TestChildRLS_FillFromParent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "A")

	var companyID, merchantID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO operations.queue_status_history (queue_id, to_status, changed_by) VALUES ($1, 'called', $2) RETURNING company_id::text, merchant_id::text",
		tn.queueID, childRLSChangedBy).Scan(&companyID, &merchantID); err != nil {
		t.Fatalf("insert history without tenant columns: %v", err)
	}
	if companyID != tn.companyID || merchantID != tn.merchantID {
		t.Fatalf("history without tenant columns: expected company %s merchant %s, got %s/%s", tn.companyID, tn.merchantID, companyID, merchantID)
	}

	if err := pool.QueryRow(ctx,
		"INSERT INTO operations.admission_guarantor (admission_id, payer_id, sequence) VALUES ($1, $2, 1) RETURNING company_id::text, merchant_id::text",
		tn.admissionID, tn.payerID).Scan(&companyID, &merchantID); err != nil {
		t.Fatalf("insert guarantor without tenant columns: %v", err)
	}
	if companyID != tn.companyID || merchantID != tn.merchantID {
		t.Fatalf("guarantor without tenant columns: expected company %s merchant %s, got %s/%s", tn.companyID, tn.merchantID, companyID, merchantID)
	}

	// Explicit wrong tenant columns are overwritten by the parent's values.
	wrongCompany := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	wrongMerchant := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if err := pool.QueryRow(ctx,
		"INSERT INTO operations.queue_status_history (queue_id, to_status, changed_by, company_id, merchant_id) VALUES ($1, 'called', $2, $3, $4) RETURNING company_id::text, merchant_id::text",
		tn.queueID, childRLSChangedBy, wrongCompany, wrongMerchant).Scan(&companyID, &merchantID); err != nil {
		t.Fatalf("insert history with wrong tenant columns: %v", err)
	}
	if companyID != tn.companyID || merchantID != tn.merchantID {
		t.Fatalf("explicit wrong tenant columns must be overwritten: expected %s/%s, got %s/%s", tn.companyID, tn.merchantID, companyID, merchantID)
	}

	if err := pool.QueryRow(ctx,
		"INSERT INTO operations.admission_guarantor (admission_id, payer_id, sequence, company_id, merchant_id) VALUES ($1, $2, 2, $3, $4) RETURNING company_id::text, merchant_id::text",
		tn.admissionID, tn.payerID, wrongCompany, wrongMerchant).Scan(&companyID, &merchantID); err != nil {
		t.Fatalf("insert guarantor with wrong tenant columns: %v", err)
	}
	if companyID != tn.companyID || merchantID != tn.merchantID {
		t.Fatalf("explicit wrong guarantor tenant columns must be overwritten: expected %s/%s, got %s/%s", tn.companyID, tn.merchantID, companyID, merchantID)
	}
}

// TestChildRLS_IsolationUnderAppRuntime — as app_runtime scoped to tenant A,
// tenant B's queue_status_history and admission_guarantor rows are invisible.
func TestChildRLS_IsolationUnderAppRuntime(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tnA := seedChildRLSTenant(t, ctx, pool, "A")
	tnB := seedChildRLSTenant(t, ctx, pool, "B")

	if _, err := pool.Exec(ctx,
		"INSERT INTO operations.queue_status_history (queue_id, to_status, changed_by) VALUES ($1, 'called', $2)", tnA.queueID, childRLSChangedBy); err != nil {
		t.Fatalf("seed tenant A history: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO operations.queue_status_history (queue_id, to_status, changed_by) VALUES ($1, 'called', $2)", tnB.queueID, childRLSChangedBy); err != nil {
		t.Fatalf("seed tenant B history: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO operations.admission_guarantor (admission_id, payer_id) VALUES ($1, $2)", tnA.admissionID, tnA.payerID); err != nil {
		t.Fatalf("seed tenant A guarantor: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO operations.admission_guarantor (admission_id, payer_id) VALUES ($1, $2)", tnB.admissionID, tnB.payerID); err != nil {
		t.Fatalf("seed tenant B guarantor: %v", err)
	}

	tx := childRLSAppRuntimeTx(t, ctx, pool, tnA.companyID, tnA.merchantID)
	defer tx.Rollback(ctx)

	var countA, countB int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM operations.queue_status_history WHERE queue_id = $1", tnA.queueID).Scan(&countA); err != nil {
		t.Fatalf("count tenant A history rows: %v", err)
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM operations.queue_status_history WHERE queue_id = $1", tnB.queueID).Scan(&countB); err != nil {
		t.Fatalf("count tenant B history rows: %v", err)
	}
	if countA != 1 || countB != 0 {
		t.Fatalf("queue_status_history isolation broken: expected A=1 B=0, got A=%d B=%d", countA, countB)
	}

	if err := tx.QueryRow(ctx, "SELECT count(*) FROM operations.admission_guarantor WHERE admission_id = $1", tnA.admissionID).Scan(&countA); err != nil {
		t.Fatalf("count tenant A guarantor rows: %v", err)
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM operations.admission_guarantor WHERE admission_id = $1", tnB.admissionID).Scan(&countB); err != nil {
		t.Fatalf("count tenant B guarantor rows: %v", err)
	}
	if countA != 1 || countB != 0 {
		t.Fatalf("admission_guarantor isolation broken: expected A=1 B=0, got A=%d B=%d", countA, countB)
	}
}

// TestChildRLS_CrossTenantParentRejected — as app_runtime scoped to tenant A,
// attaching a child row to tenant B's parent fails: the fill trigger reads the
// parent through the caller's RLS, gets NULL, and NOT NULL rejects the write.
func TestChildRLS_CrossTenantParentRejected(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tnA := seedChildRLSTenant(t, ctx, pool, "A")
	tnB := seedChildRLSTenant(t, ctx, pool, "B")

	tx := childRLSAppRuntimeTx(t, ctx, pool, tnA.companyID, tnA.merchantID)
	if _, err := tx.Exec(ctx,
		"INSERT INTO operations.admission_guarantor (admission_id, payer_id) VALUES ($1, $2)", tnB.admissionID, tnA.payerID); err == nil {
		t.Fatalf("cross-tenant guarantor insert as app_runtime must fail, got no error")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback guarantor tx: %v", err)
	}

	tx2 := childRLSAppRuntimeTx(t, ctx, pool, tnA.companyID, tnA.merchantID)
	defer tx2.Rollback(ctx)
	if _, err := tx2.Exec(ctx,
		"INSERT INTO operations.queue_status_history (queue_id, to_status, changed_by) VALUES ($1, 'called', $2)", tnB.queueID, childRLSChangedBy); err == nil {
		t.Fatalf("cross-tenant history insert as app_runtime must fail, got no error")
	}
}

// TestChildRLS_MergeLogSameCompanyOnly — the person_merge_log trigger refuses a
// merge whose two persons belong to different companies and fills company_id
// from the surviving person when both are in the same company.
func TestChildRLS_MergeLogSameCompanyOnly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tnA := seedChildRLSTenant(t, ctx, pool, "A")
	tnB := seedChildRLSTenant(t, ctx, pool, "B")

	_, err := pool.Exec(ctx,
		"INSERT INTO core.person_merge_log (surviving_person_id, merged_person_id, reason, merged_by) VALUES ($1, $2, 'cross-company test', $3)",
		tnA.personID, tnB.personID, childRLSChangedBy)
	if err == nil {
		t.Fatalf("merge across companies must fail, got no error")
	}
	if !strings.Contains(err.Error(), "person merge across companies is not allowed") {
		t.Fatalf("expected cross-company merge error, got: %v", err)
	}

	var secondPersonID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, 'Child RLS Person A Two', 'female') RETURNING id::text",
		tnA.companyID).Scan(&secondPersonID); err != nil {
		t.Fatalf("seed second tenant A person: %v", err)
	}
	var companyID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.person_merge_log (surviving_person_id, merged_person_id, reason, merged_by) VALUES ($1, $2, 'same-company test', $3) RETURNING company_id::text",
		secondPersonID, tnA.personID, childRLSChangedBy).Scan(&companyID); err != nil {
		t.Fatalf("same-company merge insert: %v", err)
	}
	if companyID != tnA.companyID {
		t.Fatalf("expected merge log company_id filled with %s, got %s", tnA.companyID, companyID)
	}
}

// TestChildRLS_ApiFlowStillWorks — the real endpoints keep working after the
// migration: POST /admissions creates its guarantor (company filled from the
// admission) and PATCH /queue/:id records a queue_status_history row with the
// right company/merchant.
func TestChildRLS_ApiFlowStillWorks(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "childrls.owner", "081234570061")

	createPersonViaAPI(t, router, token, companyID, merchantID, "Child RLS Flow Person", "female")
	personID, _ := latestPerson(t, ctx, pool, companyID, "Child RLS Flow Person")

	var departmentID, payerID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, 'CHLD-FLOW', 'Poli Child Flow') RETURNING id::text",
		companyID, merchantID).Scan(&departmentID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, 'CHLD-FLOW-PAY', 'Child Flow Payer', 'self_pay') RETURNING id::text",
		companyID, merchantID).Scan(&payerID); err != nil {
		t.Fatalf("seed payer: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"person_id":        personID,
		"department_id":    departmentID,
		"primary_payer_id": payerID,
		"guarantor_name":   "PT Child Flow Guarantor",
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/admissions", token, companyID, merchantID, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create admission expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var admissionID string
	if err := pool.QueryRow(ctx,
		"SELECT id::text FROM operations.admission WHERE person_id = $1 AND company_id = $2 ORDER BY created_at DESC LIMIT 1",
		personID, companyID).Scan(&admissionID); err != nil {
		t.Fatalf("resolve admission: %v", err)
	}
	var gCompany, gMerchant string
	if err := pool.QueryRow(ctx,
		"SELECT company_id::text, merchant_id::text FROM operations.admission_guarantor WHERE admission_id = $1",
		admissionID).Scan(&gCompany, &gMerchant); err != nil {
		t.Fatalf("resolve admission_guarantor: %v", err)
	}
	if gCompany != companyID || gMerchant != merchantID {
		t.Fatalf("guarantor company/merchant expected %s/%s, got %s/%s", companyID, merchantID, gCompany, gMerchant)
	}

	var queueID string
	if err := pool.QueryRow(ctx,
		"INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number) VALUES ($1, $2, 'pendaftaran', $3, $4, $5, 'CHLD-FLOW-001') RETURNING id::text",
		companyID, merchantID, departmentID, personID, admissionID).Scan(&queueID); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	rec = accountRequest(router, http.MethodPatch, "/api/v1/queue/"+queueID, token, companyID, merchantID, []byte(`{"status":"called"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch queue status expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var hCompany, hMerchant string
	if err := pool.QueryRow(ctx,
		"SELECT company_id::text, merchant_id::text FROM operations.queue_status_history WHERE queue_id = $1 AND to_status = 'called' ORDER BY changed_at DESC LIMIT 1",
		queueID).Scan(&hCompany, &hMerchant); err != nil {
		t.Fatalf("resolve queue_status_history: %v", err)
	}
	if hCompany != companyID || hMerchant != merchantID {
		t.Fatalf("history company/merchant expected %s/%s, got %s/%s", companyID, merchantID, hCompany, hMerchant)
	}
}