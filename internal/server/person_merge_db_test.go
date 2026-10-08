//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// mergeFixture: tenant A (merchant 1) from seedChildRLSTenant, a second
// merchant in the same company with its own admission for a second person.
type mergeFixture struct {
	companyID, merchant1, merchant2 string
	survivor, merged                string
	admission1, admission2          string
}

func seedMergeFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label string) mergeFixture {
	t.Helper()
	tn := seedChildRLSTenant(t, ctx, pool, label)
	f := mergeFixture{companyID: tn.companyID, merchant1: tn.merchantID, survivor: tn.personID, admission1: tn.admissionID}
	// the seeded admission/queue belong to the survivor; finish them so the
	// survivor has history but nothing active.
	if _, err := pool.Exec(ctx, "UPDATE operations.admission SET status = 'discharged' WHERE id = $1", tn.admissionID); err != nil {
		t.Fatalf("discharge: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE operations.queue SET status = 'done' WHERE id = $1", tn.queueID); err != nil {
		t.Fatalf("queue done: %v", err)
	}
	var dept2, payer2 string
	if err := pool.QueryRow(ctx, "INSERT INTO core.merchant (company_id, code, name) VALUES ($1, $2, $3) RETURNING id::text",
		f.companyID, "merge2-"+strings.ToLower(label), "Merge Merchant 2 "+label).Scan(&f.merchant2); err != nil {
		t.Fatalf("merchant 2: %v", err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO core.department (company_id, merchant_id, code, name) VALUES ($1, $2, $3, $4) RETURNING id::text",
		f.companyID, f.merchant2, "MRG2-"+label, "Poli 2 "+label).Scan(&dept2); err != nil {
		t.Fatalf("department 2: %v", err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type) VALUES ($1, $2, $3, $4, 'self_pay') RETURNING id::text",
		f.companyID, f.merchant2, "MRG2-PAY-"+label, "Payer 2 "+label).Scan(&payer2); err != nil {
		t.Fatalf("payer 2: %v", err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO core.person (company_id, full_name, gender) VALUES ($1, $2, 'male') RETURNING id::text",
		f.companyID, "Merged Person "+label).Scan(&f.merged); err != nil {
		t.Fatalf("merged person: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO operations.admission (company_id, merchant_id, visit_no, person_id, admission_type, department_id, primary_payer_id, status) VALUES ($1, $2, $3, $4, 'outpatient', $5, $6, 'discharged') RETURNING id::text",
		f.companyID, f.merchant2, "MRG2-"+label+"-1", f.merged, dept2, payer2).Scan(&f.admission2); err != nil {
		t.Fatalf("admission 2: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.patient_allergy (company_id, person_id, allergy_type, substance_name) VALUES ($1, $2, 'drug', 'Amoxicillin'), ($1, $3, 'drug', ' amoxicillin ')",
		f.companyID, f.survivor, f.merged); err != nil {
		t.Fatalf("allergies: %v", err)
	}
	return f
}

// TestPersonMergeDB_MovesAcrossMerchants — spec §2 B4: the request runs in
// merchant 1, yet the merged person's visit in merchant 2 moves too.
func TestPersonMergeDB_MovesAcrossMerchants(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	f := seedMergeFixture(t, ctx, pool, "MV")

	tx := childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
	var raw []byte
	err := tx.QueryRow(ctx, "SELECT core.person_merge_move($1, $2)", f.survivor, f.merged).Scan(&raw)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("person_merge_move: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	moved := map[string][]string{}
	if err := json.Unmarshal(raw, &moved); err != nil {
		t.Fatalf("decode moved: %v", err)
	}
	if len(moved["operations.admission"]) != 1 || moved["operations.admission"][0] != f.admission2 {
		t.Fatalf("moved admissions = %v, want [%s]", moved["operations.admission"], f.admission2)
	}
	var owner string
	if err := pool.QueryRow(ctx, "SELECT person_id::text FROM operations.admission WHERE id = $1", f.admission2).Scan(&owner); err != nil || owner != f.survivor {
		t.Fatalf("admission in merchant 2 owner = %s (%v), want survivor", owner, err)
	}
	var flagged int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.patient_allergy WHERE person_id = $1 AND needs_review", f.survivor).Scan(&flagged); err != nil || flagged != 2 {
		t.Fatalf("allergies flagged = %d (%v), want 2", flagged, err)
	}
}

// TestPersonMergeDB_BlockersAndCompanyScope — active visit blocks; a person
// of another company is "not found".
func TestPersonMergeDB_BlockersAndCompanyScope(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	f := seedMergeFixture(t, ctx, pool, "BL")
	other := seedChildRLSTenant(t, ctx, pool, "BLX")
	if _, err := pool.Exec(ctx, "UPDATE operations.admission SET status = 'registered' WHERE id = $1", f.admission2); err != nil {
		t.Fatalf("activate: %v", err)
	}

	tx := childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
	_, err := tx.Exec(ctx, "SELECT core.person_merge_move($1, $2)", f.survivor, f.merged)
	_ = tx.Rollback(ctx)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("active visit must block, got %v", err)
	}

	tx = childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
	_, err = tx.Exec(ctx, "SELECT core.person_merge_move($1, $2)", f.survivor, other.personID)
	_ = tx.Rollback(ctx)
	if err == nil || !strings.Contains(err.Error(), "not found in the current company") {
		t.Fatalf("other company person must be not found, got %v", err)
	}
}

// TestPersonMergeDB_GuardAndUnmerge — no new rows for a merged person; the
// unmerge moves back only the recorded rows.
func TestPersonMergeDB_GuardAndUnmerge(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	f := seedMergeFixture(t, ctx, pool, "GU")

	tx := childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT core.person_merge_move($1, $2)", f.survivor, f.merged).Scan(&raw); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("move: %v", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE core.person SET deleted_at = now(), deleted_by = $2, merged_into_person_id = $3 WHERE id = $1",
		f.merged, childRLSChangedBy, f.survivor); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("soft delete merged: %v", err)
	}
	var logID string
	if err := tx.QueryRow(ctx, "INSERT INTO core.person_merge_log (surviving_person_id, merged_person_id, reason, merged_by, moved) VALUES ($1, $2, 'test', $3, $4) RETURNING id::text",
		f.survivor, f.merged, childRLSChangedBy, raw).Scan(&logID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("log: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if _, err := pool.Exec(ctx, "INSERT INTO core.patient_allergy (company_id, person_id, allergy_type, substance_name) VALUES ($1, $2, 'food', 'Peanut')", f.companyID, f.merged); err == nil || !strings.Contains(err.Error(), "person has been merged") {
		t.Fatalf("guard must reject a row for a merged person, got %v", err)
	}

	// unmerge before restoring the merged record is refused
	tx = childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
	_, err := tx.Exec(ctx, "SELECT core.person_unmerge_move($1)", logID)
	_ = tx.Rollback(ctx)
	if err == nil || !strings.Contains(err.Error(), "restore the merged record") {
		t.Fatalf("unmerge before restore: got %v", err)
	}

	tx = childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
	if _, err := tx.Exec(ctx, "UPDATE core.person SET deleted_at = NULL, deleted_by = NULL, merged_into_person_id = NULL WHERE id = $1", f.merged); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("restore: %v", err)
	}
	var out []byte
	if err := tx.QueryRow(ctx, "SELECT core.person_unmerge_move($1)", logID).Scan(&out); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("unmerge: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit unmerge: %v", err)
	}
	var owner string
	if err := pool.QueryRow(ctx, "SELECT person_id::text FROM operations.admission WHERE id = $1", f.admission2).Scan(&owner); err != nil || owner != f.merged {
		t.Fatalf("admission 2 owner after unmerge = %s (%v), want merged", owner, err)
	}
	if err := pool.QueryRow(ctx, "SELECT person_id::text FROM operations.admission WHERE id = $1", f.admission1).Scan(&owner); err != nil || owner != f.survivor {
		t.Fatalf("survivor's own admission must stay, got %s (%v)", owner, err)
	}
	res := map[string]map[string]int{}
	if err := json.Unmarshal(out, &res); err != nil || res["operations.admission"]["restored"] != 1 {
		t.Fatalf("unmerge result = %s (%v)", out, err)
	}
}
