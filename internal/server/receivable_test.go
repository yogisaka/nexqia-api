//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestReceivable_* covers the payer receivables API (spec
// 2026-10-07-tariff-b2-receivables-claims §4.1/§4.3/§4.4.3–4/§4.5):
// creation at finalize per payer, the reopen guard, aging buckets,
// BPJS expiry watchlist, the manual dispute flag, the literal per-payer
// document checklist and balance adjustments.

var rcJakarta = mustLocJakarta()

func mustLocJakarta() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		panic(err)
	}
	return loc
}

// rcToday is merchant-local today (the finalize day) as a date-only time.
func rcToday() time.Time {
	y, m, d := time.Now().In(rcJakarta).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, rcJakarta)
}

// rcClaimBPJS creates a default price list (10000.00/consultation) and a BPJS
// payer whose item coverage bills consultation charges as a claim of the given
// claim type (a positive payer share). Returns (payerID, itemID).
func rcClaimBPJS(t *testing.T, env *qeEnv, label, claimType string) (string, string) {
	t.Helper()
	// A BPJS guarantor requires a non-`none` facility cooperation profile;
	// the claim itself comes from the payer item-coverage rule below.
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fkrtl"})
	payMapData(t, status, out, http.StatusOK)
	listID, itemID := bilDefaultList(t, env, label, "10000.00")
	_ = listID
	payerID := pcCreatePayer(t, env, label+"-BPJS", "BPJS "+label, "bpjs")
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payers/"+payerID+"/item-coverage",
		map[string]any{"rules": []map[string]any{{
			"item_type": "consultation", "coverage": "claim", "claim_type": claimType,
		}}})
	if status != http.StatusOK {
		t.Fatalf("put item coverage = %d %v", status, out)
	}
	return payerID, itemID
}

// rcNewClaimAdmission wires a new visit to the claim payer and finalizes it
// (a merchant can hold only ONE BPJS payer, so tests reuse it per visit).
func rcNewClaimAdmission(t *testing.T, env *qeEnv, payerID, itemID, label string) string {
	t.Helper()
	_, admissionID := bilNewPersonAndVisit(t, env, label, "Pasien "+label)
	bilPutGuarantor(t, env, admissionID, payerID, "")
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	payMapData(t, status, out, http.StatusCreated)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	return admissionID
}

// rcFinalizeClaim drives one visit to a finalized account with a single BPJS
// claim receivable; returns (admissionID, receivableID).
func rcFinalizeClaim(t *testing.T, env *qeEnv, label, claimType string) (string, string) {
	t.Helper()
	payerID, itemID := rcClaimBPJS(t, env, label, claimType)
	admissionID := rcNewClaimAdmission(t, env, payerID, itemID, label)
	return admissionID, rcOnlyReceivable(t, env, payerID)
}

// rcList fetches the merchant receivable list with the given query string.
func rcList(t *testing.T, env *qeEnv, query string) []map[string]any {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/receivables"+query, nil)
	if status != http.StatusOK {
		t.Fatalf("list receivables = %d %v", status, out)
	}
	return payDataSlice(t, out)
}

// rcOnlyReceivable asserts exactly one receivable for the payer and returns it.
func rcOnlyReceivable(t *testing.T, env *qeEnv, payerID string) string {
	t.Helper()
	rows := rcList(t, env, "?payer_id="+payerID)
	if len(rows) != 1 {
		t.Fatalf("receivables for payer = %d, want 1: %v", len(rows), rows)
	}
	id, _ := rows[0]["id"].(string)
	if id == "" {
		t.Fatalf("receivable id missing: %v", rows[0])
	}
	return id
}

