//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
)

// TestBillingFee_* covers the practitioner-fee wiring of the billing account
// API (spec 2026-10-07-tariff-d-practitioner-fees-tax §4.1–4.2): per-charge
// performers, the fee snapshot inserted at finalize (gross = the MEDIS
// portion of all parties), fee deletion on reopen and the capitation
// no-fee rule.

// bfeeTwoComponentList creates a default price list whose item is priced in
// two components (MEDIS 60000.00 + SARANA 40000.00 = 100000.00 per unit).
func bfeeTwoComponentList(t *testing.T, env *qeEnv, label string) (string, string) {
	t.Helper()
	itemID := plCreateItem(t, env, "BFEE-"+label, "consultation")
	medis := plCreateComponent(t, env, "MEDIS")
	sarana := plCreateComponent(t, env, "SARANA")
	listID := plCreateList(t, env, "BFEE-L-"+label, "", "", "")
	if status, out := plSetPrice(t, env, listID, itemID, plToday(env, 0), map[string]string{
		medis: "60000.00", sarana: "40000.00",
	}); status != http.StatusOK {
		t.Fatalf("set price = %d %v", status, out)
	}
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"default_price_list_id": listID})
	payMapData(t, status, out, http.StatusOK)
	return listID, itemID
}

// bfeeAccountID reads the billing account id of an admission.
func bfeeAccountID(t *testing.T, env *qeEnv, admissionID string) string {
	t.Helper()
	var id string
	if err := env.pool.QueryRow(context.Background(),
		"SELECT id::text FROM operations.billing_account WHERE admission_id = $1", admissionID).Scan(&id); err != nil {
		t.Fatalf("find account: %v", err)
	}
	return id
}

// bfeeFeeRow reads the single practitioner_fee row of an account
// (gross_amount, fee_amount as numeric strings, rule_id).
func bfeeFeeRow(t *testing.T, env *qeEnv, accountID string) (string, string, any) {
	t.Helper()
	var gross, fee string
	var ruleID any
	err := env.pool.QueryRow(context.Background(),
		"SELECT gross_amount::text, fee_amount::text, rule_id::text FROM operations.practitioner_fee WHERE account_id = $1",
		accountID).Scan(&gross, &fee, &ruleID)
	if err != nil {
		t.Fatalf("read fee: %v", err)
	}
	return gross, fee, ruleID
}

// bfeeFeeCount counts the practitioner_fee rows of an account.
func bfeeFeeCount(t *testing.T, env *qeEnv, accountID string) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM operations.practitioner_fee WHERE account_id = $1", accountID).Scan(&n); err != nil {
		t.Fatalf("count fees: %v", err)
	}
	return n
}

