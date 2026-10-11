//go:build integration

package server_test

import (
	"context"
	"testing"
)

// TestReceivableDB_Constraints — balance, dispute-reason and payment-sign
// CHECK constraints plus the unique reversal_of of payer payments
// (spec 2026-10-07-tariff-b2-receivables-claims §3).
func TestReceivableDB_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "rcvdb.constraint")
	companyID, merchantID := tn.companyID, tn.merchantID

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)

	var accountID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.billing_account (company_id, merchant_id, admission_id)
		VALUES ($1,$2,$3) RETURNING id::text`, companyID, merchantID, tn.admissionID).Scan(&accountID); err != nil {
		t.Fatalf("insert billing_account: %v", err)
	}
	var receivableID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.receivable
			(company_id, merchant_id, account_id, admission_id, payer_id,
			 service_date_from, service_date_to, amount, due_date)
		VALUES ($1,$2,$3,$4,$5, current_date, current_date, 100000, current_date)
		RETURNING id::text`, companyID, merchantID, accountID, tn.admissionID, tn.payerID).Scan(&receivableID); err != nil {
		t.Fatalf("insert receivable: %v", err)
	}

	// paid + adjusted must never exceed the receivable amount.
	savepointExec(t, ctx, tx, "paid + adjusted above amount", "chk_receivable_balance", `
		UPDATE operations.receivable SET paid_amount = 60000, adjusted_amount = 50000 WHERE id = $1`, receivableID)

	// disputed status requires a non-blank dispute_reason.
	savepointExec(t, ctx, tx, "disputed without reason", "chk_receivable_dispute", `
		UPDATE operations.receivable SET status = 'disputed', dispute_reason = NULL WHERE id = $1`, receivableID)

	// only reversals may carry a negative amount.
	savepointExec(t, ctx, tx, "negative payment without reversal_of", "chk_payer_payment_sign", `
		INSERT INTO operations.payer_payment
			(company_id, merchant_id, payer_id, amount, paid_on, recorded_by)
		VALUES ($1,$2,$3,-100000, current_date, $4)`,
		companyID, merchantID, tn.payerID, childRLSChangedBy)

	// reversal_of is UNIQUE: a payment can be reversed at most once.
	var paymentID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.payer_payment
			(company_id, merchant_id, payer_id, amount, paid_on, recorded_by)
		VALUES ($1,$2,$3,100000, current_date, $4) RETURNING id::text`,
		companyID, merchantID, tn.payerID, childRLSChangedBy).Scan(&paymentID); err != nil {
		t.Fatalf("insert payer_payment: %v", err)
	}
	savepointExec(t, ctx, tx, "reversal with positive amount", "chk_payer_payment_sign", `
		INSERT INTO operations.payer_payment
			(company_id, merchant_id, payer_id, amount, paid_on, recorded_by, reversal_of)
		VALUES ($1,$2,$3,100000, current_date, $4, $5)`,
		companyID, merchantID, tn.payerID, childRLSChangedBy, paymentID)

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx = runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO operations.payer_payment
			(company_id, merchant_id, payer_id, amount, paid_on, recorded_by, reversal_of)
		VALUES ($1,$2,$3,-100000, current_date, $4, $5)`,
		companyID, merchantID, tn.payerID, childRLSChangedBy, paymentID); err != nil {
		t.Fatalf("insert first reversal: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit first reversal: %v", err)
	}

	tx = runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	savepointExec(t, ctx, tx, "second reversal of the same payment", "payer_payment_reversal_of_key", `
		INSERT INTO operations.payer_payment
			(company_id, merchant_id, payer_id, amount, paid_on, recorded_by, reversal_of)
		VALUES ($1,$2,$3,-100000, current_date, $4, $5)`,
		companyID, merchantID, tn.payerID, childRLSChangedBy, paymentID)
}

// TestReceivableDB_Immutable — payer payments, allocations and adjustments are
// append-only: app_runtime may neither UPDATE nor DELETE them.
func TestReceivableDB_Immutable(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "rcvdb.immutable")
	companyID, merchantID := tn.companyID, tn.merchantID

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	var accountID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.billing_account (company_id, merchant_id, admission_id)
		VALUES ($1,$2,$3) RETURNING id::text`, companyID, merchantID, tn.admissionID).Scan(&accountID); err != nil {
		t.Fatalf("insert billing_account: %v", err)
	}
	var receivableID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.receivable
			(company_id, merchant_id, account_id, admission_id, payer_id,
			 service_date_from, service_date_to, amount, due_date)
		VALUES ($1,$2,$3,$4,$5, current_date, current_date, 100000, current_date)
		RETURNING id::text`, companyID, merchantID, accountID, tn.admissionID, tn.payerID).Scan(&receivableID); err != nil {
		t.Fatalf("insert receivable: %v", err)
	}
	var paymentID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.payer_payment
			(company_id, merchant_id, payer_id, amount, paid_on, recorded_by)
		VALUES ($1,$2,$3,100000, current_date, $4) RETURNING id::text`,
		companyID, merchantID, tn.payerID, childRLSChangedBy).Scan(&paymentID); err != nil {
		t.Fatalf("insert payer_payment: %v", err)
	}
	var allocationID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.payer_payment_allocation
			(company_id, merchant_id, payment_id, receivable_id, amount, created_by)
		VALUES ($1,$2,$3,$4,100000,$5) RETURNING id::text`,
		companyID, merchantID, paymentID, receivableID, childRLSChangedBy).Scan(&allocationID); err != nil {
		t.Fatalf("insert payer_payment_allocation: %v", err)
	}
	var adjustmentID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.receivable_adjustment
			(company_id, merchant_id, receivable_id, kind, amount, reason, created_by)
		VALUES ($1,$2,$3,'write_off',25000,'Tidak dapat ditagih',$4) RETURNING id::text`,
		companyID, merchantID, receivableID, childRLSChangedBy).Scan(&adjustmentID); err != nil {
		t.Fatalf("insert receivable_adjustment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	tx = runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	savepointExec(t, ctx, tx, "update payer_payment", "permission denied",
		"UPDATE operations.payer_payment SET amount = 1 WHERE id = $1", paymentID)
	savepointExec(t, ctx, tx, "delete payer_payment", "permission denied",
		"DELETE FROM operations.payer_payment WHERE id = $1", paymentID)
	savepointExec(t, ctx, tx, "update payer_payment_allocation", "permission denied",
		"UPDATE operations.payer_payment_allocation SET amount = 1 WHERE id = $1", allocationID)
	savepointExec(t, ctx, tx, "delete payer_payment_allocation", "permission denied",
		"DELETE FROM operations.payer_payment_allocation WHERE id = $1", allocationID)
	savepointExec(t, ctx, tx, "update receivable_adjustment", "permission denied",
		"UPDATE operations.receivable_adjustment SET amount = 1 WHERE id = $1", adjustmentID)
	savepointExec(t, ctx, tx, "delete receivable_adjustment", "permission denied",
		"DELETE FROM operations.receivable_adjustment WHERE id = $1", adjustmentID)
}
