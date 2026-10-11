//go:build integration

package server_test

import (
	"net/http"
	"sync"
	"testing"
)

// TestPayerPayment_* covers the payer payment API (spec
// 2026-10-07-tariff-b2-receivables-claims §4.4.1–2): payments with
// allocations, the receivable status recompute, later allocations, the
// negative-payment reversal and the FOR UPDATE guarded concurrent allocation.

// ppClaimReceivable finalizes one BPJS claim visit (10000.00 receivable) and
// returns (payerID, receivableID).
func ppClaimReceivable(t *testing.T, env *qeEnv, label string) (string, string) {
	t.Helper()
	payerID, itemID := rcClaimBPJS(t, env, label, "other")
	rcNewClaimAdmission(t, env, payerID, itemID, label)
	return payerID, rcOnlyReceivable(t, env, payerID)
}

// ppCreate posts a payment and returns the raw status/body.
func ppCreate(t *testing.T, env *qeEnv, payerID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	return qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+payerID+"/payments", body)
}

// ppToday is today's date string in the merchant timezone.
func ppToday(t *testing.T, env *qeEnv) string {
	t.Helper()
	return rcToday().Format("2006-01-02")
}

// ppStatusOf reads the single receivable of the payer via the list endpoint.
func ppStatusOf(t *testing.T, env *qeEnv, payerID string) map[string]any {
	t.Helper()
	rows := rcList(t, env, "?payer_id="+payerID)
	if len(rows) != 1 {
		t.Fatalf("receivables = %d, want 1: %v", len(rows), rows)
	}
	return rows[0]
}

// ppAlloc is a shorthand allocation {receivable_id, amount}.
func ppAlloc(receivableID, amount string) map[string]any {
	return map[string]any{"receivable_id": receivableID, "amount": amount}
}

// TestPayerPayment_AllocateAndStatus — a partial allocation drives the
// receivable to partially_paid, the remainder payment to paid; the payment
// view carries allocated/unallocated and the date filter works.
func TestPayerPayment_AllocateAndStatus(t *testing.T) {
	env := payNewEnv(t, "pp.alloc")
	payerID, rcID := ppClaimReceivable(t, env, "PPA")
	today := ppToday(t, env)

	// Partial: 4000 of the 10000.00 receivable.
	status, out := ppCreate(t, env, payerID, map[string]any{
		"amount": "4000.00", "paid_on": today,
		"allocations": []map[string]any{ppAlloc(rcID, "4000.00")},
	})
	data, _ := payMapData(t, status, out, http.StatusCreated)["payment"].(map[string]any)
	if data["amount"] != "4000.00" || data["allocated"] != "4000.00" || data["unallocated"] != "0.00" {
		t.Fatalf("payment = %v", data)
	}
	row := ppStatusOf(t, env, payerID)
	if row["status"] != "partially_paid" || row["paid_amount"] != "4000.00" || row["balance"] != "6000.00" {
		t.Fatalf("after partial = %v", row)
	}

	// Remainder: the receivable becomes paid.
	status, out = ppCreate(t, env, payerID, map[string]any{
		"amount": "6000.00", "paid_on": today,
		"allocations": []map[string]any{ppAlloc(rcID, "6000.00")},
	})
	payMapData(t, status, out, http.StatusCreated)
	if row = ppStatusOf(t, env, payerID); row["status"] != "paid" {
		t.Fatalf("after remainder = %v, want paid", row["status"])
	}

	// Unallocated remainder: no allocations → 1000.00 stays unallocated.
	status, out = ppCreate(t, env, payerID, map[string]any{"amount": "1000.00", "paid_on": today})
	data, _ = payMapData(t, status, out, http.StatusCreated)["payment"].(map[string]any)
	if data["allocated"] != "0.00" || data["unallocated"] != "1000.00" {
		t.Fatalf("unallocated payment = %v", data)
	}

	// List: three payments with allocated/unallocated/reversed.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+payerID+"/payments", nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d %v", status, out)
	}
	rows := payDataSlice(t, out)
	if len(rows) != 3 {
		t.Fatalf("payments = %d, want 3: %v", len(rows), rows)
	}
	for _, p := range rows {
		if _, ok := p["allocated"]; !ok {
			t.Fatalf("payment missing allocated: %v", p)
		}
		if p["reversed"] != false {
			t.Fatalf("payment reversed = %v, want false", p["reversed"])
		}
	}
	// Date filter: from tomorrow → nothing.
	status, out = qeRequest(t, env, http.MethodGet,
		"/api/v1/payers/"+payerID+"/payments?from="+rcToday().AddDate(0, 0, 1).Format("2006-01-02"), nil)
	if got := payDataSlice(t, out); status != http.StatusOK || len(got) != 0 {
		t.Fatalf("list from tomorrow = %d %v", status, got)
	}
}