// TestBillingFee_PerformersAndSnapshot — a 40% MEDIS rule with a primary
// performer yields one fee row at finalize: gross = the MEDIS portion of all
// parties (60000.00), fee = 40% of it (24000.00), rule_id recorded; the
// performer set is echoed and shown per charge on GET billing.
func TestBillingFee_PerformersAndSnapshot(t *testing.T) {
	env := payNewEnv(t, "bfee.snap")
	_, admissionID := bilNewPersonAndVisit(t, env, "BFEE", "Pasien Jasa Medis")
	_, itemID := bfeeTwoComponentList(t, env, "SNAP")

	physicianID, _ := frCreatePhysician(t, env, "BFEE Dok", nil)
	rule := frCreateRule(t, env, map[string]any{
		"physician_id": physicianID, "basis": "percent_of_component",
		"component_code": "MEDIS", "percent": "40.00", "effective_from": plToday(env, 0),
	}, http.StatusCreated)
	ruleID, _ := rule["id"].(string)
	if ruleID == "" {
		t.Fatalf("rule id missing: %v", rule)
	}

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	chargeID, _ := ch["id"].(string)

	// Replace the performer set.
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/charges/"+chargeID+"/performers",
		map[string]any{"performers": []map[string]any{{"physician_id": physicianID, "role": "primary"}}})
	perfData := payMapData(t, status, out, http.StatusOK)
	perfRows, _ := perfData["performers"].([]any)
	if len(perfRows) != 1 {
		t.Fatalf("performers = %v", perfData)
	}
	perf, _ := perfRows[0].(map[string]any)
	if perf["physician_id"] != physicianID || perf["role"] != "primary" {
		t.Fatalf("performer = %v", perf)
	}

	// GET billing shows the performers per charge.
	got := bilAdmissionBilling(t, env, admissionID)
	gCharges, _ := got["charges"].([]any)
	gCh, _ := gCharges[0].(map[string]any)
	gPerf, _ := gCh["performers"].([]any)
	if len(gPerf) != 1 {
		t.Fatalf("GET performers = %v", gCh)
	}
	gPerf0, _ := gPerf[0].(map[string]any)
	if gPerf0["physician_id"] != physicianID || gPerf0["role"] != "primary" || gPerf0["name"] == "" {
		t.Fatalf("GET performer = %v", gPerf0)
	}

	// Finalize: gross = MEDIS of all parties = 60000.00; fee = 40% = 24000.00.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	accountID := bfeeAccountID(t, env, admissionID)
	gross, fee, feeRuleID := bfeeFeeRow(t, env, accountID)
	if gross != "60000.00" || fee != "24000.00" {
		t.Fatalf("fee row gross=%s fee=%s, want 60000.00/24000.00", gross, fee)
	}
	if feeRuleID != ruleID {
		t.Fatalf("rule_id = %v, want %s", feeRuleID, ruleID)
	}
	if n := bfeeFeeCount(t, env, accountID); n != 1 {
		t.Fatalf("fee rows = %d, want 1", n)
	}
}

// TestBillingFee_FixedPerItemQuantity — a fixed_per_item rule pays
// amount × quantity per performer: charge 2 × (MEDIS 60000 + SARANA 40000)
// gives gross 120000.00 and fee 15000.00 × 1 performer = 30000.00.
func TestBillingFee_FixedPerItemQuantity(t *testing.T) {
	env := payNewEnv(t, "bfee.fpiq")
	_, admissionID := bilNewPersonAndVisit(t, env, "BFEEQ", "Pasien Jasa Per Item")
	_, itemID := bfeeTwoComponentList(t, env, "FPIQ")

	physicianID, _ := frCreatePhysician(t, env, "BFEEQ Dok", nil)
	rule := frCreateRule(t, env, map[string]any{
		"physician_id": physicianID, "service_item_id": itemID,
		"basis": "fixed_per_item", "amount": "15000.00", "effective_from": plToday(env, 0),
	}, http.StatusCreated)
	ruleID, _ := rule["id"].(string)
	if ruleID == "" {
		t.Fatalf("rule id missing: %v", rule)
	}

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "2"})
	data := payMapData(t, status, out, http.StatusCreated)
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	chargeID, _ := ch["id"].(string)

	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/charges/"+chargeID+"/performers",
		map[string]any{"performers": []map[string]any{{"physician_id": physicianID, "role": "primary"}}})
	payMapData(t, status, out, http.StatusOK)

	// Finalize: gross = MEDIS 60000.00 × qty 2 = 120000.00; fee = 15000.00 × 2 = 30000.00.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	accountID := bfeeAccountID(t, env, admissionID)
	gross, fee, feeRuleID := bfeeFeeRow(t, env, accountID)
	if gross != "120000.00" || fee != "30000.00" {
		t.Fatalf("fee row gross=%s fee=%s, want 120000.00/30000.00", gross, fee)
	}
	if feeRuleID != ruleID {
		t.Fatalf("rule_id = %v, want %s", feeRuleID, ruleID)
	}
	if n := bfeeFeeCount(t, env, accountID); n != 1 {
		t.Fatalf("fee rows = %d, want 1", n)
	}
}

