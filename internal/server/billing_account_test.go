//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
)

// TestBilling_* covers the visit billing account API (spec
// 2026-10-07-tariff-b1-billing-core §5): charge rows with live recomputation,
// price_missing rejection, BPJS capitation zero-billing with shadow amounts,
// the excluded-needs-consent finalize guard, guarantee letters, split
// freezing, reopen permissions and the cancel-with-active-charges guard.

// bilNewPersonAndVisit seeds a person plus one active visit and returns both ids.
func bilNewPersonAndVisit(t *testing.T, env *qeEnv, label, name string) (string, string) {
	t.Helper()
	ctx := context.Background()
	personID := pcNewPerson(t, env, name)
	mergeSeedAdmission(t, ctx, env.pool, env.companyID, env.merchantID, personID, label)
	var admissionID string
	if err := env.pool.QueryRow(ctx,
		"SELECT id::text FROM operations.admission WHERE merchant_id = $1 AND person_id = $2 ORDER BY created_at DESC LIMIT 1",
		env.merchantID, personID).Scan(&admissionID); err != nil {
		t.Fatalf("find admission: %v", err)
	}
	return personID, admissionID
}

// bilDefaultList creates a price list with one component-priced item and makes
// it the merchant's default (Umum) list. Returns (listID, itemID).
func bilDefaultList(t *testing.T, env *qeEnv, label, amount string) (string, string) {
	t.Helper()
	itemID := plCreateItem(t, env, "BIL-"+label, "consultation")
	compID := plCreateComponent(t, env, "BILC-"+label)
	listID := plCreateList(t, env, "BILL-"+label, "", "", "")
	if status, out := plSetPrice(t, env, listID, itemID, plToday(env, 0), map[string]string{compID: amount}); status != http.StatusOK {
		t.Fatalf("set price = %d %v", status, out)
	}
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"default_price_list_id": listID})
	payMapData(t, status, out, http.StatusOK)
	return listID, itemID
}

// bilAdmissionBilling fetches GET /admissions/:id/billing data.
func bilAdmissionBilling(t *testing.T, env *qeEnv, admissionID string) map[string]any {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodGet, "/api/v1/admissions/"+admissionID+"/billing", nil)
	return payMapData(t, status, out, http.StatusOK)
}

// bilPutGuarantor replaces the visit's guarantor set with a single payer
// (optionally pointing at a plan).
func bilPutGuarantor(t *testing.T, env *qeEnv, admissionID, payerID, planID string) {
	t.Helper()
	g := map[string]any{"payer_id": payerID}
	if planID != "" {
		g["plan_id"] = planID
	}
	status, out := qeRequest(t, env, http.MethodPut, "/api/v1/admissions/"+admissionID+"/guarantors",
		map[string]any{"guarantors": []map[string]any{g}})
	if status != http.StatusOK {
		t.Fatalf("put guarantor = %d %v", status, out)
	}
}

// TestBilling_ChargeAndSplitUmum — a charge without guarantors is billed to
// the patient at the default list price, qty as a numeric string, split
// uncovered, and GET shows the same recomputed state.
func TestBilling_ChargeAndSplitUmum(t *testing.T) {
	env := payNewEnv(t, "bil.umum")
	_, admissionID := bilNewPersonAndVisit(t, env, "BILU", "Pasien Umum Tagihan")
	_, itemID := bilDefaultList(t, env, "UMUM", "10000.00")

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "2", "note": "konsultasi"})
	data := payMapData(t, status, out, http.StatusCreated)

	acct, _ := data["account"].(map[string]any)
	if acct == nil || acct["status"] != "open" || acct["patient_total"] != nil {
		t.Fatalf("account = %v", acct)
	}
	totals, _ := data["totals"].(map[string]any)
	if totals["patient"] != "20000.00" || totals["payer"] != "0.00" {
		t.Fatalf("totals = %v", totals)
	}
	charges, _ := data["charges"].([]any)
	if len(charges) != 1 {
		t.Fatalf("charges = %v", data)
	}
	ch, _ := charges[0].(map[string]any)
	if ch["quantity"] != "2.000" || ch["status"] != "active" {
		t.Fatalf("charge = %v", ch)
	}
	item, _ := ch["service_item"].(map[string]any)
	if item == nil || item["id"] != itemID || item["item_type"] != "consultation" {
		t.Fatalf("service_item = %v", item)
	}
	splits, _ := ch["splits"].([]any)
	if len(splits) != 1 {
		t.Fatalf("splits = %v", ch)
	}
	sp, _ := splits[0].(map[string]any)
	if sp["party"] != "patient" || sp["payer"] != nil || sp["coverage"] != "uncovered" ||
		sp["unit_amount"] != "10000.00" || sp["gross_amount"] != "20000.00" || sp["amount"] != "20000.00" {
		t.Fatalf("split = %v", sp)
	}

	// GET shows the same fresh state.
	got := bilAdmissionBilling(t, env, admissionID)
	gTotals, _ := got["totals"].(map[string]any)
	if gTotals["patient"] != "20000.00" {
		t.Fatalf("GET totals = %v", got)
	}
}