// TestReceivable_CreatedOnFinalize — finalize inserts one receivable per payer
// with a positive share (BPJS claim + private plan → 2), due = final day +
// payment_term_days, expires_on = service_date_to + 6 months only for BPJS;
// capitation-only accounts create none.
func TestReceivable_CreatedOnFinalize(t *testing.T) {
	env := payNewEnv(t, "rc.final")
	// fktp_network: BPJS covers only the claim-marked item; the rest falls
	// through to the private plan. A BPJS guarantor requires non-`none`.
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_network", "bpjs_network_parent": "FKTP Induk"})
	payMapData(t, status, out, http.StatusOK)
	_, admissionID := bilNewPersonAndVisit(t, env, "RCF", "Pasien Piutang Ganda")
	// BPJS claim on item A only; item B falls through to the private plan.
	bpjsListID, itemA := bilDefaultList(t, env, "RCFA", "10000.00")
	itemB := plCreateItem(t, env, "BIL-RCFB", "consultation")
	compB := plCreateComponent(t, env, "BILRCFB")
	if status, out := plSetPrice(t, env, bpjsListID, itemB, plToday(env, 0), map[string]string{compB: "10000.00"}); status != http.StatusOK {
		t.Fatalf("set price B = %d %v", status, out)
	}
	bpjsID := pcCreatePayer(t, env, "RCF-BPJS", "BPJS Klaim RCF", "bpjs")
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payers/"+bpjsID+"/item-coverage",
		map[string]any{"rules": []map[string]any{{
			"service_item_id": itemA, "coverage": "claim", "claim_type": "other",
		}}})
	if status != http.StatusOK {
		t.Fatalf("put item coverage = %d %v", status, out)
	}
	privID := pcCreatePayer(t, env, "RCF-PRV", "Asuransi RCF", "private_insurance")
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+privID+"/plans",
		map[string]any{"code": "RCF-PLAN", "name": "Plan RCF", "annual_limit": "20000.00"})
	planID, _ := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payer-plans/"+planID+"/rules",
		map[string]any{"rules": []map[string]any{{"care_type": "outpatient", "copay_percent": "0.00", "deductible_per_claim": "0.00"}}})
	if status != http.StatusOK {
		t.Fatalf("put plan rules = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/admissions/"+admissionID+"/guarantors",
		map[string]any{"guarantors": []map[string]any{{"payer_id": bpjsID}, {"payer_id": privID, "plan_id": planID}}})
	if status != http.StatusOK {
		t.Fatalf("put guarantors = %d %v", status, out)
	}
	for _, itemID := range []string{itemA, itemB} {
		status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
			map[string]any{"service_item_id": itemID, "quantity": "1"})
		payMapData(t, status, out, http.StatusCreated)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)

	rows := rcList(t, env, "")
	if len(rows) != 2 {
		t.Fatalf("receivables = %d, want 2: %v", len(rows), rows)
	}
	today := rcToday()
	byPayer := map[string]map[string]any{}
	for _, r := range rows {
		payer, _ := r["payer"].(map[string]any)
		byPayer[payer["id"].(string)] = r
	}
	bp := byPayer[bpjsID]
	pv := byPayer[privID]
	if bp == nil || pv == nil {
		t.Fatalf("missing receivables: %v", rows)
	}
	// BPJS: claim type, 6-month expiry, due = today + payment_term_days (30).
	if got, _ := bp["claim_types"].([]any); len(got) != 1 || got[0] != "other" {
		t.Fatalf("bpjs claim_types = %v", bp["claim_types"])
	}
	if bp["expires_on"] == "" || bp["expires_on"] == nil {
		t.Fatalf("bpjs expires_on must be set: %v", bp)
	}
	wantExp := today.AddDate(0, 6, 0).Format("2006-01-02")
	if bp["expires_on"] != wantExp {
		t.Fatalf("bpjs expires_on = %v, want %v", bp["expires_on"], wantExp)
	}
	if bp["amount"] != "10000.00" || bp["paid_amount"] != "0.00" ||
		bp["adjusted_amount"] != "0.00" || bp["balance"] != "10000.00" || bp["status"] != "open" {
		t.Fatalf("bpjs receivable = %v", bp)
	}
	wantDue := today.AddDate(0, 0, 30).Format("2006-01-02")
	if bp["due_date"] != wantDue {
		t.Fatalf("bpjs due_date = %v, want %v", bp["due_date"], wantDue)
	}
	if got, _ := bp["required_documents"].([]any); len(got) != 0 {
		t.Fatalf("bpjs required_documents = %v, want none for claim type other", bp["required_documents"])
	}
	// Private: no expiry, no per-receivable documents.
	if pv["expires_on"] != nil && pv["expires_on"] != "" {
		t.Fatalf("private expires_on = %v, want null", pv["expires_on"])
	}
	if pv["amount"] != "10000.00" || pv["status"] != "open" {
		t.Fatalf("private receivable = %v", pv)
	}
	if got, _ := pv["required_documents"].([]any); len(got) != 0 {
		t.Fatalf("private required_documents = %v", pv["required_documents"])
	}
	person, _ := bp["person"].(map[string]any)
	if person == nil || person["id"] == "" || person["full_name"] != "Pasien Piutang Ganda" {
		t.Fatalf("bpjs person = %v", bp["person"])
	}
	admission, _ := bp["admission"].(map[string]any)
	if admission == nil || admission["id"] != admissionID || admission["visit_no"] == "" {
		t.Fatalf("bpjs admission = %v", bp["admission"])
	}

	// Capitation only: zero payer share → no receivable. One BPJS payer per
	// merchant, so the capitation visit reuses the same payer with an item
	// that has NO claim rule (fktp_capitation default coverage applies).
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_capitation"})
	payMapData(t, status, out, http.StatusOK)
	_, capAdm := bilNewPersonAndVisit(t, env, "RCFC", "Pasien Kapitasi RCF")
	bilPutGuarantor(t, env, capAdm, bpjsID, "")
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+capAdm+"/charges",
		map[string]any{"service_item_id": itemB, "quantity": "1"})
	payMapData(t, status, out, http.StatusCreated)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+capAdm+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	// Still only the two claim receivables; nothing for the capitation visit.
	rows = rcList(t, env, "")
	if len(rows) != 2 {
		t.Fatalf("receivables after capitation finalize = %d, want 2: %v", len(rows), rows)
	}
	for _, r := range rows {
		admission, _ := r["admission"].(map[string]any)
		if admission["id"] == capAdm {
			t.Fatalf("capitation visit got a receivable: %v", r)
		}
	}
}