// TestBillingFee_RuleWindow — the rule's effective window is evaluated on the
// service date in the merchant timezone: expired → no fee, not yet effective →
// no fee, boundary days (from and to today) are inclusive.
func TestBillingFee_RuleWindow(t *testing.T) {
	newScenario := func(t *testing.T, label string) (*qeEnv, string, string) {
		t.Helper()
		env := payNewEnv(t, "bfee.win."+label)
		_, admissionID := bilNewPersonAndVisit(t, env, "BFEEW"+label, "Pasien Jendela "+label)
		_, itemID := bfeeTwoComponentList(t, env, "W-"+label)
		return env, admissionID, itemID
	}
	finalizeAndRead := func(t *testing.T, env *qeEnv, admissionID string) (string, string, any) {
		t.Helper()
		status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
		payMapData(t, status, out, http.StatusOK)
		return bfeeFeeRow(t, env, bfeeAccountID(t, env, admissionID))
	}

	// (a) Expired: effective_to in the past → no fee row rule match.
	env, admissionID, itemID := newScenario(t, "exp")
	physicianID, _ := frCreatePhysician(t, env, "BFEEW Exp", nil)
	rule := frCreateRule(t, env, map[string]any{
		"physician_id": physicianID, "basis": "percent_of_component",
		"component_code": "MEDIS", "percent": "40.00", "effective_from": plToday(env, -1),
	}, http.StatusCreated)
	ruleID, _ := rule["id"].(string)
	if _, err := env.pool.Exec(context.Background(),
		"UPDATE core.practitioner_fee_rule SET effective_to = $2 WHERE id = $1",
		ruleID, plToday(env, -1)); err != nil {
		t.Fatalf("set effective_to: %v", err)
	}
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/charges/"+ch["id"].(string)+"/performers",
		map[string]any{"performers": []map[string]any{{"physician_id": physicianID, "role": "primary"}}})
	payMapData(t, status, out, http.StatusOK)
	gross, fee, feeRuleID := finalizeAndRead(t, env, admissionID)
	if gross != "60000.00" || fee != "0.00" || feeRuleID != nil {
		t.Fatalf("expired: gross=%s fee=%s rule=%v, want 60000.00/0.00/NULL", gross, fee, feeRuleID)
	}

	// (b) Not yet effective: effective_from tomorrow → no fee.
	env, admissionID, itemID = newScenario(t, "fut")
	physicianID, _ = frCreatePhysician(t, env, "BFEEW Fut", nil)
	frCreateRule(t, env, map[string]any{
		"physician_id": physicianID, "basis": "percent_of_component",
		"component_code": "MEDIS", "percent": "40.00", "effective_from": plToday(env, 1),
	}, http.StatusCreated)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data = payMapData(t, status, out, http.StatusCreated)
	charges, _ = data["charges"].([]any)
	ch, _ = charges[0].(map[string]any)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/charges/"+ch["id"].(string)+"/performers",
		map[string]any{"performers": []map[string]any{{"physician_id": physicianID, "role": "primary"}}})
	payMapData(t, status, out, http.StatusOK)
	gross, fee, feeRuleID = finalizeAndRead(t, env, admissionID)
	if gross != "60000.00" || fee != "0.00" || feeRuleID != nil {
		t.Fatalf("future: gross=%s fee=%s rule=%v, want 60000.00/0.00/NULL", gross, fee, feeRuleID)
	}

	// (c) Inclusive boundary: from = to = today → fee applies.
	env, admissionID, itemID = newScenario(t, "edge")
	physicianID, _ = frCreatePhysician(t, env, "BFEEW Edge", nil)
	rule = frCreateRule(t, env, map[string]any{
		"physician_id": physicianID, "basis": "percent_of_component",
		"component_code": "MEDIS", "percent": "40.00", "effective_from": plToday(env, 0),
	}, http.StatusCreated)
	ruleID, _ = rule["id"].(string)
	if _, err := env.pool.Exec(context.Background(),
		"UPDATE core.practitioner_fee_rule SET effective_to = $2 WHERE id = $1",
		ruleID, plToday(env, 0)); err != nil {
		t.Fatalf("set effective_to: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data = payMapData(t, status, out, http.StatusCreated)
	charges, _ = data["charges"].([]any)
	ch, _ = charges[0].(map[string]any)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/charges/"+ch["id"].(string)+"/performers",
		map[string]any{"performers": []map[string]any{{"physician_id": physicianID, "role": "primary"}}})
	payMapData(t, status, out, http.StatusOK)
	gross, fee, feeRuleID = finalizeAndRead(t, env, admissionID)
	// 40% × MEDIS 60000.00, qty 1 = 24000.00.
	if gross != "60000.00" || fee != "24000.00" || feeRuleID != ruleID {
		t.Fatalf("boundary: gross=%s fee=%s rule=%v, want 60000.00/24000.00/%s", gross, fee, feeRuleID, ruleID)
	}
}