// TestBilling_PriceMissingRejected — without a resolvable price the charge is
// rejected with 409 price_missing and nothing is stored (rollback).
func TestBilling_PriceMissingRejected(t *testing.T) {
	env := payNewEnv(t, "bil.pmiss")
	_, admissionID := bilNewPersonAndVisit(t, env, "BILP", "Pasien Tanpa Harga")
	itemID := plCreateItem(t, env, "BIL-PMISS", "consultation")

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	if status != http.StatusConflict {
		t.Fatalf("status = %d %v", status, out)
	}
	if out["error"] != "price_missing" || out["service_item_id"] != itemID {
		t.Fatalf("body = %v", out)
	}
	got := bilAdmissionBilling(t, env, admissionID)
	if got["account"] != nil {
		t.Fatalf("account must be null after rollback, got %v", got["account"])
	}
	charges, _ := got["charges"].([]any)
	if len(charges) != 0 {
		t.Fatalf("charges must be empty, got %v", charges)
	}
}

// TestBilling_BPJSCapitationZero — fktp_capitation profile: BPJS bears 0 with
// a shadow amount at the BPJS list price, the patient pays 0.
func TestBilling_BPJSCapitationZero(t *testing.T) {
	env := payNewEnv(t, "bil.cap")
	personID, admissionID := bilNewPersonAndVisit(t, env, "BILC", "Pasien Kapitasi")
	_, itemID := bilDefaultList(t, env, "CAP", "10000.00")
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_capitation"})
	payMapData(t, status, out, http.StatusOK)

	bpjsListID := plCreateList(t, env, "BILCAPBPJS", "", "", "")
	compID := plCreateComponent(t, env, "BILCAPC")
	if status, out := plSetPrice(t, env, bpjsListID, itemID, plToday(env, 0), map[string]string{compID: "8000.00"}); status != http.StatusOK {
		t.Fatalf("set bpjs price = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/payers",
		map[string]any{"code": "BILCAP-BPJS", "name": "BPJS Kapitasi", "payer_type": "bpjs", "price_list_id": bpjsListID})
	bpjsID, _ := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	bilPutGuarantor(t, env, admissionID, bpjsID, "")

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	totals, _ := data["totals"].(map[string]any)
	if totals["patient"] != "0.00" || totals["payer"] != "0.00" {
		t.Fatalf("totals = %v, want patient 0 and payer 0", totals)
	}
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	splits, _ := ch["splits"].([]any)
	if len(splits) != 1 {
		t.Fatalf("splits = %v", ch)
	}
	sp, _ := splits[0].(map[string]any)
	if sp["party"] != "payer" || sp["coverage"] != "capitation" ||
		sp["amount"] != "0.00" || sp["shadow_amount"] != "8000.00" || sp["unit_amount"] != "8000.00" {
		t.Fatalf("split = %v", sp)
	}
	payer, _ := sp["payer"].(map[string]any)
	if payer == nil || payer["id"] != bpjsID {
		t.Fatalf("split payer = %v", payer)
	}
	if personID == "" {
		t.Fatal("person missing")
	}
}

// TestBilling_ExcludedNeedsConsent — fktp_network profile: BPJS excludes the
// item, the patient pays the Umum price, finalize is refused until a consent
// is recorded, then it succeeds.
func TestBilling_ExcludedNeedsConsent(t *testing.T) {
	env := payNewEnv(t, "bil.exc")
	_, admissionID := bilNewPersonAndVisit(t, env, "BILE", "Pasien Excluded")
	_, itemID := bilDefaultList(t, env, "EXC", "10000.00")
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_network", "bpjs_network_parent": "FKTP Induk"})
	payMapData(t, status, out, http.StatusOK)
	bpjsID := pcCreatePayer(t, env, "BILE-BPJS", "BPJS Excluded", "bpjs")
	bilPutGuarantor(t, env, admissionID, bpjsID, "")

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	splits, _ := ch["splits"].([]any)
	sp, _ := splits[0].(map[string]any)
	if sp["coverage"] != "excluded" || sp["unit_amount"] != "10000.00" || sp["amount"] != "10000.00" {
		t.Fatalf("excluded split = %v", sp)
	}
	chargeID, _ := ch["id"].(string)

	// Finalize without consent → 409 consent_required.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	if status != http.StatusConflict {
		t.Fatalf("finalize = %d %v", status, out)
	}
	if out["error"] != "consent_required" {
		t.Fatalf("body = %v", out)
	}
	ids, _ := out["charge_ids"].([]any)
	if len(ids) != 1 || ids[0] != chargeID {
		t.Fatalf("charge_ids = %v", out)
	}

	// Record the consent (blanket, no charge_id), then finalize.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/consents",
		map[string]any{"consented_by_name": "Keluarga Pasien", "relation": "family"})
	payMapData(t, status, out, http.StatusCreated)

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	data = payMapData(t, status, out, http.StatusOK)
	acct, _ := data["account"].(map[string]any)
	if acct["status"] != "finalized" || acct["patient_total"] != "10000.00" || acct["finalized_at"] == nil {
		t.Fatalf("finalized account = %v", acct)
	}
	if needs, _ := data["needs_consent"].([]any); len(needs) != 0 {
		t.Fatalf("needs_consent = %v", data)
	}
}