// TestReceivable_ReopenGuard — reopening deletes fresh open receivables and
// recreates them on the next finalize; receivables in process answer 409
// `billing has receivables in process`.
func TestReceivable_ReopenGuard(t *testing.T) {
	env := payNewEnv(t, "rc.reopen")
	admissionID, rcID := rcFinalizeClaim(t, env, "RCR", "other")

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/reopen",
		map[string]any{"reason": "salah hitung"})
	payMapData(t, status, out, http.StatusOK)
	rows := rcList(t, env, "")
	if len(rows) != 0 {
		t.Fatalf("receivables after reopen = %v, want none", rows)
	}

	// Finalize again → the receivable is recreated (the charge survived the
	// reopen, so no new charge is needed).
	_ = rcID
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	rows = rcList(t, env, "")
	if len(rows) != 1 {
		t.Fatalf("receivables after re-finalize = %v, want 1", rows)
	}

	// Mark the receivable in process → reopen is refused.
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.receivable SET status = 'in_batch' WHERE account_id = (SELECT id FROM operations.billing_account WHERE admission_id = $1)",
		admissionID); err != nil {
		t.Fatalf("mark in_batch: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/reopen",
		map[string]any{"reason": "coba lagi"})
	if status != http.StatusConflict || out["error"] != "billing has receivables in process" {
		t.Fatalf("reopen guarded = %d %v", status, out)
	}

	// An allocation/adjustment also blocks, even on a status-open receivable.
	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.receivable SET status = 'open' WHERE account_id = (SELECT id FROM operations.billing_account WHERE admission_id = $1)",
		admissionID); err != nil {
		t.Fatalf("reset open: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO operations.receivable_adjustment
		     (company_id, merchant_id, receivable_id, kind, amount, reason, created_by)
		 SELECT company_id, merchant_id, id, 'write_off', 1.00, 'uji', '11111111-1111-1111-1111-111111111111'
		 FROM operations.receivable
		 WHERE account_id = (SELECT id FROM operations.billing_account WHERE admission_id = $1)`,
		admissionID); err != nil {
		t.Fatalf("seed adjustment: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/reopen",
		map[string]any{"reason": "coba lagi"})
	if status != http.StatusConflict || out["error"] != "billing has receivables in process" {
		t.Fatalf("reopen with adjustment = %d %v", status, out)
	}
}

// TestReceivable_AgingBuckets — outstanding balances bucket per payer by
// as_of - due_date (≤ 0 days lands in d0_30); disputed receivables split off.
func TestReceivable_AgingBuckets(t *testing.T) {
	env := payNewEnv(t, "rc.aging")
	// One merchant = one BPJS payer; three receivables on three visits.
	payerID, itemID := rcClaimBPJS(t, env, "RCA", "other")
	rcNewClaimAdmission(t, env, payerID, itemID, "RCA")
	rcNewClaimAdmission(t, env, payerID, itemID, "RCB")
	rcNewClaimAdmission(t, env, payerID, itemID, "RCC")
	rows := rcList(t, env, "")
	if len(rows) != 3 {
		t.Fatalf("receivables = %d, want 3: %v", len(rows), rows)
	}
	byVisit := map[string]string{} // visit label → receivable id
	for _, r := range rows {
		admission, _ := r["admission"].(map[string]any)
		visitNo, _ := admission["visit_no"].(string)
		for _, label := range []string{"RCA", "RCB", "RCC"} {
			if strings.HasSuffix(visitNo, label+"-1") {
				byVisit[label], _ = r["id"].(string)
			}
		}
	}
	ra, rb, rc := byVisit["RCA"], byVisit["RCB"], byVisit["RCC"]
	if ra == "" || rb == "" || rc == "" {
		t.Fatalf("missing receivables: %v", byVisit)
	}

	ctx := context.Background()
	today := rcToday()
	setDue := func(id string, due time.Time) {
		if _, err := env.pool.Exec(ctx, "UPDATE operations.receivable SET due_date = $2 WHERE id = $1",
			id, due.Format("2006-01-02")); err != nil {
			t.Fatalf("set due: %v", err)
		}
	}
	setDue(ra, today)                     // 0 days → d0_30
	setDue(rb, today.AddDate(0, 0, -45))  // 45 days → d31_60
	setDue(rc, today.AddDate(0, 0, -100)) // disputed → its own column
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/receivables/"+rc,
		map[string]any{"disputed": true, "reason": "tagihan diperselisihkan"})
	payMapData(t, status, out, http.StatusOK)

	status, out = qeRequest(t, env, http.MethodGet,
		"/api/v1/merchants/"+env.merchantID+"/receivables/aging?as_of="+today.Format("2006-01-02"), nil)
	if status != http.StatusOK {
		t.Fatalf("aging = %d %v", status, out)
	}
	aging := payDataSlice(t, out)
	if len(aging) != 1 {
		t.Fatalf("aging rows = %d, want 1: %v", len(aging), aging)
	}
	row := aging[0]
	// ra in d0_30, rb in d31_60, rc in the disputed column.
	if row["d0_30"] != "10000.00" || row["d31_60"] != "10000.00" ||
		row["d61_90"] != "0.00" || row["over_90"] != "0.00" || row["disputed"] != "10000.00" {
		t.Fatalf("aging row = %v", row)
	}
}

// TestReceivable_Expiring — the expiry watchlist lists BPJS receivables with a
// balance whose expires_on ≤ today+days; days defaults to 30 and must be 1-180.
func TestReceivable_Expiring(t *testing.T) {
	env := payNewEnv(t, "rc.exp")
	payerID, itemID := rcClaimBPJS(t, env, "RCE", "other")
	rcNewClaimAdmission(t, env, payerID, itemID, "RCE1")
	rcNewClaimAdmission(t, env, payerID, itemID, "RCE2")
	rows := rcList(t, env, "")
	if len(rows) != 2 {
		t.Fatalf("receivables = %d, want 2: %v", len(rows), rows)
	}
	r1, _ := rows[0]["id"].(string)
	r2, _ := rows[1]["id"].(string)

	ctx := context.Background()
	today := rcToday()
	if _, err := env.pool.Exec(ctx, "UPDATE operations.receivable SET expires_on = $2 WHERE id = $1",
		r1, today.AddDate(0, 0, 10).Format("2006-01-02")); err != nil {
		t.Fatalf("set expires r1: %v", err)
	}
	if _, err := env.pool.Exec(ctx, "UPDATE operations.receivable SET expires_on = $2 WHERE id = $1",
		r2, today.AddDate(0, 0, 40).Format("2006-01-02")); err != nil {
		t.Fatalf("set expires r2: %v", err)
	}

	ids := func(query string) []string {
		status, out := qeRequest(t, env, http.MethodGet,
			"/api/v1/merchants/"+env.merchantID+"/receivables/expiring"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("expiring %s = %d %v", query, status, out)
		}
		var got []string
		for _, r := range payDataSlice(t, out) {
			got = append(got, r["id"].(string))
		}
		return got
	}
	if got := ids("?days=30"); len(got) != 1 || got[0] != r1 {
		t.Fatalf("expiring 30 = %v, want [%s]", got, r1)
	}
	if got := ids("?days=45"); len(got) != 2 {
		t.Fatalf("expiring 45 = %v, want both", got)
	}
	for _, bad := range []string{"?days=0", "?days=181", "?days=x"} {
		status, out := qeRequest(t, env, http.MethodGet,
			"/api/v1/merchants/"+env.merchantID+"/receivables/expiring"+bad, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("expiring %s = %d %v, want 400", bad, status, out)
		}
	}
	// Default = 30 days.
	status, out := qeRequest(t, env, http.MethodGet,
		"/api/v1/merchants/"+env.merchantID+"/receivables/expiring", nil)
	if status != http.StatusOK {
		t.Fatalf("expiring default = %d %v", status, out)
	}
	if got := payDataSlice(t, out); len(got) != 1 {
		t.Fatalf("expiring default = %v, want only r1", got)
	}
}

// TestReceivable_DisputeAndDocuments — the literal BPJS checklist gates the
// document keys; the dispute flag needs a reason and clearing it recomputes
// the status back to open with a NULL dispute_reason (chk_receivable_dispute).
func TestReceivable_DisputeAndDocuments(t *testing.T) {
	env := payNewEnv(t, "rc.disp")
	_, rcID := rcFinalizeClaim(t, env, "RCD", "anc")

	rows := rcList(t, env, "")
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	req, _ := json.Marshal(rows[0]["required_documents"])
	if string(req) != `["buku_kia"]` {
		t.Fatalf("required_documents = %s, want [buku_kia]", req)
	}

	// Documents: only checklist keys are accepted.
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/receivables/"+rcID+"/documents",
		map[string]any{"documents": map[string]any{"partograf": true}})
	if status != http.StatusBadRequest || out["error"] != "unknown document partograf" {
		t.Fatalf("unknown document = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/receivables/"+rcID+"/documents",
		map[string]any{"documents": map[string]any{"buku_kia": true}})
	data := payMapData(t, status, out, http.StatusOK)
	docs, _ := data["documents"].(map[string]any)
	if docs == nil || docs["buku_kia"] != true {
		t.Fatalf("documents = %v", data["documents"])
	}

	// Dispute: a reason is mandatory.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/receivables/"+rcID,
		map[string]any{"disputed": true})
	if status != http.StatusBadRequest || out["error"] != "dispute needs a reason" {
		t.Fatalf("dispute without reason = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/receivables/"+rcID,
		map[string]any{"disputed": true, "reason": "tagihan diperselisihkan"})
	data = payMapData(t, status, out, http.StatusOK)
	if data["status"] != "disputed" {
		t.Fatalf("disputed status = %v", data["status"])
	}

	// Clearing recomputes: back to open, dispute_reason NULL in the DB.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/receivables/"+rcID,
		map[string]any{"disputed": false})
	data = payMapData(t, status, out, http.StatusOK)
	if data["status"] != "open" {
		t.Fatalf("cleared status = %v, want open", data["status"])
	}
	ctx := context.Background()
	var reason *string
	if err := env.pool.QueryRow(ctx,
		"SELECT dispute_reason FROM operations.receivable WHERE id = $1", rcID).Scan(&reason); err != nil {
		t.Fatalf("read dispute_reason: %v", err)
	}
	if reason != nil {
		t.Fatalf("dispute_reason = %v, want NULL", reason)
	}
}

// TestReceivable_Adjustments — to_patient then write_off within the balance
// drive the status to partially_paid and settled; exceeding the balance is 409.
func TestReceivable_Adjustments(t *testing.T) {
	env := payNewEnv(t, "rc.adj")
	_, rcID := rcFinalizeClaim(t, env, "RCJ", "other")

	adjust := func(kind, amount string) (int, map[string]any) {
		return qeRequest(t, env, http.MethodPost, "/api/v1/receivables/"+rcID+"/adjustments",
			map[string]any{"kind": kind, "amount": amount, "reason": "selisih " + kind})
	}
	status, out := adjust("to_patient", "4000.00")
	data := payMapData(t, status, out, http.StatusCreated)
	if data["adjusted_amount"] != "4000.00" || data["balance"] != "6000.00" || data["status"] != "partially_paid" {
		t.Fatalf("after to_patient = %v", data)
	}
	status, out = adjust("write_off", "7000.00")
	if status != http.StatusConflict || out["error"] != "adjustment exceeds receivable balance" {
		t.Fatalf("over balance = %d %v", status, out)
	}
	status, out = adjust("write_off", "6000.00")
	data = payMapData(t, status, out, http.StatusCreated)
	// paid 0 + adjusted 10000 = amount → settled.
	if data["status"] != "settled" || data["balance"] != "0.00" {
		t.Fatalf("after write_off = %v", data)
	}
	status, out = adjust("write_off", "1.00")
	if status != http.StatusConflict {
		t.Fatalf("settled over = %d %v", status, out)
	}
}

// TestReceivable_RequiresPermission — every receivable endpoint answers 403
// without billing.receivable.manage.
func TestReceivable_RequiresPermission(t *testing.T) {
	env := payNewEnv(t, "rc.perm")
	_, rcID := rcFinalizeClaim(t, env, "RCP", "other")

	ctx := context.Background()
	seedMerchantStaff(t, ctx, env.pool, env.companyID, env.merchantID, "rc-perm-staff", "",
		"billing.invoice.view", "billing.invoice.create")
	staff := qeStaffToken(t, ctx, env.pool, env.router, env.companyID, "rc-perm-staff")

	paths := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/merchants/" + env.merchantID + "/receivables"},
		{http.MethodGet, "/api/v1/merchants/" + env.merchantID + "/receivables/aging"},
		{http.MethodGet, "/api/v1/merchants/" + env.merchantID + "/receivables/expiring"},
		{http.MethodPatch, "/api/v1/receivables/" + rcID},
		{http.MethodPatch, "/api/v1/receivables/" + rcID + "/documents"},
		{http.MethodPost, "/api/v1/receivables/" + rcID + "/adjustments"},
	}
	for _, p := range paths {
		status, out := qeRequestToken(t, env.router, p.method, p.path, staff,
			env.companyID, env.merchantID, map[string]any{})
		if status != http.StatusForbidden {
			t.Fatalf("%s %s = %d %v, want 403", p.method, p.path, status, out)
		}
	}
	// The owner token carries the permission via the template roles.
	if rows := rcList(t, env, "?payer_id="); len(rows) != 1 {
		t.Fatalf("owner list = %v, want the receivable", rows)
	}
}