// TestPayerPayment_OverAllocationRejected — allocations above the payment
// amount answer 400 `allocations exceed payment`, allocations above the
// receivable balance answer 409, foreign receivables answer 400, non-positive
// amounts answer 400 — and failed requests leave no payment behind.
func TestPayerPayment_OverAllocationRejected(t *testing.T) {
	env := payNewEnv(t, "pp.over")
	payerID, rcID := ppClaimReceivable(t, env, "PPO")
	// A second payer of the same merchant holding no receivables.
	otherID := pcCreatePayer(t, env, "PPO-OTH", "Asuransi PPO", "private_insurance")

	// Sum of allocations above the payment amount.
	status, out := ppCreate(t, env, payerID, map[string]any{
		"amount": "5000.00", "paid_on": ppToday(t, env),
		"allocations": []map[string]any{ppAlloc(rcID, "6000.00")},
	})
	if status != http.StatusBadRequest || out["error"] != "allocations exceed payment" {
		t.Fatalf("over payment = %d %v", status, out)
	}
	// Allocation above the receivable balance.
	status, out = ppCreate(t, env, payerID, map[string]any{
		"amount": "10001.00", "paid_on": ppToday(t, env),
		"allocations": []map[string]any{ppAlloc(rcID, "10001.00")},
	})
	if status != http.StatusConflict || out["error"] != "allocation exceeds receivable balance" {
		t.Fatalf("over balance = %d %v", status, out)
	}
	// The receivable belongs to another payer.
	status, out = ppCreate(t, env, otherID, map[string]any{
		"amount": "1000.00", "paid_on": ppToday(t, env),
		"allocations": []map[string]any{ppAlloc(rcID, "1000.00")},
	})
	if status != http.StatusBadRequest || out["error"] != "receivable does not belong to payer" {
		t.Fatalf("foreign receivable = %d %v", status, out)
	}
	// amount must be positive.
	status, out = ppCreate(t, env, payerID, map[string]any{"amount": "0", "paid_on": ppToday(t, env)})
	if status != http.StatusBadRequest {
		t.Fatalf("zero amount = %d %v", status, out)
	}
	// Nothing was written.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+payerID+"/payments", nil)
	if got := payDataSlice(t, out); status != http.StatusOK || len(got) != 0 {
		t.Fatalf("payments after rejects = %v, want none", got)
	}
	if row := ppStatusOf(t, env, payerID); row["status"] != "open" || row["paid_amount"] != "0.00" {
		t.Fatalf("receivable after rejects = %v", row)
	}
}