// TestBilling_ExcludedPartialPrivate — fktp_network profile: BPJS excludes
// the item, a private plan's annual limit covers part of it; the remainder
// stays with the patient (coverage "excluded") but NO consent is required.
func TestBilling_ExcludedPartialPrivate(t *testing.T) {
	env := payNewEnv(t, "bil.excp")
	_, admissionID := bilNewPersonAndVisit(t, env, "BIXP", "Pasien Excluded Parsial")
	_, itemID := bilDefaultList(t, env, "EXCP", "10000.00")
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_network", "bpjs_network_parent": "FKTP Induk"})
	payMapData(t, status, out, http.StatusOK)
	bpjsID := pcCreatePayer(t, env, "BIXP-BPJS", "BPJS Excluded Parsial", "bpjs")
	bilPutGuarantor(t, env, admissionID, bpjsID, "")
	privID := pcCreatePayer(t, env, "BIXP-PRV", "Asuransi Parsial", "private_insurance")
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+privID+"/plans",
		map[string]any{"code": "BIXP-PLAN", "name": "Plan Parsial", "annual_limit": "6000.00"})
	planID, _ := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payer-plans/"+planID+"/rules",
		map[string]any{"rules": []map[string]any{{
			"care_type": "outpatient", "copay_percent": "0.00", "deductible_per_claim": "0.00",
		}}})
	if status != http.StatusOK {
		t.Fatalf("put rules = %d %v", status, out)
	}
	// bilPutGuarantor replaces the whole set, so both guarantors go in one PUT.
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/admissions/"+admissionID+"/guarantors",
		map[string]any{"guarantors": []map[string]any{
			{"payer_id": bpjsID},
			{"payer_id": privID, "plan_id": planID},
		}})
	if status != http.StatusOK {
		t.Fatalf("put guarantors = %d %v", status, out)
	}

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	totals, _ := data["totals"].(map[string]any)
	if totals["payer"] != "6000.00" || totals["patient"] != "4000.00" {
		t.Fatalf("partial totals = %v", totals)
	}
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	splits, _ := ch["splits"].([]any)
	if len(splits) != 2 {
		t.Fatalf("splits = %v", ch)
	}
	sp0, _ := splits[0].(map[string]any)
	sp1, _ := splits[1].(map[string]any)
	if sp0["party"] != "payer" || sp0["coverage"] != "plan" || sp0["amount"] != "6000.00" || sp0["note"] != "annual limit" {
		t.Fatalf("payer split = %v", sp0)
	}
	if sp1["party"] != "patient" || sp1["coverage"] != "excluded" || sp1["amount"] != "4000.00" {
		t.Fatalf("patient split = %v", sp1)
	}
	if needs, _ := data["needs_consent"].([]any); len(needs) != 0 {
		t.Fatalf("needs_consent = %v", data)
	}

	// Finalize without any consent — allowed: the exclusion is partially covered.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	data = payMapData(t, status, out, http.StatusOK)
	acct, _ := data["account"].(map[string]any)
	if acct["status"] != "finalized" || acct["patient_total"] != "4000.00" || acct["payer_total"] != "6000.00" {
		t.Fatalf("finalized account = %v", acct)
	}
}

