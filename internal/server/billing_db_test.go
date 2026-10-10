//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"
)

// TestBillingDB_Constraints — CHECK constraints and partial unique indexes of
// the billing core: BPJS network parent, one live BPJS payer per merchant,
// coverage target/claim pairing, void reason, split party pairing and the
// finalized-account totals.
func TestBillingDB_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "bildb.constraint")
	companyID, merchantID := tn.companyID, tn.merchantID

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)

	savepointExec(t, ctx, tx, "fktp_network without parent", "chk_merchant_bpjs_network",
		"UPDATE core.merchant SET bpjs_cooperation = 'fktp_network' WHERE id = $1", merchantID)

	var bpjsPayerID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type)
		VALUES ($1,$2,'BPJS-1','BPJS Payer','bpjs') RETURNING id::text`,
		companyID, merchantID).Scan(&bpjsPayerID); err != nil {
		t.Fatalf("insert bpjs payer: %v", err)
	}
	savepointExec(t, ctx, tx, "second live bpjs payer", "uq_payer_bpjs_per_merchant", `
		INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type)
		VALUES ($1,$2,'BPJS-2','BPJS Payer 2','bpjs')`, companyID, merchantID)

	savepointExec(t, ctx, tx, "claim coverage without claim_type", "chk_payer_item_coverage_claim", `
		INSERT INTO core.payer_item_coverage (company_id, merchant_id, payer_id, item_type, coverage)
		VALUES ($1,$2,$3,'consultation','claim')`, companyID, merchantID, bpjsPayerID)

	var serviceItemID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.service_item (company_id, merchant_id, code, name, item_type)
		VALUES ($1,$2,'BILDB-1','Konsultasi Billing','consultation') RETURNING id::text`,
		companyID, merchantID).Scan(&serviceItemID); err != nil {
		t.Fatalf("insert service_item: %v", err)
	}
	savepointExec(t, ctx, tx, "coverage with item and type", "chk_payer_item_coverage_target", `
		INSERT INTO core.payer_item_coverage (company_id, merchant_id, payer_id, service_item_id, item_type, coverage)
		VALUES ($1,$2,$3,$4,'consultation','capitation')`, companyID, merchantID, bpjsPayerID, serviceItemID)

	var admissionID string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM operations.admission WHERE merchant_id = $1 LIMIT 1", merchantID).Scan(&admissionID); err != nil {
		t.Fatalf("resolve admission: %v", err)
	}
	savepointExec(t, ctx, tx, "finalized account without totals", "chk_billing_account_final", `
		INSERT INTO operations.billing_account (company_id, merchant_id, admission_id, status, finalized_at)
		VALUES ($1,$2,$3,'finalized', now())`, companyID, merchantID, admissionID)

	var accountID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.billing_account (company_id, merchant_id, admission_id)
		VALUES ($1,$2,$3) RETURNING id::text`, companyID, merchantID, admissionID).Scan(&accountID); err != nil {
		t.Fatalf("insert billing_account: %v", err)
	}
	savepointExec(t, ctx, tx, "void charge without reason", "chk_charge_void", `
		INSERT INTO operations.charge (company_id, merchant_id, account_id, admission_id, service_item_id, quantity, service_date, status)
		VALUES ($1,$2,$3,$4,$5,1, current_date, 'void')`, companyID, merchantID, accountID, admissionID, serviceItemID)

	var chargeID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.charge (company_id, merchant_id, account_id, admission_id, service_item_id, quantity, service_date)
		VALUES ($1,$2,$3,$4,$5,1, current_date) RETURNING id::text`,
		companyID, merchantID, accountID, admissionID, serviceItemID).Scan(&chargeID); err != nil {
		t.Fatalf("insert charge: %v", err)
	}
	var priceListID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.price_list (company_id, merchant_id, code, name)
		VALUES ($1,$2,'PL-BILDB','Price List Billing') RETURNING id::text`,
		companyID, merchantID).Scan(&priceListID); err != nil {
		t.Fatalf("insert price_list: %v", err)
	}
	savepointExec(t, ctx, tx, "payer split without payer_id", "chk_charge_split_party", `
		INSERT INTO operations.charge_split
			(company_id, merchant_id, charge_id, seq, party, coverage, price_list_id, unit_amount, gross_amount, amount)
		VALUES ($1,$2,$3,1,'payer','claim',$4,10000,10000,10000)`,
		companyID, merchantID, chargeID, priceListID)
}

// TestBillingDB_ConsentImmutable — out-of-benefit consent rows are append-only:
// app_runtime may neither UPDATE nor DELETE them.
func TestBillingDB_ConsentImmutable(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "bildb.consent")
	companyID, merchantID := tn.companyID, tn.merchantID

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	var admissionID string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM operations.admission WHERE merchant_id = $1 LIMIT 1", merchantID).Scan(&admissionID); err != nil {
		t.Fatalf("resolve admission: %v", err)
	}
	var consentID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.billing_consent (company_id, merchant_id, admission_id, consented_by_name, relation, recorded_by)
		VALUES ($1,$2,$3,'Pasien Sendiri','self',$4) RETURNING id::text`,
		companyID, merchantID, admissionID, childRLSChangedBy).Scan(&consentID); err != nil {
		t.Fatalf("insert billing_consent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit consent: %v", err)
	}

	tx = runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	savepointExec(t, ctx, tx, "update consent", "permission denied",
		"UPDATE operations.billing_consent SET consented_by_name = 'Lain' WHERE id = $1", consentID)
	savepointExec(t, ctx, tx, "delete consent", "permission denied",
		"DELETE FROM operations.billing_consent WHERE id = $1", consentID)
}