// TestPayerPayment_LaterAllocation — an unallocated payment is allocated
// afterwards in steps; the payment-total cap holds across calls.
func TestPayerPayment_LaterAllocation(t *testing.T) {
	env := payNewEnv(t, "pp.later")
	payerID, rcID := ppClaimReceivable(t, env, "PPL")

	status, out := ppCreate(t, env, payerID, map[string]any{
		"amount": "10000.00", "paid_on": ppToday(t, env),
	})
	data := payMapData(t, status, out, http.StatusCreated)
	if data["unallocated"] != "10000.00" {
		t.Fatalf("fresh unallocated = %v", data["unallocated"])
	}
	if row := ppStatusOf(t, env, payerID); row["status"] != "open" {
		t.Fatalf("status before allocation = %v, want open", row["status"])
	}
	paymentID, _ := data["payment"].(map[string]any)["id"].(string)

	status, out = qeRequest(t, env, http.MethodPost,
		"/api/v1/payer-payments/"+paymentID+"/allocations",
		map[string]any{"allocations": []map[string]any{ppAlloc(rcID, "4000.00")}})
	payMapData(t, status, out, http.StatusCreated)
	if row := ppStatusOf(t, env, payerID); row["status"] != "partially_paid" {
		t.Fatalf("after later alloc = %v, want partially_paid", row["status"])
	}
	// 4000 already allocated; +7000 would exceed the 10000 payment.
	status, out = qeRequest(t, env, http.MethodPost,
		"/api/v1/payer-payments/"+paymentID+"/allocations",
		map[string]any{"allocations": []map[string]any{ppAlloc(rcID, "7000.00")}})
	if status != http.StatusBadRequest || out["error"] != "allocations exceed payment" {
		t.Fatalf("later over payment = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPost,
		"/api/v1/payer-payments/"+paymentID+"/allocations",
		map[string]any{"allocations": []map[string]any{ppAlloc(rcID, "6000.00")}})
	data = payMapData(t, status, out, http.StatusCreated)
	if data["unallocated"] != "0.00" {
		t.Fatalf("final unallocated = %v", data["unallocated"])
	}
	if row := ppStatusOf(t, env, payerID); row["status"] != "paid" {
		t.Fatalf("final status = %v, want paid", row["status"])
	}
}

// TestPayerPayment_Reverse — a reversed payment becomes paid again through a
// negative payment + negative allocations; the second reversal answers 409 and
// the list flags the original as reversed.
func TestPayerPayment_Reverse(t *testing.T) {
	env := payNewEnv(t, "pp.rev")
	payerID, rcID := ppClaimReceivable(t, env, "PPR")
	today := ppToday(t, env)

	status, out := ppCreate(t, env, payerID, map[string]any{
		"amount": "10000.00", "paid_on": today, "reference": "TRX-1",
		"allocations": []map[string]any{ppAlloc(rcID, "10000.00")},
	})
	paymentID, _ := payMapData(t, status, out, http.StatusCreated)["payment"].(map[string]any)["id"].(string)
	if row := ppStatusOf(t, env, payerID); row["status"] != "paid" {
		t.Fatalf("pre reverse = %v, want paid", row["status"])
	}

	status, out = qeRequest(t, env, http.MethodPost,
		"/api/v1/payer-payments/"+paymentID+"/reverse", map[string]any{"reason": "klaim ditolak"})
	data := payMapData(t, status, out, http.StatusCreated)
	rev, _ := data["payment"].(map[string]any)
	if rev["amount"] != "-10000.00" || rev["note"] != "klaim ditolak" || rev["reversal_of"] != paymentID {
		t.Fatalf("reversal payment = %v", rev)
	}
	allocs, _ := data["allocations"].([]any)
	if len(allocs) != 1 {
		t.Fatalf("reversal allocations = %v", data["allocations"])
	}
	// The status recomputes back: paid 0 → open.
	if row := ppStatusOf(t, env, payerID); row["status"] != "open" || row["paid_amount"] != "0.00" {
		t.Fatalf("after reverse = %v", row)
	}

	// Reversing twice is refused (unique reversal_of).
	status, out = qeRequest(t, env, http.MethodPost,
		"/api/v1/payer-payments/"+paymentID+"/reverse", map[string]any{"reason": "coba lagi"})
	if status != http.StatusConflict || out["error"] != "payment already reversed" {
		t.Fatalf("second reverse = %d %v", status, out)
	}
	// The reversal payment itself cannot be reversed.
	revID, _ := rev["id"].(string)
	status, out = qeRequest(t, env, http.MethodPost,
		"/api/v1/payer-payments/"+revID+"/reverse", map[string]any{"reason": "balik balik"})
	if status != http.StatusConflict {
		t.Fatalf("reverse of reversal = %d %v", status, out)
	}

	// List flags the original as reversed.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+payerID+"/payments", nil)
	rows := payDataSlice(t, out)
	if status != http.StatusOK || len(rows) != 2 {
		t.Fatalf("payments = %d %v, want 2", len(rows), rows)
	}
	byAmount := map[string]map[string]any{}
	for _, p := range rows {
		byAmount[p["amount"].(string)] = p
	}
	if byAmount["10000.00"]["reversed"] != true {
		t.Fatalf("original reversed flag = %v", byAmount["10000.00"]["reversed"])
	}
	if byAmount["-10000.00"]["reversed"] != false {
		t.Fatalf("reversal reversed flag = %v", byAmount["-10000.00"]["reversed"])
	}
}

// TestPayerPayment_DisputedStays — an allocation on a disputed receivable
// keeps it disputed (amounts still refresh); releasing the dispute lands on
// the computed status.
func TestPayerPayment_DisputedStays(t *testing.T) {
	env := payNewEnv(t, "pp.disp")
	payerID, rcID := ppClaimReceivable(t, env, "PPD")

	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/receivables/"+rcID,
		map[string]any{"disputed": true, "reason": "tagihan diperselisihkan"})
	payMapData(t, status, out, http.StatusOK)

	status, out = ppCreate(t, env, payerID, map[string]any{
		"amount": "4000.00", "paid_on": ppToday(t, env),
		"allocations": []map[string]any{ppAlloc(rcID, "4000.00")},
	})
	payMapData(t, status, out, http.StatusCreated)
	row := ppStatusOf(t, env, payerID)
	if row["status"] != "disputed" {
		t.Fatalf("disputed status = %v, want disputed", row["status"])
	}
	if row["paid_amount"] != "4000.00" || row["balance"] != "6000.00" {
		t.Fatalf("disputed amounts = %v", row)
	}

	// Releasing the dispute recomputes to partially_paid.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/receivables/"+rcID,
		map[string]any{"disputed": false})
	payMapData(t, status, out, http.StatusOK)
	if row = ppStatusOf(t, env, payerID); row["status"] != "partially_paid" {
		t.Fatalf("released status = %v, want partially_paid", row["status"])
	}
}