// TestBilling_GLRequired — a requires_gl plan that covers the visit needs a
// valid guarantee letter before finalize.
func TestBilling_GLRequired(t *testing.T) {
	env := payNewEnv(t, "bil.gl")
	_, admissionID := bilNewPersonAndVisit(t, env, "BILG", "Pasien Surat Jaminan")
	_, itemID := bilDefaultList(t, env, "GL", "10000.00")
	privID := pcCreatePayer(t, env, "BILG-PRV", "Asuransi GL", "private_insurance")
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+privID+"/plans",
		map[string]any{"code": "BILG-PLAN", "name": "Plan GL", "requires_gl": true})
	planID, _ := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payer-plans/"+planID+"/rules",
		map[string]any{"rules": []map[string]any{{
			"care_type": "outpatient", "copay_percent": "0.00", "deductible_per_claim": "0.00",
		}}})
	if status != http.StatusOK {
		t.Fatalf("put rules = %d %v", status, out)
	}
	bilPutGuarantor(t, env, admissionID, privID, planID)

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	payMapData(t, status, out, http.StatusCreated)

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	if status != http.StatusConflict || out["error"] != "guarantee letter required for Asuransi GL" {
		t.Fatalf("finalize without GL = %d %v", status, out)
	}

	// Add a valid letter → finalize succeeds, payer bears the full amount.
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/admissions/"+admissionID+"/guarantee-letters",
		map[string]any{"letters": []map[string]any{{
			"payer_id": privID, "gl_number": "GL-BILG", "amount_limit": "1000000.00", "valid_from": "2026-01-01",
		}}})
	if status != http.StatusOK {
		t.Fatalf("put letters = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	data := payMapData(t, status, out, http.StatusOK)
	totals, _ := data["totals"].(map[string]any)
	if totals["payer"] != "10000.00" || totals["patient"] != "0.00" {
		t.Fatalf("totals = %v", totals)
	}
}

// TestBilling_FinalizeFreezes — after finalize a price change does not alter
// the stored splits, and new charges are refused.
func TestBilling_FinalizeFreezes(t *testing.T) {
	env := payNewEnv(t, "bil.frz")
	_, admissionID := bilNewPersonAndVisit(t, env, "BILF", "Pasien Beku")
	listID, itemID := bilDefaultList(t, env, "FRZ", "10000.00")

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	payMapData(t, status, out, http.StatusCreated)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)

	// Change the list price (a new future version).
	compID := plCreateComponent(t, env, "BILFRZC2")
	if status, out := plSetPrice(t, env, listID, itemID, plToday(env, 1), map[string]string{compID: "99999.00"}); status != http.StatusOK {
		t.Fatalf("new price version = %d %v", status, out)
	}

	got := bilAdmissionBilling(t, env, admissionID)
	totals, _ := got["totals"].(map[string]any)
	if totals["patient"] != "10000.00" {
		t.Fatalf("frozen totals = %v, want 10000.00", totals)
	}
	charges, _ := got["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	splits, _ := ch["splits"].([]any)
	sp, _ := splits[0].(map[string]any)
	if sp["unit_amount"] != "10000.00" {
		t.Fatalf("frozen split = %v", sp)
	}

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	if status != http.StatusConflict || out["error"] != "billing is finalized" {
		t.Fatalf("charge after final = %d %v", status, out)
	}
}

// TestBilling_Reopen — reopen needs billing.account.reopen (403 without it);
// with it the account returns to open with nulled totals and a bumped counter.
func TestBilling_Reopen(t *testing.T) {
	env := payNewEnv(t, "bil.reopen")
	_, admissionID := bilNewPersonAndVisit(t, env, "BILR", "Pasien Buka Ulang")
	_, itemID := bilDefaultList(t, env, "REOPEN", "10000.00")

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	payMapData(t, status, out, http.StatusCreated)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)

	// Staff with only view+create → 403.
	ctx := context.Background()
	seedMerchantStaff(t, ctx, env.pool, env.companyID, env.merchantID, "bil-reopen-staff", "",
		"billing.invoice.view", "billing.invoice.create")
	staff := qeStaffToken(t, ctx, env.pool, env.router, env.companyID, "bil-reopen-staff")
	status, out = qeRequestToken(t, env.router, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/reopen",
		staff, env.companyID, env.merchantID, map[string]any{"reason": "salah hitung"})
	if status != http.StatusForbidden {
		t.Fatalf("reopen without permission = %d %v", status, out)
	}

	// Owner (has billing.account.reopen) → open, totals nulled, counter +1.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/reopen",
		map[string]any{"reason": "salah hitung"})
	data := payMapData(t, status, out, http.StatusOK)
	acct, _ := data["account"].(map[string]any)
	if acct["status"] != "open" || acct["patient_total"] != nil || acct["payer_total"] != nil ||
		acct["finalized_at"] != nil || acct["reopened_count"] != float64(1) {
		t.Fatalf("reopened account = %v", acct)
	}
}