// TestBillingDB_CoverageFollowsMerge — an RM merge moves the patient coverage
// to the survivor, the unmerge restores it, and no new coverage may be created
// for an already-merged person.
func TestBillingDB_CoverageFollowsMerge(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	f := seedMergeFixture(t, ctx, pool, "CV")

	var payerID, coverageID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type)
		VALUES ($1,$2,'CV-PAY','Coverage Payer CV','bpjs') RETURNING id::text`,
		f.companyID, f.merchant1).Scan(&payerID); err != nil {
		t.Fatalf("seed payer: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO core.patient_coverage (company_id, merchant_id, person_id, payer_id, member_number, bpjs_class)
		VALUES ($1,$2,$3,$4,'0001234567','1') RETURNING id::text`,
		f.companyID, f.merchant1, f.merged, payerID).Scan(&coverageID); err != nil {
		t.Fatalf("seed coverage: %v", err)
	}

	tx := childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT core.person_merge_move($1, $2)", f.survivor, f.merged).Scan(&raw); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("person_merge_move: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit merge: %v", err)
	}
	var owner string
	if err := pool.QueryRow(ctx, "SELECT person_id::text FROM core.patient_coverage WHERE id = $1", coverageID).Scan(&owner); err != nil || owner != f.survivor {
		t.Fatalf("coverage owner after merge = %s (%v), want survivor", owner, err)
	}

	// soft delete + merge log, then the guard blocks new coverage for the
	// merged person.
	tx = childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
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
		t.Fatalf("commit log: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO core.patient_coverage (company_id, merchant_id, person_id, payer_id, member_number)
		VALUES ($1,$2,$3,$4,'0009999999')`, f.companyID, f.merchant1, f.merged, payerID); err == nil || !strings.Contains(err.Error(), "person has been merged") {
		t.Fatalf("guard must reject coverage for a merged person, got %v", err)
	}

	// unmerge restores the coverage to the merged person.
	tx = childRLSAppRuntimeTx(t, ctx, pool, f.companyID, f.merchant1)
	if _, err := tx.Exec(ctx, "UPDATE core.person SET deleted_at = NULL, deleted_by = NULL, merged_into_person_id = NULL WHERE id = $1", f.merged); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("restore: %v", err)
	}
	var out []byte
	if err := tx.QueryRow(ctx, "SELECT core.person_unmerge_move($1)", logID).Scan(&out); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("person_unmerge_move: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit unmerge: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT person_id::text FROM core.patient_coverage WHERE id = $1", coverageID).Scan(&owner); err != nil || owner != f.merged {
		t.Fatalf("coverage owner after unmerge = %s (%v), want merged", owner, err)
	}
}
