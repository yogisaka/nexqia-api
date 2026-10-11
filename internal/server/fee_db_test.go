//go:build integration

package server_test

import (
	"context"
	"testing"
)

// TestFeeDB_TaxDataSeeded — migration 000070 seeds the PPh 21 reference data
// (spec 2026-10-07-tariff-d-practitioner-fees-tax §3): TER brackets with no
// gaps between consecutive rows, PTKP annual amounts, the five Pasal 17
// brackets, and write protection for app_runtime.
func TestFeeDB_TaxDataSeeded(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	tx := runtimeTenantTx(t, ctx, pool, "00000000-0000-0000-0000-0000000000f0", "00000000-0000-0000-0000-0000000000f1")

	var total int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.tax_ter_rate").Scan(&total); err != nil {
		t.Fatalf("count ter rates: %v", err)
	}
	if total != 125 {
		t.Fatalf("expected 125 TER rows (A 44 + B 40 + C 41), got %d", total)
	}
	rows, err := tx.Query(ctx, `
		SELECT category, count(*) FROM core.tax_ter_rate GROUP BY category`)
	if err != nil {
		t.Fatalf("count per category: %v", err)
	}
	perCategory := map[string]int{}
	for rows.Next() {
		var cat string
		var n int
		if err := rows.Scan(&cat, &n); err != nil {
			t.Fatalf("scan category count: %v", err)
		}
		perCategory[cat] = n
	}
	rows.Close()
	if perCategory["A"] != 44 || perCategory["B"] != 40 || perCategory["C"] != 41 {
		t.Fatalf("expected A=44 B=40 C=41, got A=%d B=%d C=%d", perCategory["A"], perCategory["B"], perCategory["C"])
	}

	// A first bracket is 0..5.400.000 at 0%.
	var lower, upper, rate float64
	var upperPtr *float64
	if err := tx.QueryRow(ctx, `
		SELECT lower_exclusive, upper_inclusive, rate_percent FROM core.tax_ter_rate
		WHERE category = 'A' ORDER BY lower_exclusive LIMIT 1`).Scan(&lower, &upper, &rate); err != nil {
		t.Fatalf("first A bracket: %v", err)
	}
	if lower != 0 || upper != 5400000 || rate != 0 {
		t.Fatalf("first A bracket: got (%v, %v, %v), want (0, 5400000, 0)", lower, upper, rate)
	}

	// Top brackets are open-ended: A ends at 1.400.000.000, B at 1.405.000.000,
	// C at 1.419.000.000, all at 34%.
	for _, tc := range []struct {
		cat       string
		lastLower float64
	}{
		{"A", 1400000000}, {"B", 1405000000}, {"C", 1419000000},
	} {
		if err := tx.QueryRow(ctx, `
			SELECT lower_exclusive, upper_inclusive, rate_percent FROM core.tax_ter_rate
			WHERE category = $1 ORDER BY lower_exclusive DESC LIMIT 1`, tc.cat).Scan(&lower, &upperPtr, &rate); err != nil {
			t.Fatalf("last %s bracket: %v", tc.cat, err)
		}
		if lower != tc.lastLower || upperPtr != nil || rate != 34 {
			t.Fatalf("last %s bracket: got (%v, %v, %v), want (%v, NULL, 34)", tc.cat, lower, upperPtr, rate, tc.lastLower)
		}
	}

	// Absolute boundary guard (PP 58/2023): category B must transition at
	// 7.300.000 (not 7.350.000, which belongs to category C).
	var b1, b3, b4 float64
	var b3Upper *float64
	if err := tx.QueryRow(ctx, `
		SELECT lower_exclusive FROM core.tax_ter_rate
		WHERE category = 'B' ORDER BY lower_exclusive LIMIT 1 OFFSET 3`).Scan(&b3); err != nil {
		t.Fatalf("B bracket 4: %v", err)
	}
	if b3 != 6850000 {
		t.Fatalf("B bracket 4 lower: got %v, want 6850000", b3)
	}
	if err := tx.QueryRow(ctx, `
		SELECT lower_exclusive FROM core.tax_ter_rate
		WHERE category = 'B' ORDER BY lower_exclusive LIMIT 1 OFFSET 4`).Scan(&b4); err != nil {
		t.Fatalf("B bracket 5: %v", err)
	}
	if b4 != 7300000 {
		t.Fatalf("B bracket 4 lower: got %v, want 7300000", b4)
	}
	if err := tx.QueryRow(ctx, `
		SELECT lower_exclusive, upper_inclusive FROM core.tax_ter_rate
		WHERE category = 'B' AND lower_exclusive = 7300000`).Scan(&b1, &b3Upper); err != nil {
		t.Fatalf("B bracket at 7300000: %v", err)
	}
	if b3Upper == nil || *b3Upper != 9200000 {
		t.Fatalf("B bracket at 7300000 upper: got %v, want 9200000", b3Upper)
	}
	var leaked int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM core.tax_ter_rate
		WHERE category = 'B' AND (lower_exclusive = 7350000 OR upper_inclusive = 7350000)`).Scan(&leaked); err != nil {
		t.Fatalf("check B 7350000 leak: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("category B must not use 7350000 (category C boundary), found %d rows", leaked)
	}

	// No gaps: within each category, upper_inclusive of row i equals
	// lower_exclusive of row i+1.
	rows, err = tx.Query(ctx, `
		SELECT category, lower_exclusive, upper_inclusive FROM core.tax_ter_rate
		ORDER BY category, lower_exclusive`)
	if err != nil {
		t.Fatalf("list brackets: %v", err)
	}
	defer rows.Close()
	type bracket struct {
		cat          string
		lower, upper *float64
	}
	var brackets []bracket
	for rows.Next() {
		var b bracket
		if err := rows.Scan(&b.cat, &b.lower, &b.upper); err != nil {
			t.Fatalf("scan bracket: %v", err)
		}
		brackets = append(brackets, b)
	}
	rows.Close()
	for i := 1; i < len(brackets); i++ {
		prev, cur := brackets[i-1], brackets[i]
		if prev.cat != cur.cat {
			continue
		}
		if prev.upper == nil || *prev.upper != *cur.lower {
			t.Fatalf("gap in category %s: upper %v does not continue at lower %v", prev.cat, prev.upper, cur.lower)
		}
	}

	var ptkpCount int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.tax_ptkp").Scan(&ptkpCount); err != nil {
		t.Fatalf("count ptkp: %v", err)
	}
	if ptkpCount != 8 {
		t.Fatalf("expected 8 PTKP rows, got %d", ptkpCount)
	}
	var pasal17Count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.tax_pasal17_bracket").Scan(&pasal17Count); err != nil {
		t.Fatalf("count pasal 17 brackets: %v", err)
	}
	if pasal17Count != 5 {
		t.Fatalf("expected 5 Pasal 17 brackets, got %d", pasal17Count)
	}

	// Tax reference data is platform-owned: app_runtime may not write it.
	savepointExec(t, ctx, tx, "insert ter rate", "permission denied", `
		INSERT INTO core.tax_ter_rate (category, lower_exclusive, rate_percent, effective_from)
		VALUES ('A', 999999999, 1, current_date)`)
	savepointExec(t, ctx, tx, "update tax setting", "permission denied", `
		UPDATE core.tax_setting SET value = 6 WHERE key = 'biaya_jabatan_percent'`)

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestFeeDB_Constraints — CHECK constraints of the practitioner fee schema:
// PTKP pairs with employment_type, fee rules pair basis with target/value
// columns, in-batch fees must reference a batch, and only employees may carry
// other_gross (spec 2026-10-07-tariff-d-practitioner-fees-tax §3).
func TestFeeDB_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	tn := seedChildRLSTenant(t, ctx, pool, "feedb.constraint")
	companyID, merchantID := tn.companyID, tn.merchantID

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)

	var physicianID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.physician (company_id, merchant_id, person_id)
		VALUES ($1,$2,$3) RETURNING id::text`,
		companyID, merchantID, tn.personID).Scan(&physicianID); err != nil {
		t.Fatalf("insert physician: %v", err)
	}

	// employee requires a PTKP status.
	savepointExec(t, ctx, tx, "employee without PTKP", "chk_physician_ptkp",
		"UPDATE core.physician SET employment_type = 'employee' WHERE id = $1", physicianID)
	// partners (and owners) must not carry a PTKP status.
	savepointExec(t, ctx, tx, "partner with PTKP", "chk_physician_ptkp",
		"UPDATE core.physician SET ptkp_status = 'TK/0' WHERE id = $1", physicianID)

	var serviceItemID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.service_item (company_id, merchant_id, code, name, item_type)
		VALUES ($1,$2,'FEEDB-1','Konsultasi Fee','consultation') RETURNING id::text`,
		companyID, merchantID).Scan(&serviceItemID); err != nil {
		t.Fatalf("insert service_item: %v", err)
	}
	var accountID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.billing_account (company_id, merchant_id, admission_id)
		VALUES ($1,$2,$3) RETURNING id::text`, companyID, merchantID, tn.admissionID).Scan(&accountID); err != nil {
		t.Fatalf("insert billing_account: %v", err)
	}
	var chargeID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.charge (company_id, merchant_id, account_id, admission_id, service_item_id, quantity, service_date)
		VALUES ($1,$2,$3,$4,$5,1, current_date) RETURNING id::text`,
		companyID, merchantID, accountID, tn.admissionID, serviceItemID).Scan(&chargeID); err != nil {
		t.Fatalf("insert charge: %v", err)
	}

	// percent_of_component must name a component_code.
	savepointExec(t, ctx, tx, "percent_of_component without component_code", "chk_fee_rule_component", `
		INSERT INTO core.practitioner_fee_rule
			(company_id, merchant_id, physician_id, basis, percent, effective_from)
		VALUES ($1,$2,$3,'percent_of_component',50, current_date)`,
		companyID, merchantID, physicianID)

	// a rule targets either a service item or an item type, never both.
	savepointExec(t, ctx, tx, "fee rule with item and type", "chk_fee_rule_target", `
		INSERT INTO core.practitioner_fee_rule
			(company_id, merchant_id, physician_id, service_item_id, item_type, basis, percent, effective_from)
		VALUES ($1,$2,$3,$4,'consultation','percent_of_total',10, current_date)`,
		companyID, merchantID, physicianID, serviceItemID)

	// an in_batch fee must reference its payout batch.
	savepointExec(t, ctx, tx, "in_batch fee without batch", "chk_practitioner_fee_batch", `
		INSERT INTO operations.practitioner_fee
			(company_id, merchant_id, account_id, charge_id, physician_id, role, service_date, gross_amount, fee_amount, status)
		VALUES ($1,$2,$3,$4,$5,'primary', current_date, 100000, 50000, 'in_batch')`,
		companyID, merchantID, accountID, chargeID, physicianID)

	var batchID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.fee_payout_batch (company_id, merchant_id, period_year, period_month)
		VALUES ($1,$2,2026,10) RETURNING id::text`, companyID, merchantID).Scan(&batchID); err != nil {
		t.Fatalf("insert fee_payout_batch: %v", err)
	}

	// only employees may carry other_gross.
	savepointExec(t, ctx, tx, "partner line with other_gross", "chk_fee_line_other_gross", `
		INSERT INTO operations.fee_payout_line
			(company_id, merchant_id, batch_id, physician_id, employment_type, other_gross)
		VALUES ($1,$2,$3,$4,'partner',5000)`,
		companyID, merchantID, batchID, physicianID)

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