// TestBilling_CancelGuard — a visit with active charges cannot be cancelled;
// after voiding the charge it can.
func TestBilling_CancelGuard(t *testing.T) {
	env := payNewEnv(t, "bil.cancel")
	_, admissionID := bilNewPersonAndVisit(t, env, "BILX", "Pasien Batal")
	_, itemID := bilDefaultList(t, env, "CANCEL", "10000.00")

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	chargeID, _ := ch["id"].(string)

	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/admissions/"+admissionID,
		map[string]any{"status": "cancelled"})
	if status != http.StatusConflict || out["error"] != "admission has active charges" {
		t.Fatalf("cancel guard = %d %v", status, out)
	}

	// Void with an empty reason → 400; with a reason → 200 and the charge is
	// no longer active.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/charges/"+chargeID+"/void", map[string]any{})
	if status != http.StatusBadRequest {
		t.Fatalf("void without reason = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/charges/"+chargeID+"/void",
		map[string]any{"reason": "salah input"})
	data = payMapData(t, status, out, http.StatusOK)
	charges, _ = data["charges"].([]any)
	ch, _ = charges[0].(map[string]any)
	if ch["status"] != "void" {
		t.Fatalf("voided charge = %v", ch)
	}
	totals, _ := data["totals"].(map[string]any)
	if totals["patient"] != "0.00" {
		t.Fatalf("totals after void = %v", totals)
	}

	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/admissions/"+admissionID,
		map[string]any{"status": "cancelled"})
	payMapData(t, status, out, http.StatusOK)
}