// TestBillingFee_ReopenDeletesOpenFees — reopening the account deletes its
// (still open) fee rows.
func TestBillingFee_ReopenDeletesOpenFees(t *testing.T) {
	env := payNewEnv(t, "bfee.reopen")
	_, admissionID := bilNewPersonAndVisit(t, env, "BFEER", "Pasien Jasa Buka Ulang")
	_, itemID := bfeeTwoComponentList(t, env, "REOPEN")
	physicianID, _ := frCreatePhysician(t, env, "BFEER Dok", nil)

	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	chargeID, _ := ch["id"].(string)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/charges/"+chargeID+"/performers",
		map[string]any{"performers": []map[string]any{{"physician_id": physicianID, "role": "primary"}}})
	payMapData(t, status, out, http.StatusOK)

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	accountID := bfeeAccountID(t, env, admissionID)
	if n := bfeeFeeCount(t, env, accountID); n != 1 {
		t.Fatalf("fee rows after finalize = %d, want 1", n)
	}

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/reopen",
		map[string]any{"reason": "salah pelaksana"})
	payMapData(t, status, out, http.StatusOK)
	if n := bfeeFeeCount(t, env, accountID); n != 0 {
		t.Fatalf("fee rows after reopen = %d, want 0", n)
	}
}

// TestBillingFee_CapitationNoFee — a charge fully borne by a capitation
// payer produces no fee row even with a matching rule and performer.
func TestBillingFee_CapitationNoFee(t *testing.T) {
	env := payNewEnv(t, "bfee.cap")
	_, admissionID := bilNewPersonAndVisit(t, env, "BFEEC", "Pasien Jasa Kapitasi")
	_, itemID := bfeeTwoComponentList(t, env, "CAP")
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_capitation"})
	payMapData(t, status, out, http.StatusOK)

	bpjsListID := plCreateList(t, env, "BFEECAPBPJS", "", "", "")
	compID := plCreateComponent(t, env, "BILCAPC2")
	if status, out := plSetPrice(t, env, bpjsListID, itemID, plToday(env, 0), map[string]string{compID: "8000.00"}); status != http.StatusOK {
		t.Fatalf("set bpjs price = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/payers",
		map[string]any{"code": "BFEECAP-BPJS", "name": "BPJS Jasa Kapitasi", "payer_type": "bpjs", "price_list_id": bpjsListID})
	bpjsID, _ := payMapData(t, status, out, http.StatusCreated)["id"].(string)
	bilPutGuarantor(t, env, admissionID, bpjsID, "")

	physicianID, _ := frCreatePhysician(t, env, "BFEEC Dok", nil)
	frCreateRule(t, env, map[string]any{
		"physician_id": physicianID, "basis": "percent_of_component",
		"component_code": "MEDIS", "percent": "40.00", "effective_from": plToday(env, 0),
	}, http.StatusCreated)

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/charges",
		map[string]any{"service_item_id": itemID, "quantity": "1"})
	data := payMapData(t, status, out, http.StatusCreated)
	charges, _ := data["charges"].([]any)
	ch, _ := charges[0].(map[string]any)
	chargeID, _ := ch["id"].(string)
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/charges/"+chargeID+"/performers",
		map[string]any{"performers": []map[string]any{{"physician_id": physicianID, "role": "primary"}}})
	payMapData(t, status, out, http.StatusOK)

	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/admissions/"+admissionID+"/billing/finalize", nil)
	payMapData(t, status, out, http.StatusOK)
	accountID := bfeeAccountID(t, env, admissionID)
	if n := bfeeFeeCount(t, env, accountID); n != 0 {
		t.Fatalf("capitation fee rows = %d, want 0", n)
	}
}