// TestPayerPayment_Concurrent — two concurrent full allocations onto the same
// receivable: exactly one succeeds, the other answers 409 (SELECT … FOR UPDATE
// serializes the balance checks).
func TestPayerPayment_Concurrent(t *testing.T) {
	env := payNewEnv(t, "pp.cc")
	payerID, rcID := ppClaimReceivable(t, env, "PPC")
	today := ppToday(t, env)

	type result struct {
		status int
		body   map[string]any
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(slot int) {
			defer wg.Done()
			// No t.Fatalf here: failures surface via the status check below.
			status, out := qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+payerID+"/payments", map[string]any{
				"amount": "10000.00", "paid_on": today,
				"allocations": []map[string]any{ppAlloc(rcID, "10000.00")},
			})
			results[slot] = result{status: status, body: out}
		}(i)
	}
	wg.Wait()

	created, conflicts := 0, 0
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			if r.body["error"] != "allocation exceeds receivable balance" {
				t.Fatalf("conflict body = %v", r.body)
			}
			conflicts++
		default:
			t.Fatalf("unexpected status %d: %v", r.status, r.body)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("created = %d, conflicts = %d, want 1/1: %v", created, conflicts, results)
	}
	if row := ppStatusOf(t, env, payerID); row["status"] != "paid" || row["paid_amount"] != "10000.00" {
		t.Fatalf("after race = %v", row)
	}
	status, out := qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+payerID+"/payments", nil)
	if got := payDataSlice(t, out); status != http.StatusOK || len(got) != 1 {
		t.Fatalf("payments after race = %d %v, want exactly one", len(got), got)
	}
}