// TestBilling_AnnualUsage — a first finalized account consuming 80% of the
// plan's annual limit caps the second visit's coverage at the remainder.
func TestBilling_AnnualUsage(t *testing.T) {
	env := payNewEnv(t, "bil.annual")
	personID, admissionA := bilNewPersonAndVisit(t, env, "BILA", "Pasien Limit Tahunan")
	_, itemID := bilDefaultList(t, env, "ANNUAL", "50000.00")
	privID := pcCreatePayer(t, env, "BILA-PRV", "Asuransi Limit", "private_insurance")
	// limit 62500.00, first visit uses 50000.00 = 80%; remainder 12500.00.
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+privID+"/plans",
		map[string]any{"code": "BILA-PLAN", "name": "Plan Limit", "annual_limit": "62500.00"})
	planID, _ := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payer-plans/"+planID+"/rules",
		map[string]any{"rules": []map[string]any{{
			"care_type": "outpatient", "copay_percent": "0.00", "deductible_per_claim": "0.00",
		}}})
	if status != http.StatusOK {
		t.Fatalf("put rules = %d %v", status, out)
	}
	bilPutGuarantor(t, env, admissionA, privID, planID)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionA+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	payMapData(t, status, out, http.StatusCreated)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionA+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)

	// Second visit, same person and plan: only the annual remainder is borne.
	mergeSeedAdmission(t, context.Background(), env.pool, env.companyID, env.merchantID, personID, "BILB")
	var admissionB string
	if err := env.pool.QueryRow(context.Background(),
		"SELECT id::text FROM operations.admission WHERE merchant_id = $1 AND person_id = $2 ORDER BY created_at DESC LIMIT 1",
		env.merchantID, personID).Scan(&admissionB); err != nil {
		t.Fatalf("find admission B: %v", err)
	}
	bilPutGuarantor(t, env, admissionB, privID, planID)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionB+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	totals, _ := data["totals"].(map[string]any)
	if totals["payer"] != "12500.00" || totals["patient"] != "37500.00" {
		t.Fatalf("annual-capped totals = %v", totals)
	}
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	splits, _ := ch["splits"].([]any)
	if len(splits) != 2 {
		t.Fatalf("splits = %v", ch)
	}
	sp0, _ := splits[0].(map[string]any)
	if sp0["party"] != "payer" || sp0["coverage"] != "plan" || sp0["amount"] != "12500.00" || sp0["note"] != "annual limit" {
		t.Fatalf("payer split = %v", sp0)
	}
}

// TestBilling_AnnualUsageTwoPlans — annual usage is per plan: a second plan of
// the SAME payer keeps its own full annual limit even after the first plan of
// that payer has been consumed by the same person.
func TestBilling_AnnualUsageTwoPlans(t *testing.T) {
	env := payNewEnv(t, "bil.annual2")
	personID, admissionA := bilNewPersonAndVisit(t, env, "BIL2A", "Pasien Dua Plan")
	_, itemID := bilDefaultList(t, env, "ANNUAL2", "50000.00")
	privID := pcCreatePayer(t, env, "BIL2-PRV", "Asuransi Dua Plan", "private_insurance")
	putRules := func(planID string) {
		t.Helper()
		status, out := qeRequest(t, env, http.MethodPut, "/api/v1/payer-plans/"+planID+"/rules",
			map[string]any{"rules": []map[string]any{{
				"care_type": "outpatient", "copay_percent": "0.00", "deductible_per_claim": "0.00",
			}}})
		if status != http.StatusOK {
			t.Fatalf("put rules = %d %v", status, out)
		}
	}
	// Two plans of the same payer, each with a 62500.00 annual limit.
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+privID+"/plans",
		map[string]any{"code": "BIL2-PLAN-A", "name": "Plan A", "annual_limit": "62500.00"})
	planA, _ := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+privID+"/plans",
		map[string]any{"code": "BIL2-PLAN-B", "name": "Plan B", "annual_limit": "62500.00"})
	planB, _ := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	putRules(planA)
	putRules(planB)

	// Visit A on plan A consumes 50000.00 of plan A's limit and finalizes.
	bilPutGuarantor(t, env, admissionA, privID, planA)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionA+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	payMapData(t, status, out, http.StatusCreated)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionA+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)

	// Visit B on plan B of the same payer must NOT be capped by plan A's
	// usage: the payer bears the full 50000.00.
	mergeSeedAdmission(t, context.Background(), env.pool, env.companyID, env.merchantID, personID, "BIL2B")
	var admissionB string
	if err := env.pool.QueryRow(context.Background(),
		"SELECT id::text FROM operations.admission WHERE merchant_id = $1 AND person_id = $2 ORDER BY created_at DESC LIMIT 1",
		env.merchantID, personID).Scan(&admissionB); err != nil {
		t.Fatalf("find admission B: %v", err)
	}
	bilPutGuarantor(t, env, admissionB, privID, planB)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionB+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	totals, _ := data["totals"].(map[string]any)
	if totals["payer"] != "50000.00" || totals["patient"] != "0.00" {
		t.Fatalf("plan B totals = %v, want payer 50000.00 patient 0", totals)
	}
}
